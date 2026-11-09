// Command versuscheck plays Versus against a running API with two bots, round
// after round, to see it through a deploy: start it, deploy, and see whether
// the bots carried on without losing anything.
//
//	go run ./cmd/versuscheck -api https://pokewordle-api.example.com -for 5m
//
// Every 10 seconds it prints what the bots have done; at the end, a summary,
// exiting 1 if anything went wrong (see internal/versusbot): a move never
// answered, an unexpected error, a room going back to an older version, or a
// bot losing the room. Reconnects aren't problems: a deploy causes them. It's
// how the bots come through them that counts.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/versusbot"
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

func run(ctx context.Context, api, mode string) error {
	setup, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	names, err := versusclient.PokemonNames(setup, api, 1)
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

	st := &versusbot.Stats{Log: log.Default()}
	room := &versusbot.Room{}
	bots := []*versusbot.Bot{
		{Name: "host", Host: true, Mode: mode, Names: names, Stats: st, Room: room, S: host.Open(ctx, code)},
		{Name: "guest", Mode: mode, Names: names, Stats: st, Room: room, S: guest.Open(ctx, code)},
	}
	var wg sync.WaitGroup
	for _, b := range bots {
		wg.Go(func() { b.Play(ctx) })
	}

	summary := func() string {
		return fmt.Sprintf("rounds %d, moves %d (answered %d), reconnects: host %d, guest %d",
			st.Rounds.Load()/2, st.Moves.Load(), st.Answered.Load(),
			bots[0].S.Reconnects.Load(), bots[1].S.Reconnects.Load())
	}
	report := time.NewTicker(10 * time.Second)
	defer report.Stop()
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
		if b.Unanswered() {
			st.Problem("%s: a move never answered", b.Name)
		}
	}
	log.Print("done: " + summary())
	if p := st.Problems(); len(p) > 0 {
		return fmt.Errorf("%d problem(s): %s", len(p), strings.Join(p, "; "))
	}
	log.Print("no problems")
	return nil
}
