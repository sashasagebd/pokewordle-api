// Command versuscheck plays Versus against a running API with two bots, round
// after round, to see it through a deploy: start it, deploy, and see whether
// the bots carried on without losing anything.
//
//	go run ./cmd/versuscheck -api https://pokewordle-api.example.com -for 5m
//
// The bots play badly on purpose (random answers, a second or two apart), so
// streak rounds are short and many. Every 10 seconds it prints what they've
// done; at the end, a summary, exiting 1 if anything went wrong:
//
//   - a move never answered (lost in a reconnect)
//   - an error from the server other than the expected kind (a resend
//     refused, a round that ended first)
//   - a room going back to an older version
//   - a bot losing the room for good
//
// Reconnects aren't problems: a deploy causes them. It's how the bots come
// through them that counts.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/live"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
	"github.com/sashasagebd/pokewordle-api/internal/versusclient"
)

func main() {
	api := flag.String("api", "https://pokewordle-api.sashasagebd.com", "the API to play against")
	length := flag.Duration("for", 5*time.Minute, "how long to play")
	mode := flag.String("mode", "higher_lower", "the mode: wordle, higher_lower or whos_that")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *length)
	defer cancel()

	if err := run(ctx, strings.TrimRight(*api, "/"), *mode); err != nil {
		log.Fatal(err)
	}
}

// stats are what the bots did and what went wrong.
type stats struct {
	moves, answered, rounds atomic.Int64
	mu                      sync.Mutex
	problems                []string
}

func (st *stats) problem(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Print("PROBLEM: " + msg)
	st.mu.Lock()
	st.problems = append(st.problems, msg)
	st.mu.Unlock()
}

func run(ctx context.Context, api, mode string) error {
	setup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	names, err := pokemonNames(setup, api)
	if err != nil {
		return fmt.Errorf("loading Pokémon names: %w", err)
	}

	host, guest := versusclient.New(api), versusclient.New(api)
	if err := host.NewPlayer(setup); err != nil {
		return err
	}
	if err := guest.NewPlayer(setup); err != nil {
		return err
	}
	code, err := host.CreateRoom(setup, []int{1})
	if err != nil {
		return err
	}
	if err := guest.Join(setup, code); err != nil {
		return err
	}
	log.Printf("playing %s in room %s; deploy whenever you like", mode, code)

	st := &stats{}
	bots := []*bot{
		{name: "host", host: true, mode: mode, names: names, st: st, s: host.Open(ctx, code)},
		{name: "guest", mode: mode, names: names, st: st, s: guest.Open(ctx, code)},
	}
	var wg sync.WaitGroup
	for _, b := range bots {
		wg.Go(func() { b.play(ctx) })
	}

	report := time.NewTicker(10 * time.Second)
	defer report.Stop()
	summary := func() string {
		return fmt.Sprintf("rounds %d, moves %d (answered %d), reconnects: host %d, guest %d",
			st.rounds.Load()/2, st.moves.Load(), st.answered.Load(),
			bots[0].s.Reconnects.Load(), bots[1].s.Reconnects.Load())
	}
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-report.C:
			log.Print(summary())
		}
	}
	wg.Wait()

	for _, b := range bots {
		if n := b.unanswered(); n > 0 {
			st.problem("%s: %d move(s) never answered", b.name, n)
		}
	}
	log.Print("done: " + summary())
	if len(st.problems) > 0 {
		return fmt.Errorf("%d problem(s): %s", len(st.problems), strings.Join(st.problems, "; "))
	}
	log.Print("no problems")
	return nil
}

// pokemonNames lists Gen 1's names, for guesses.
func pokemonNames(ctx context.Context, api string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/pokemon?gens=1", nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var ps []struct{ Name string }
	if err := json.NewDecoder(res.Body).Decode(&ps); err != nil {
		return nil, err
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return names, nil
}

// A bot is one player, acting on each version of the room it's sent.
type bot struct {
	name  string
	host  bool
	mode  string
	names []string
	st    *stats
	s     *versusclient.Socket

	seq     int
	asked   int       // the room version it last asked for something in (ready, start, settings)
	sent    int       // the move number it last sent
	sentAt  time.Time // when, to send it again if it seems lost
	pending bool      // a move sent, not yet answered
	guessed map[string]bool
	round   int
	timer   *time.Timer
	// later runs what a timer set up, in play's goroutine: only play touches
	// the bot's state.
	later chan func()
}

func (b *bot) unanswered() int {
	if b.pending && time.Since(b.sentAt) > 10*time.Second {
		return 1
	}
	return 0
}

func (b *bot) play(ctx context.Context) {
	b.later = make(chan func(), 1)
	// A nudge, for when nothing arrives to act on (a reply lost in a
	// reconnect): look at the room again, and send a lost move again.
	nudge := time.NewTicker(3 * time.Second)
	defer nudge.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-b.later:
			f()
		case <-nudge.C:
			if v, seq := b.s.Room(); seq > 0 {
				b.act(ctx, v, time.Now())
			}
		case m, open := <-b.s.Messages():
			if !open {
				if ctx.Err() == nil {
					b.st.problem("%s lost the room: %v", b.name, b.s.Err())
				}
				return
			}
			b.handle(ctx, m)
		}
	}
}

func (b *bot) handle(ctx context.Context, m versusclient.Message) {
	switch m.Type {
	case "room":
		if m.Seq < b.seq {
			b.st.problem("%s: room went back from version %d to %d", b.name, b.seq, m.Seq)
			return
		}
		b.seq = m.Seq
		b.act(ctx, m.Room, m.ServerNow)
	case "guess", "answer":
		if m.N == b.sent && b.pending {
			b.pending = false
			b.st.answered.Add(1)
		}
		v, _ := b.s.Room()
		b.act(ctx, v, time.Now())
	case "error":
		// Expected: a resend refused after a reconnect, the round ending
		// before the move, a mid-round newcomer, slowing down.
		for _, ok := range []string{"already been answered", "no round", "already finished", "not playing", "slow down", "hasn't started", "isn't played", "already guessed", "isn't in the selected", "no pokémon called", "everyone has to be ready", "at least 2"} {
			if strings.Contains(strings.ToLower(m.Error), ok) {
				if m.N == b.sent {
					b.pending = false
				}
				v, _ := b.s.Room()
				b.act(ctx, v, time.Now())
				return
			}
		}
		b.st.problem("%s: error for %s: %s", b.name, m.For, m.Error)
	}
}

// act does what the room calls for: settings, readying, starting, a move.
func (b *bot) act(ctx context.Context, v live.View, serverNow time.Time) {
	if b.timer != nil {
		b.timer.Stop()
	}
	switch {
	case v.Status == rooms.Lobby:
		b.lobby(ctx, v)
	case v.Status == rooms.Playing && v.Round != nil:
		b.race(ctx, v, serverNow)
	}
}

func (b *bot) lobby(ctx context.Context, v live.View) {
	if b.asked == b.seq {
		return // already asked in this version; wait for the next
	}
	var me live.MemberView
	allReady := len(v.Members) >= 2
	for _, m := range v.Members {
		if m.ID == v.You {
			me = m
		} else if m.ID != v.Host && !m.Ready {
			allReady = false
		}
	}
	switch {
	case b.host && string(v.Mode) != b.mode:
		b.asked = b.seq
		b.send(ctx, map[string]any{"type": "settings", "mode": b.mode, "gens": []int{1}})
	case b.host && allReady:
		b.asked = b.seq
		b.send(ctx, map[string]any{"type": "start"})
	case !b.host && !me.Ready && string(v.Mode) == b.mode:
		b.asked = b.seq
		b.send(ctx, map[string]any{"type": "ready", "ready": true})
	}
}

func (b *bot) race(ctx context.Context, v live.View, serverNow time.Time) {
	rd := v.Round
	if rd.N != b.round {
		b.round, b.sent, b.pending, b.guessed = rd.N, 0, false, map[string]bool{}
		b.st.rounds.Add(1)
	}
	if _, ok := b.next(v); !ok {
		return
	}
	// A second or two after the countdown, or the last move.
	wait := time.Duration(1000+rand.IntN(1000)) * time.Millisecond
	if until := rd.StartsAt.Sub(serverNow); until > 0 {
		wait += until
	}
	b.timer = time.AfterFunc(wait, func() {
		select {
		case b.later <- func() { b.moveNow(ctx) }:
		default: // one's waiting already, and acts on the newest room too
		}
	})
}

// next returns the number of the move the bot is on in v's round, and
// whether it should make it: it's racing, and isn't waiting on an answer to
// it (for a while: a move lost in a reconnect is sent again).
func (b *bot) next(v live.View) (int, bool) {
	rd := v.Round
	if v.Status != rooms.Playing || rd == nil || rd.N != b.round {
		return 0, false
	}
	var me *live.PlayerView
	for i := range rd.Players {
		if rd.Players[i].ID == v.You {
			me = &rd.Players[i]
		}
	}
	g := rd.Game
	if me == nil || !me.Racing || g == nil {
		return 0, false
	}
	// Wordle counts guesses, the others rounds.
	n := len(g.Guesses) + 1
	if b.mode != "wordle" {
		if g.Round == nil {
			return 0, false
		}
		n = g.Round.N
	}
	if b.pending && n == b.sent && time.Since(b.sentAt) < 5*time.Second {
		return n, false
	}
	return n, true
}

// moveNow makes the move the newest room calls for, if any.
func (b *bot) moveNow(ctx context.Context) {
	v, _ := b.s.Room()
	n, ok := b.next(v)
	if !ok {
		return
	}
	b.sent, b.sentAt, b.pending = n, time.Now(), true
	b.st.moves.Add(1)
	b.send(ctx, b.move(n))
}

func (b *bot) move(n int) map[string]any {
	switch b.mode {
	case "higher_lower":
		dir := []string{"higher", "lower"}[rand.IntN(2)]
		return map[string]any{"type": "answer", "n": n, "dir": dir, "stat": "height"}
	case "whos_that":
		return map[string]any{"type": "answer", "n": n, "name": b.names[rand.IntN(len(b.names))]}
	}
	for {
		name := b.names[rand.IntN(len(b.names))]
		if !b.guessed[name] {
			b.guessed[name] = true
			return map[string]any{"type": "guess", "n": n, "name": name}
		}
	}
}

func (b *bot) send(ctx context.Context, msg map[string]any) {
	if err := b.s.Send(ctx, msg); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		b.st.problem("%s: sending %v: %v", b.name, msg["type"], err)
	}
}
