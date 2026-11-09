package versusbot

import (
	"testing"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/live"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
)

func TestQuantiles(t *testing.T) {
	var s Samples
	if s.Quantile(0.5) != 0 {
		t.Error("no samples isn't 0")
	}
	for i := 100; i >= 1; i-- {
		s.Add(time.Duration(i) * time.Millisecond)
	}
	for q, want := range map[float64]time.Duration{0.5: 50 * time.Millisecond, 0.99: 99 * time.Millisecond, 1: 100 * time.Millisecond, 0: time.Millisecond} {
		if got := s.Quantile(q); got != want {
			t.Errorf("Quantile(%v) = %v, want %v", q, got, want)
		}
	}
	s.Reset()
	if s.Len() != 0 {
		t.Error("Reset kept samples")
	}
}

func TestBroadcastIsTimedOnceWhenSeen(t *testing.T) {
	var r Room
	var into Samples
	t0 := time.Now()
	r.moved(move{"alice", 1, 1}, t0)
	r.moved(move{"alice", 1, 1}, t0.Add(time.Second)) // a resend keeps the first time
	r.moved(move{"alice", 1, 2}, t0.Add(2*time.Second))

	view := func(you string, score int, out bool) live.View {
		outcome := rooms.Standing
		if out {
			outcome = rooms.Out
		}
		return live.View{You: you, Round: &live.RoundView{N: 1, Mode: "higher_lower",
			Players: []live.PlayerView{{ID: "alice", Score: score, Outcome: outcome}, {ID: "bob"}}}}
	}
	// alice herself seeing it doesn't count.
	r.seen(view("alice", 1, false), t0.Add(100*time.Millisecond), &into)
	if into.Len() != 0 {
		t.Fatal("timed alice's move when she saw it")
	}
	// bob sees her first answer: timed from when it was first sent.
	r.seen(view("bob", 1, false), t0.Add(150*time.Millisecond), &into)
	if into.Len() != 1 || into.Quantile(1) != 150*time.Millisecond {
		t.Fatalf("after bob saw move 1: %d samples, %v", into.Len(), into.Quantile(1))
	}
	// Seeing it again doesn't count again; her second (a miss, ending her
	// run) does.
	r.seen(view("bob", 1, false), t0.Add(time.Second), &into)
	r.seen(view("bob", 1, true), t0.Add(2*time.Second+30*time.Millisecond), &into)
	if into.Len() != 2 || into.Quantile(0) != 30*time.Millisecond {
		t.Errorf("after bob saw move 2: %d samples, least %v", into.Len(), into.Quantile(0))
	}
}

func TestProgress(t *testing.T) {
	for _, c := range []struct {
		mode string
		p    live.PlayerView
		want int
	}{
		{"wordle", live.PlayerView{Guesses: 4, Outcome: rooms.Unsolved}, 4},
		{"higher_lower", live.PlayerView{Score: 3, Outcome: rooms.Standing}, 3},
		{"whos_that", live.PlayerView{Score: 3, Outcome: rooms.Out}, 4},
		{"whos_that", live.PlayerView{Score: 2, Outcome: rooms.Forfeited}, 2},
	} {
		if got := progress(c.mode, c.p); got != c.want {
			t.Errorf("progress(%s, %+v) = %d, want %d", c.mode, c.p, got, c.want)
		}
	}
}
