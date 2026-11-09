package live_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sashasagebd/pokewordle-api/internal/api"
	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/dbtest"
	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/live"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
)

const frontend = "https://pokewordle.example"

// An instance is one running copy of the app: its own pool, hub and server,
// on the shared test database, as during a deploy.
type instance struct {
	url  string
	pool *pgxpool.Pool
	dex  *games.Dex
	stop func() // stops the hub, as a shutdown does
}

func start(t *testing.T) *instance {
	t.Helper()
	pool := dbtest.Pool(t)
	ctx := context.Background()
	seed, err := pokedex.Seed()
	if err != nil {
		t.Fatal(err)
	}
	dexStore := pokedex.NewStore(pool)
	if _, err := dexStore.Load(ctx, seed); err != nil {
		t.Fatal(err)
	}
	dex := games.NewDex(seed)
	gs := games.NewStore(pool, &games.Engine{Dex: dex})
	rs := rooms.NewStore(pool, gs)

	hubCtx, cancel := context.WithCancel(ctx)
	hub := live.NewHub(hubCtx, pool, rs, gs)
	done := make(chan struct{})
	go func() { hub.Run(); close(done) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }

	srv := httptest.NewServer(api.New(api.Config{
		Dex: dexStore, Players: auth.NewStore(pool), Games: gs, Silhouette: pokedex.Silhouette,
		AllowedOrigins: []string{frontend}, Rooms: rs, Sockets: hub,
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(stop) // first: sockets close before the server and pool
	return &instance{srv.URL, pool, dex, stop}
}

// post sends a JSON request and decodes the answer into out, failing the
// test unless the status is want.
func (in *instance) post(t *testing.T, path, token string, body any, want int, out any) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, in.url+path, bytes.NewReader(data))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != want {
		t.Fatalf("POST %s: %d, want %d", path, res.StatusCode, want)
	}
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

func (in *instance) newPlayer(t *testing.T) string {
	t.Helper()
	var body struct{ Token string }
	in.post(t, "/players", "", nil, http.StatusCreated, &body)
	return body.Token
}

func (in *instance) ticket(t *testing.T, token, code string) string {
	t.Helper()
	var body struct{ Ticket string }
	in.post(t, "/rooms/"+code+"/ticket", token, nil, http.StatusCreated, &body)
	return body.Ticket
}

func (in *instance) wsURL(ticket string) string {
	return "ws" + strings.TrimPrefix(in.url, "http") + "/ws?ticket=" + ticket
}

// msg is any message from the server.
type msg struct {
	Type   string           `json:"type"`
	Seq    int              `json:"seq"`
	Room   live.View        `json:"room"`
	N      int              `json:"n"`
	Result games.Comparison `json:"result"`
	Error  string           `json:"error"`
	raw    string
}

// A client is a player's socket.
type client struct {
	t    *testing.T
	conn *websocket.Conn
	msgs chan msg
	// closed gets the error the socket ended with.
	closed chan error
}

func (in *instance) dial(t *testing.T, ticket string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, in.wsURL(ticket), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {frontend}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t, conn, make(chan msg, 100), make(chan error, 1)}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				c.closed <- err
				close(c.msgs)
				return
			}
			var m msg
			if err := json.Unmarshal(data, &m); err != nil {
				c.closed <- err
				close(c.msgs)
				return
			}
			m.raw = string(data)
			c.msgs <- m
		}
	}()
	t.Cleanup(func() { conn.CloseNow() })
	return c
}

func (c *client) send(m map[string]any) {
	c.t.Helper()
	data, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		c.t.Fatal(err)
	}
}

// await returns the first message that ok accepts, skipping the others.
func (c *client) await(what string, ok func(msg) bool) msg {
	c.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, open := <-c.msgs:
			if !open {
				c.t.Fatalf("socket closed waiting for %s: %v", what, <-c.closed)
			}
			if m.Type == "error" {
				c.t.Logf("error message: %s", m.raw)
			}
			if ok(m) {
				return m
			}
		case <-timeout:
			c.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// awaitRoom returns the first room view that ok accepts.
func (c *client) awaitRoom(what string, ok func(live.View) bool) live.View {
	c.t.Helper()
	return c.await(what, func(m msg) bool { return m.Type == "room" && ok(m.Room) }).Room
}

func member(v live.View, id string) live.MemberView {
	for _, m := range v.Members {
		if m.ID == id {
			return m
		}
	}
	return live.MemberView{}
}

func player(v live.View, id string) live.PlayerView {
	for _, p := range v.Round.Players {
		if p.ID == id {
			return p
		}
	}
	return live.PlayerView{}
}

// skipCountdown moves the open round of room code to after its countdown.
func (in *instance) skipCountdown(t *testing.T, code string) {
	t.Helper()
	if _, err := in.pool.Exec(context.Background(), `
		UPDATE room_rounds SET starts_at = starts_at - $2::float8 * interval '1 second',
			ends_at = ends_at - $2::float8 * interval '1 second'
		WHERE room_id = (SELECT id FROM rooms WHERE code = $1) AND finished_at IS NULL`,
		code, rooms.Countdown.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func (in *instance) answerOf(t *testing.T, code string) string {
	t.Helper()
	var id int
	if err := in.pool.QueryRow(context.Background(), `
		SELECT answer_id FROM room_rounds WHERE room_id = (SELECT id FROM rooms WHERE code = $1)
		ORDER BY n DESC LIMIT 1`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return in.dex.Get(id).Name
}

// room makes a room on a with a player there, joined by a player on b, both
// connected: alice's socket is to a, bob's to b.
func room(t *testing.T, a, b *instance) (code string, alice, bob *client, aliceID, bobID string) {
	t.Helper()
	aliceToken, bobToken := a.newPlayer(t), b.newPlayer(t)
	var made struct{ Code string }
	a.post(t, "/rooms", aliceToken, map[string]any{"gens": []int{1}}, http.StatusCreated, &made)
	code = made.Code
	b.post(t, "/rooms/"+strings.ToLower(code)+"/join", bobToken, nil, http.StatusOK, nil)

	alice = a.dial(t, a.ticket(t, aliceToken, code))
	bob = b.dial(t, b.ticket(t, bobToken, code))
	aliceID = alice.awaitRoom("alice's first view", func(live.View) bool { return true }).You
	bobID = bob.awaitRoom("bob's first view", func(live.View) bool { return true }).You
	alice.awaitRoom("bob connected, as alice sees it", func(v live.View) bool { return member(v, bobID).Connected })
	return code, alice, bob, aliceID, bobID
}

func TestARaceAcrossTwoInstances(t *testing.T) {
	a, b := start(t), start(t)
	code, alice, bob, aliceID, bobID := room(t, a, b)

	// Every change crosses between the instances through Postgres.
	bob.send(map[string]any{"type": "ready", "ready": true})
	alice.awaitRoom("bob ready", func(v live.View) bool { return member(v, bobID).Ready })
	alice.send(map[string]any{"type": "start"})
	for _, c := range []*client{alice, bob} {
		v := c.awaitRoom("the round", func(v live.View) bool { return v.Status == rooms.Playing })
		if v.Round == nil || v.Round.N != 1 || v.Round.Game == nil || len(v.Round.Players) != 2 {
			t.Fatalf("round started: %+v", v.Round)
		}
	}

	// Too early: still counting down.
	right := a.answerOf(t, code)
	wrong := "bulbasaur"
	if right == wrong {
		wrong = "ivysaur"
	}
	bob.send(map[string]any{"type": "guess", "n": 1, "name": wrong})
	bob.await("the countdown refusal", func(m msg) bool { return m.Type == "error" && m.N == 1 })
	b.skipCountdown(t, code)

	bob.send(map[string]any{"type": "guess", "n": 1, "name": wrong})
	if m := bob.await("bob's result", func(m msg) bool { return m.Type == "guess" }); m.N != 1 || m.Result.Correct {
		t.Fatalf("bob's guess: %+v", m)
	}
	// Alice sees bob's tile, but not what he guessed.
	var seen msg
	alice.await("bob's tile", func(m msg) bool {
		seen = m
		return m.Type == "room" && len(player(m.Room, bobID).Tiles) == 1
	})
	if strings.Contains(seen.raw, wrong) {
		t.Errorf("alice's view shows bob's guess %q: %s", wrong, seen.raw)
	}

	alice.send(map[string]any{"type": "guess", "n": 1, "name": right})
	for _, c := range []*client{alice, bob} {
		v := c.awaitRoom("the result", func(v live.View) bool { return v.Round != nil && v.Round.FinishedAt != nil })
		if v.Status != rooms.Lobby || v.Round.Winner != aliceID || v.Round.Answer == nil || v.Round.Answer.Name != right {
			t.Errorf("result: %s, %+v", v.Status, v.Round)
		}
		if p := player(v, bobID); p.Outcome != rooms.Unsolved || len(p.Comparisons) != 1 {
			t.Errorf("bob in the results: %+v", p)
		}
	}
}

func TestDisconnectsShowAndShutdownSaysReconnect(t *testing.T) {
	a, b := start(t), start(t)
	code, alice, bob, _, bobID := room(t, a, b)

	bob.conn.Close(websocket.StatusNormalClosure, "bye")
	v := alice.awaitRoom("bob gone", func(v live.View) bool { return !member(v, bobID).Connected })
	if member(v, bobID).GraceEndsAt == nil {
		t.Error("bob's grace isn't shown")
	}

	// Stopping alice's instance closes her socket with "service restart",
	// and records her as disconnected.
	a.stop()
	select {
	case err := <-alice.closed:
		if code := websocket.CloseStatus(err); code != websocket.StatusServiceRestart {
			t.Errorf("closed with %v, want 1012", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("alice's socket is still open after shutdown")
	}
	var disconnected int
	if err := b.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM room_members
		WHERE room_id = (SELECT id FROM rooms WHERE code = $1) AND disconnected_at IS NOT NULL`,
		code).Scan(&disconnected); err != nil {
		t.Fatal(err)
	}
	if disconnected != 2 {
		t.Errorf("%d of 2 members recorded as disconnected after the shutdown", disconnected)
	}
}

func TestSocketsCheckOriginAndTickets(t *testing.T) {
	a := start(t)
	token := a.newPlayer(t)
	var made struct{ Code string }
	a.post(t, "/rooms", token, map[string]any{}, http.StatusCreated, &made)
	ticket := a.ticket(t, token, made.Code)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, res, err := websocket.Dial(ctx, a.wsURL(ticket), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"https://evil.example"}},
	})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("from another origin: %v, %v; want 403", res, err)
	}

	a.dial(t, ticket).awaitRoom("the room", func(live.View) bool { return true })
	_, res, err = websocket.Dial(ctx, a.wsURL(ticket), nil)
	if err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a used ticket: %v, %v; want 401", res, err)
	}

	// Not a member: no ticket.
	a.post(t, "/rooms/"+made.Code+"/ticket", a.newPlayer(t), nil, http.StatusForbidden, nil)
	a.post(t, "/rooms/ZZZZZZ/join", token, nil, http.StatusNotFound, nil)
}

// pairOf returns Higher or Lower round n of room code's latest round (the
// same for every player): the Pokémon shown and the one whose value is
// hidden, as only a test may see them.
func (in *instance) pairOf(t *testing.T, code string, n int) (shown, hidden pokedex.Pokemon) {
	t.Helper()
	var other, answer int
	if err := in.pool.QueryRow(context.Background(), `
		SELECT gr.other_id, gr.answer_id FROM game_rounds gr
		JOIN room_round_players p ON p.game_id = gr.game_id
		JOIN room_rounds rd ON rd.id = p.round_id
		JOIN rooms r ON r.id = rd.room_id
		WHERE r.code = $1 AND gr.n = $2 ORDER BY rd.n DESC LIMIT 1`, code, n).Scan(&other, &answer); err != nil {
		t.Fatal(err)
	}
	return in.dex.Get(other), in.dex.Get(answer)
}

func TestAHigherOrLowerRaceAcrossTwoInstances(t *testing.T) {
	a, b := start(t), start(t)
	code, alice, bob, aliceID, bobID := room(t, a, b)

	alice.send(map[string]any{"type": "settings", "mode": "higher_lower", "gens": []int{1}})
	bob.awaitRoom("the mode change", func(v live.View) bool { return v.Mode == "higher_lower" })
	bob.send(map[string]any{"type": "ready", "ready": true})
	alice.awaitRoom("bob ready", func(v live.View) bool { return member(v, bobID).Ready })
	alice.send(map[string]any{"type": "start"})
	for _, c := range []*client{alice, bob} {
		v := c.awaitRoom("the round", func(v live.View) bool { return v.Status == rooms.Playing })
		if v.Round.Mode != "higher_lower" || v.Round.Game == nil || v.Round.Game.Round == nil || v.Round.Game.Round.Current == nil {
			t.Fatalf("round started: %+v", v.Round)
		}
	}
	a.skipCountdown(t, code)

	// A stat the two differ on: a tie would count either answer as right.
	shown, hidden := a.pairOf(t, code, 1)
	stat, s, h := "height", shown.Height, hidden.Height
	if s == h {
		stat, s, h = "weight", shown.Weight, hidden.Weight
	}
	if s == h {
		t.Skip("the first pair ties on both stats")
	}
	right, wrong := "higher", "lower"
	if h < s {
		right, wrong = wrong, right
	}

	alice.send(map[string]any{"type": "answer", "n": 1, "dir": right, "stat": stat})
	var got struct {
		Type    string `json:"type"`
		Correct bool   `json:"correct"`
	}
	m := alice.await("alice's result", func(m msg) bool { return m.Type == "answer" })
	if err := json.Unmarshal([]byte(m.raw), &got); err != nil || !got.Correct {
		t.Fatalf("alice's right answer: %s", m.raw)
	}
	bob.awaitRoom("alice's streak", func(v live.View) bool { return player(v, aliceID).Score == 1 })

	// Bob misses the same pair: alice is ahead and alone, so she's won.
	bob.send(map[string]any{"type": "answer", "n": 1, "dir": wrong, "stat": stat})
	for _, c := range []*client{alice, bob} {
		v := c.awaitRoom("the result", func(v live.View) bool { return v.Round != nil && v.Round.FinishedAt != nil })
		if v.Round.Winner != aliceID {
			t.Errorf("winner %q, want alice", v.Round.Winner)
		}
		if p := player(v, bobID); p.Outcome != rooms.Out || p.EndedOn == nil || p.EndedOn.ID != hidden.ID {
			t.Errorf("bob in the results: %+v", p)
		}
	}
}
