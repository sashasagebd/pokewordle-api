// Package versusbot plays Versus with bots, for cmd/versuscheck (seeing a
// deploy through) and cmd/versusload (many rooms at once). They play badly
// on purpose, at random a moment or two apart, so streak rounds are short
// and many, and round after round: the host starts the next as soon as the
// others are ready.
//
// While they play, they measure, into Stats:
//
//   - reply: from a bot sending a move to its answer coming back
//   - broadcast: from a bot sending a move to another bot in the room seeing
//     it (the room's next version, through the server's LISTEN/NOTIFY and
//     the other's socket): what an opponent waits for
//
// and note problems: a move never answered, an error from the server other
// than the expected kind, a room going back to an older version, a bot
// losing the room.
package versusbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/live"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
	"github.com/sashasagebd/pokewordle-api/internal/versusclient"
)

// Stats are what the bots did and saw, across every room.
type Stats struct {
	Moves, Answered, Rounds atomic.Int64
	Reply, Broadcast        Samples
	// Log, if set, gets each problem as it happens.
	Log *log.Logger

	mu       sync.Mutex
	problems []string
}

// Problem notes something that went wrong.
func (st *Stats) Problem(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if st.Log != nil {
		st.Log.Print("PROBLEM: " + msg)
	}
	st.mu.Lock()
	st.problems = append(st.problems, msg)
	st.mu.Unlock()
}

// Problems returns every problem noted.
func (st *Stats) Problems() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Clone(st.problems)
}

// Samples are durations measured, for their quantiles.
type Samples struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *Samples) Add(d time.Duration) {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
}

// Reset forgets every sample.
func (s *Samples) Reset() {
	s.mu.Lock()
	s.d = nil
	s.mu.Unlock()
}

// Len is how many there are.
func (s *Samples) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.d)
}

// Quantile returns the q quantile (0.5 for the median, 1 for the most), or 0
// with no samples.
func (s *Samples) Quantile(q float64) time.Duration {
	s.mu.Lock()
	d := slices.Clone(s.d)
	s.mu.Unlock()
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	i := int(q*float64(len(d))+0.5) - 1
	return d[min(max(i, 0), len(d)-1)]
}

// A Room is what the bots in one room share: when each move was sent, so
// whichever bot sees it first can time the broadcast.
type Room struct {
	mu   sync.Mutex
	sent map[move]time.Time
}

// A move is a player's move n in a round.
type move struct {
	player string // public ID
	round  int
	n      int
}

func (r *Room) moved(m move, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sent == nil {
		r.sent = map[move]time.Time{}
	}
	if _, again := r.sent[m]; !again { // a resend doesn't restart the clock
		r.sent[m] = at
	}
}

// seen times every move the view shows (for players other than the viewer)
// that no one has seen before, and forgets them.
func (r *Room) seen(v live.View, at time.Time, into *Samples) {
	rd := v.Round
	if rd == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range rd.Players {
		if p.ID == v.You {
			continue
		}
		done := progress(string(rd.Mode), p)
		for m, sent := range r.sent {
			if m.player == p.ID && m.round == rd.N && m.n <= done {
				into.Add(at.Sub(sent))
				delete(r.sent, m)
			}
		}
	}
}

// progress is how many moves a player's race shows: Wordle guesses, or
// streak answers (each right one, and the wrong one ending a run).
func progress(mode string, p live.PlayerView) int {
	if mode == "wordle" {
		return p.Guesses
	}
	if p.Outcome == rooms.Out {
		return p.Score + 1
	}
	return p.Score
}

// A Bot is one player in one room.
type Bot struct {
	Name  string
	Host  bool     // the one who sets the mode and starts rounds
	Mode  string   // wordle, higher_lower or whos_that
	Names []string // Pokémon to guess from (the room plays Gen 1)
	S     *versusclient.Socket
	Stats *Stats
	Room  *Room
	// Think is how long a bot waits before each move: at random between
	// Think and twice it. 1s if 0.
	Think time.Duration

	seq   int
	asked int // the room version it last asked for something in (ready, start, settings)
	// The round it last asked to start after (0: before the first), and
	// when: the lobby may change again before the round begins (someone's
	// connection recorded), and that's no reason to ask twice.
	startAfter int
	startedAt  time.Time
	round      int
	sent       int // the move number it last sent
	sentAt     time.Time
	pending    bool // a move sent, not yet answered
	guessed    map[string]bool
	timer      *time.Timer
	// later runs what a timer set up, in Play's goroutine: only Play touches
	// the bot's state.
	later chan func()
}

// Unanswered reports whether the bot ended with a move unanswered for a
// while: lost.
func (b *Bot) Unanswered() bool { return b.pending && time.Since(b.sentAt) > 10*time.Second }

// Play plays until ctx ends or the room is lost.
func (b *Bot) Play(ctx context.Context) {
	b.later = make(chan func(), 1)
	// A nudge, for when nothing arrives to act on (a reply lost in a
	// reconnect): look at the room again, and send a lost move again.
	nudge := time.NewTicker(3 * time.Second)
	defer nudge.Stop()
	defer func() {
		if b.timer != nil {
			b.timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-b.later:
			f()
		case <-nudge.C:
			if v, seq := b.S.Room(); seq > 0 {
				b.act(ctx, v, seq, time.Now())
			}
		case m, open := <-b.S.Messages():
			if !open {
				if ctx.Err() == nil {
					b.Stats.Problem("%s lost the room: %v", b.Name, b.S.Err())
				}
				return
			}
			b.handle(ctx, m)
		}
	}
}

// expected are errors that happen in play: a resend refused after a
// reconnect, the round ending before a move, a newcomer mid-round, slowing
// down, a guess the bot shouldn't have made.
var expected = []string{
	"already been answered", "no round", "already finished", "not playing", "slow down",
	"hasn't started", "isn't played", "already guessed", "isn't in the selected", "no pokémon called",
	"everyone has to be ready", "at least 2",
}

func (b *Bot) handle(ctx context.Context, m versusclient.Message) {
	now := time.Now()
	switch m.Type {
	case "room":
		if m.Seq < b.seq {
			b.Stats.Problem("%s: the room went back from version %d to %d", b.Name, b.seq, m.Seq)
			return
		}
		b.seq = m.Seq
		b.Room.seen(m.Room, now, &b.Stats.Broadcast)
		b.act(ctx, m.Room, m.Seq, m.ServerNow)
	case "guess", "answer":
		if m.N == b.sent && b.pending {
			b.pending = false
			b.Stats.Answered.Add(1)
			b.Stats.Reply.Add(now.Sub(b.sentAt))
		}
		v, seq := b.S.Room()
		b.act(ctx, v, seq, now)
	case "error":
		for _, ok := range expected {
			if strings.Contains(strings.ToLower(m.Error), ok) {
				if m.N == b.sent {
					b.pending = false
				}
				v, seq := b.S.Room()
				b.act(ctx, v, seq, now)
				return
			}
		}
		b.Stats.Problem("%s: error for %s: %s", b.Name, m.For, m.Error)
	}
}

// act does what the room calls for: settings, readying, starting, a move.
// seq is v's version: the socket's newest room can be ahead of the last
// message handled, so what was asked is kept by the version it was asked in.
func (b *Bot) act(ctx context.Context, v live.View, seq int, serverNow time.Time) {
	if b.timer != nil {
		b.timer.Stop()
	}
	switch {
	case v.Status == rooms.Lobby:
		b.lobby(ctx, v, seq)
	case v.Status == rooms.Playing && v.Round != nil:
		b.race(ctx, v, serverNow)
	}
}

func (b *Bot) lobby(ctx context.Context, v live.View, seq int) {
	if seq <= b.asked {
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
	case b.Host && string(v.Mode) != b.Mode:
		b.asked = seq
		b.send(ctx, map[string]any{"type": "settings", "mode": b.Mode, "gens": []int{1}})
	case b.Host && allReady:
		last := 0
		if v.Round != nil {
			last = v.Round.N
		}
		if last == b.startAfter && !b.startedAt.IsZero() && time.Since(b.startedAt) < 5*time.Second {
			return // asked already; the round is on its way
		}
		b.asked, b.startAfter, b.startedAt = seq, last, time.Now()
		b.send(ctx, map[string]any{"type": "start"})
	case !b.Host && !me.Ready && string(v.Mode) == b.Mode:
		b.asked = seq
		b.send(ctx, map[string]any{"type": "ready", "ready": true})
	}
}

func (b *Bot) race(ctx context.Context, v live.View, serverNow time.Time) {
	rd := v.Round
	if rd.N != b.round {
		b.round, b.sent, b.pending, b.guessed = rd.N, 0, false, map[string]bool{}
		b.Stats.Rounds.Add(1)
	}
	if _, ok := b.next(v); !ok {
		return
	}
	think := b.Think
	if think == 0 {
		think = time.Second
	}
	wait := think + rand.N(think)
	if until := rd.StartsAt.Sub(serverNow); until > 0 {
		wait += until // after the countdown
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
func (b *Bot) next(v live.View) (int, bool) {
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
	if b.Mode != "wordle" {
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
func (b *Bot) moveNow(ctx context.Context) {
	v, _ := b.S.Room()
	n, ok := b.next(v)
	if !ok {
		return
	}
	now := time.Now()
	if !b.pending || b.sent != n {
		b.sentAt = now // a resend keeps the first's time
	}
	b.sent, b.pending = n, true
	b.Stats.Moves.Add(1)
	b.Room.moved(move{player: v.You, round: v.Round.N, n: n}, now)
	b.send(ctx, b.move(n))
}

func (b *Bot) move(n int) map[string]any {
	switch b.Mode {
	case "higher_lower":
		dir := []string{"higher", "lower"}[rand.IntN(2)]
		return map[string]any{"type": "answer", "n": n, "dir": dir, "stat": "height"}
	case "whos_that":
		return map[string]any{"type": "answer", "n": n, "name": b.Names[rand.IntN(len(b.Names))]}
	}
	for {
		name := b.Names[rand.IntN(len(b.Names))]
		if !b.guessed[name] {
			b.guessed[name] = true
			return map[string]any{"type": "guess", "n": n, "name": name}
		}
	}
}

func (b *Bot) send(ctx context.Context, msg map[string]any) {
	if err := b.S.Send(ctx, msg); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		b.Stats.Problem("%s: sending %v: %v", b.Name, msg["type"], err)
	}
}
