package live_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/live"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
	"github.com/sashasagebd/pokewordle-api/internal/versusclient"
)

// The failures Versus has to get through: a deploy (one instance stopping,
// its clients moving to the next), the database dropping the hub's LISTEN
// connection, deadlines passing with nobody connected, and one player's tabs
// on two instances. Each is staged for real, against two instances on the
// test database.

// A tunnel stands in for Cloudflare's: every client goes through it, and it
// sends each new connection (sockets included) to whichever instance is live.
type tunnel struct {
	url    string
	target atomic.Pointer[url.URL]
}

func newTunnel(t *testing.T, to *instance) *tunnel {
	t.Helper()
	tn := &tunnel{}
	tn.switchTo(to)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httputil.NewSingleHostReverseProxy(tn.target.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	tn.url = srv.URL
	return tn
}

func (tn *tunnel) switchTo(in *instance) {
	u, _ := url.Parse(in.url)
	tn.target.Store(u)
}

func within(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// players makes a room at base with two players in it, both connected, and
// returns its code and their sockets.
func players(t *testing.T, ctx context.Context, base string) (code string, alice, bob *versusclient.Socket, aliceID, bobID string) {
	t.Helper()
	ac, bc := versusclient.New(base), versusclient.New(base)
	must(t, ac.NewPlayer(ctx))
	must(t, bc.NewPlayer(ctx))
	code, err := ac.CreateRoom(ctx, []int{1})
	must(t, err)
	must(t, bc.Join(ctx, code))
	alice, bob = ac.Open(ctx, code), bc.Open(ctx, code)
	t.Cleanup(alice.Close)
	t.Cleanup(bob.Close)
	av, err := alice.AwaitRoom(ctx, func(v live.View) bool { return len(v.Members) == 2 })
	must(t, err)
	bv, err := bob.AwaitRoom(ctx, func(live.View) bool { return true })
	must(t, err)
	aliceID, bobID = av.You, bv.You
	_, err = alice.AwaitRoom(ctx, func(v live.View) bool { return member(v, aliceID).Connected && member(v, bobID).Connected })
	must(t, err)
	return code, alice, bob, aliceID, bobID
}

// race starts a Wordle round in the room and skips its countdown.
func race(t *testing.T, ctx context.Context, in *instance, code string, alice, bob *versusclient.Socket, bobID string) {
	t.Helper()
	must(t, bob.Send(ctx, map[string]any{"type": "ready", "ready": true}))
	_, err := alice.AwaitRoom(ctx, func(v live.View) bool { return member(v, bobID).Ready })
	must(t, err)
	must(t, alice.Send(ctx, map[string]any{"type": "start"}))
	for _, s := range []*versusclient.Socket{alice, bob} {
		_, err := s.AwaitRoom(ctx, func(v live.View) bool { return v.Status == rooms.Playing })
		must(t, err)
	}
	in.skipCountdown(t, code)
}

// guess plays a Wordle guess and waits for its answer.
func guess(t *testing.T, ctx context.Context, s *versusclient.Socket, n int, name string) versusclient.Message {
	t.Helper()
	must(t, s.Send(ctx, map[string]any{"type": "guess", "n": n, "name": name}))
	m, err := s.Await(ctx, func(m versusclient.Message) bool { return (m.Type == "guess" || m.Type == "error") && m.N == n })
	must(t, err)
	if m.Type == "error" {
		t.Fatalf("guess %d (%s): %s", n, name, m.Error)
	}
	return m
}

func TestARoundCarriesOnThroughADeploy(t *testing.T) {
	ctx := within(t, 60*time.Second)
	old, next := start(t), start(t)
	tn := newTunnel(t, old)
	code, alice, bob, aliceID, bobID := players(t, ctx, tn.url)
	race(t, ctx, old, code, alice, bob, bobID)

	right := old.answerOf(t, code)
	wrongs := []string{"bulbasaur", "ivysaur", "venusaur"}
	for i, w := range wrongs {
		if w == right {
			wrongs = append(wrongs[:i], wrongs[i+1:]...)
			break
		}
	}
	guess(t, ctx, bob, 1, wrongs[0])

	// The deploy: new connections go to the next version, then the old one
	// stops, closing its sockets with 1012.
	tn.switchTo(next)
	old.stop()

	// Both come back through the tunnel, now to the next version, and the
	// round carries on where it was: bob's next guess is his second.
	if m := guess(t, ctx, bob, 2, wrongs[1]); m.Type != "guess" {
		t.Fatalf("bob's second guess: %s", m.Raw)
	}
	guess(t, ctx, alice, 1, right)
	for _, s := range []*versusclient.Socket{alice, bob} {
		v, err := s.AwaitRoom(ctx, func(v live.View) bool { return v.Round != nil && v.Round.FinishedAt != nil })
		must(t, err)
		if v.Round.Winner != aliceID {
			t.Errorf("winner %q, want alice", v.Round.Winner)
		}
		// None of bob's guesses was lost or counted twice.
		if p := player(v, bobID); p.Guesses != 2 || len(p.Comparisons) != 2 {
			t.Errorf("bob's guesses after the deploy: %+v", p)
		}
	}
	if alice.Reconnects.Load() < 1 || bob.Reconnects.Load() < 1 {
		t.Errorf("reconnects: alice %d, bob %d; want both to have moved", alice.Reconnects.Load(), bob.Reconnects.Load())
	}

	// Whatever order the old version's disconnects and the new one's
	// connects landed in, both end up connected.
	_, err := bob.AwaitRoom(ctx, func(v live.View) bool { return member(v, aliceID).Connected && member(v, bobID).Connected })
	must(t, err)
}

func TestChangesGetThroughTheListenConnectionDropping(t *testing.T) {
	ctx := within(t, 60*time.Second)
	a, b := start(t), start(t)
	// alice on one instance, bob on the other: only Postgres connects them.
	ac, bc := versusclient.New(a.url), versusclient.New(b.url)
	must(t, ac.NewPlayer(ctx))
	must(t, bc.NewPlayer(ctx))
	code, err := ac.CreateRoom(ctx, []int{1})
	must(t, err)
	must(t, bc.Join(ctx, code))
	alice, bob := ac.Open(ctx, code), bc.Open(ctx, code)
	t.Cleanup(alice.Close)
	t.Cleanup(bob.Close)
	bv, err := bob.AwaitRoom(ctx, func(v live.View) bool { return len(v.Members) == 2 })
	must(t, err)
	bobID := bv.You
	_, err = alice.AwaitRoom(ctx, func(v live.View) bool { return member(v, bobID).Connected })
	must(t, err)

	// Postgres drops every hub's LISTEN connection (a database restart, a
	// failover), and a change is made at once, before they're back.
	var dropped int
	must(t, a.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = $1) t`, live.ListenerName).Scan(&dropped))
	if dropped < 2 {
		t.Fatalf("dropped %d LISTEN connections, want both hubs'", dropped)
	}
	must(t, bob.Send(ctx, map[string]any{"type": "ready", "ready": true}))

	// alice's hub reconnects, catches up on what it missed, and keeps up.
	_, err = alice.AwaitRoom(ctx, func(v live.View) bool { return member(v, bobID).Ready })
	must(t, err)
	must(t, bob.Send(ctx, map[string]any{"type": "ready", "ready": false}))
	_, err = alice.AwaitRoom(ctx, func(v live.View) bool { return !member(v, bobID).Ready })
	must(t, err)
}

func TestDeadlinesPassWithNobodyConnected(t *testing.T) {
	ctx := within(t, 60*time.Second)
	in := start(t)
	code, alice, bob, _, bobID := players(t, ctx, in.url)
	race(t, ctx, in, code, alice, bob, bobID)

	// Everyone goes, and the round runs out of time with nobody connected:
	// no hub has a timer for it, so the sweeper has to end it.
	alice.Close()
	bob.Close()
	if _, err := in.pool.Exec(context.Background(), `
		UPDATE room_rounds SET ends_at = clock_timestamp() - interval '1 second'
		WHERE room_id = (SELECT id FROM rooms WHERE code = $1)`, code); err != nil {
		t.Fatal(err)
	}
	for {
		var finished bool
		must(t, in.pool.QueryRow(context.Background(), `
			SELECT finished_at IS NOT NULL FROM room_rounds WHERE room_id = (SELECT id FROM rooms WHERE code = $1)`,
			code).Scan(&finished))
		if finished {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("the round never ended with nobody connected")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestATabOnEachInstance(t *testing.T) {
	ctx := within(t, 60*time.Second)
	a, b := start(t), start(t)
	ac, bc := versusclient.New(b.url), versusclient.New(b.url)
	must(t, ac.NewPlayer(ctx))
	must(t, bc.NewPlayer(ctx))
	code, err := ac.CreateRoom(ctx, []int{1})
	must(t, err)
	must(t, bc.Join(ctx, code))
	first, bob := ac.Open(ctx, code), bc.Open(ctx, code)
	t.Cleanup(first.Close)
	t.Cleanup(bob.Close)
	_, err = bob.AwaitRoom(ctx, func(v live.View) bool {
		return len(v.Members) == 2 && v.Members[0].Connected && v.Members[1].Connected
	})
	must(t, err)

	// alice opens a second tab, on the other instance, then closes it. That
	// instance records her as disconnected; the one with her first tab puts
	// it right.
	second := &versusclient.Client{Base: a.url, HTTP: ac.HTTP, Token: ac.Token}
	tab := second.Open(ctx, code)
	_, err = tab.AwaitRoom(ctx, func(live.View) bool { return true })
	must(t, err)
	tab.Close()

	disconnected := func() int {
		var n int
		must(t, a.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM room_members m JOIN rooms r ON r.id = m.room_id
			WHERE r.code = $1 AND m.disconnected_at IS NOT NULL`, code).Scan(&n))
		return n
	}
	// Give the disconnect time to land, then it must be put right, and stay.
	time.Sleep(500 * time.Millisecond)
	for disconnected() != 0 {
		select {
		case <-ctx.Done():
			t.Fatal("alice is recorded as disconnected with a tab still open")
		case <-time.After(100 * time.Millisecond):
		}
	}
	time.Sleep(time.Second)
	if n := disconnected(); n != 0 {
		t.Errorf("%d members recorded as disconnected a second later", n)
	}
}
