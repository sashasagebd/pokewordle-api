// Command versusload plays many Versus rooms at once against a running API,
// with two bots in each, and measures how long moves take to get around:
//
//   - reply: a bot's move until its answer comes back
//
//   - broadcast: a bot's move until the other bot in the room sees it, what
//     an opponent waits for: commit, NOTIFY, the hub reloading the room,
//     the other socket
//
//     go run ./cmd/versusload -api http://localhost:8080 -rooms 50 -for 3m
//
// The API rate-limits by address (10 new players at once, 2 joins and tickets
// a second), so from one machine it can't make many players or open rooms
// quickly. It makes a few players (-players) and puts them in many rooms each
// (a player can be in any number), and opens the rooms as fast as the limits
// allow, waiting out 429s. Measuring starts once every room is open, so the
// ramp doesn't count.
//
// At the end it prints a Markdown table row for the README, and exits 1 if
// the bots ran into problems (see internal/versusbot).
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
	api := flag.String("api", "http://localhost:8080", "the API to load")
	rooms := flag.Int("rooms", 20, "how many rooms to play at once (two bots each)")
	players := flag.Int("players", 4, "how many players to make, shared between the rooms (at least 2)")
	mode := flag.String("mode", "higher_lower", "the mode: wordle, higher_lower or whos_that")
	length := flag.Duration("for", 3*time.Minute, "how long to measure, once every room is open")
	think := flag.Duration("think", time.Second, "each bot waits between this and twice it before each move")
	flag.Parse()
	if *players < 2 || *rooms < 1 {
		log.Fatal("it takes at least 2 players and 1 room")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, strings.TrimRight(*api, "/"), *rooms, *players, *mode, *length, *think); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, api string, nRooms, nPlayers int, mode string, length, think time.Duration) error {
	names, err := versusclient.PokemonNames(ctx, api, 1)
	if err != nil {
		return fmt.Errorf("loading Pokémon names: %w", err)
	}
	clients := make([]*versusclient.Client, nPlayers)
	for i := range clients {
		clients[i] = versusclient.New(api)
		if err := clients[i].NewPlayer(ctx); err != nil {
			return fmt.Errorf("making player %d: %w", i+1, err)
		}
	}

	st := &versusbot.Stats{Log: log.Default()}
	play, stopPlay := context.WithCancel(ctx)
	defer stopPlay()
	var wg sync.WaitGroup
	var bots []*versusbot.Bot

	log.Printf("opening %d rooms with %d players (rate limits make this take a while)", nRooms, nPlayers)
	ramp := time.Now()
	for i := range nRooms {
		host, guest := clients[i%nPlayers], clients[(i+1)%nPlayers]
		code, err := host.CreateRoom(ctx, []int{1})
		if err != nil {
			return fmt.Errorf("room %d: %w", i+1, err)
		}
		if err := guest.Join(ctx, code); err != nil {
			return fmt.Errorf("room %d: %w", i+1, err)
		}
		room := &versusbot.Room{}
		for _, b := range []*versusbot.Bot{
			{Name: fmt.Sprintf("room %d host", i+1), Host: true, Mode: mode, Names: names, Stats: st, Room: room, Think: think, S: host.Open(play, code)},
			{Name: fmt.Sprintf("room %d guest", i+1), Mode: mode, Names: names, Stats: st, Room: room, Think: think, S: guest.Open(play, code)},
		} {
			bots = append(bots, b)
			wg.Go(func() { b.Play(play) })
		}
		if (i+1)%10 == 0 {
			log.Printf("%d rooms open", i+1)
		}
	}
	// Every socket connected (it has had a room), before the clock starts.
	// Polled: the bot is reading the socket's messages.
	for _, b := range bots {
		for {
			if _, seq := b.S.Room(); seq > 0 {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	log.Printf("all %d rooms open in %s; measuring for %s", nRooms, time.Since(ramp).Round(time.Second), length)

	st.Reply.Reset()
	st.Broadcast.Reset()
	moves0 := st.Moves.Load()
	reconnects := func() int64 {
		var n int64
		for _, b := range bots {
			n += b.S.Reconnects.Load()
		}
		return n
	}
	reconnects0 := reconnects()
	started := time.Now()
	line := func() string {
		secs := time.Since(started).Seconds()
		return fmt.Sprintf("%.1f moves/s; reply p50 %s, p99 %s; broadcast p50 %s, p99 %s; reconnects %d; problems %d",
			float64(st.Moves.Load()-moves0)/secs,
			ms(st.Reply.Quantile(0.5)), ms(st.Reply.Quantile(0.99)),
			ms(st.Broadcast.Quantile(0.5)), ms(st.Broadcast.Quantile(0.99)),
			reconnects()-reconnects0, len(st.Problems()))
	}

	measure, cancel := context.WithTimeout(ctx, length)
	defer cancel()
	report := time.NewTicker(10 * time.Second)
	defer report.Stop()
loop:
	for {
		select {
		case <-measure.Done():
			break loop
		case <-report.C:
			log.Print(line())
		}
	}
	secs := time.Since(started).Seconds()
	movesPerSec := float64(st.Moves.Load()-moves0) / secs
	stopPlay()
	wg.Wait()
	log.Print("done: " + line())

	fmt.Println()
	fmt.Printf("%s, %s, %d rooms (%d sockets), think %s, %s:\n\n", time.Now().Format("2006-01-02"), mode, nRooms, 2*nRooms, think, length)
	fmt.Println("| Rooms | Sockets | Moves/s | Reply p50 | p95 | p99 | Broadcast p50 | p95 | p99 | max | Samples |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|")
	fmt.Printf("| %d | %d | %.1f | %s | %s | %s | %s | %s | %s | %s | %d |\n",
		nRooms, 2*nRooms, movesPerSec,
		ms(st.Reply.Quantile(0.5)), ms(st.Reply.Quantile(0.95)), ms(st.Reply.Quantile(0.99)),
		ms(st.Broadcast.Quantile(0.5)), ms(st.Broadcast.Quantile(0.95)), ms(st.Broadcast.Quantile(0.99)),
		ms(st.Broadcast.Quantile(1)), st.Broadcast.Len())
	fmt.Println()

	if p := st.Problems(); len(p) > 0 {
		return fmt.Errorf("%d problem(s), the first: %s", len(p), p[0])
	}
	return nil
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.0f ms", float64(d)/float64(time.Millisecond))
}
