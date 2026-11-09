package live_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/versusbot"
	"github.com/sashasagebd/pokewordle-api/internal/versusclient"
)

// TestBotsPlayRoundAfterRound runs the bots that cmd/versuscheck and
// cmd/versusload play with, on two instances, so the tools are tested too:
// round after round, nothing going wrong, and the numbers they report
// measured.
func TestBotsPlayRoundAfterRound(t *testing.T) {
	ctx := within(t, 60*time.Second)
	a, b := start(t), start(t)
	names, err := versusclient.PokemonNames(ctx, a.url, 1)
	must(t, err)

	host, guest := versusclient.New(a.url), versusclient.New(b.url)
	must(t, host.NewPlayer(ctx))
	must(t, guest.NewPlayer(ctx))
	code, err := host.CreateRoom(ctx, []int{1})
	must(t, err)
	must(t, guest.Join(ctx, code))

	play, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	st := &versusbot.Stats{}
	room := &versusbot.Room{}
	bots := []*versusbot.Bot{
		{Name: "host", Host: true, Mode: "higher_lower", Names: names, Stats: st, Room: room, Think: 30 * time.Millisecond, S: host.Open(play, code)},
		{Name: "guest", Mode: "higher_lower", Names: names, Stats: st, Room: room, Think: 30 * time.Millisecond, S: guest.Open(play, code)},
	}
	var wg sync.WaitGroup
	for _, bot := range bots {
		wg.Go(func() { bot.Play(play) })
	}
	wg.Wait()

	if p := st.Problems(); len(p) > 0 {
		t.Errorf("problems: %v", p)
	}
	// Rounds counts each bot's: two each at least.
	if r := st.Rounds.Load(); r < 4 {
		t.Errorf("%d rounds between the two bots in 15s, want 4 or more", r)
	}
	if st.Moves.Load() == 0 || st.Answered.Load() == 0 || st.Reply.Len() == 0 || st.Broadcast.Len() == 0 {
		t.Errorf("moves %d, answered %d, reply samples %d, broadcast samples %d; want some of each",
			st.Moves.Load(), st.Answered.Load(), st.Reply.Len(), st.Broadcast.Len())
	}
	t.Logf("reply p50 %v, broadcast p50 %v, p99 %v", st.Reply.Quantile(0.5), st.Broadcast.Quantile(0.5), st.Broadcast.Quantile(0.99))

	// Each hub timed its fanouts (changes it heard about, sent on).
	for _, in := range []*instance{a, b} {
		if hs := in.hub.Stats(); hs.Sent == 0 || len(hs.Fanout) == 0 || hs.Notifications == 0 {
			t.Errorf("hub stats %+v; want sends, notifications and fanout times", hs)
		}
	}
}
