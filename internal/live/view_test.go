package live

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

const (
	alice = "player-alice-secret"
	bob   = "player-bob-secret"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// race is a room where alice (host) and bob are racing, alice one wrong
// guess in, with their games' states.
func race(t *testing.T) (*rooms.Room, map[string]games.State) {
	t.Helper()
	r := rooms.New("room-1", "K7QX2M", alice, "Alice", games.AllGens, t0)
	must(t, r.Connect(alice, t0))
	must(t, r.Join(bob, "Bob", t0))
	must(t, r.Connect(bob, t0))
	must(t, r.SetReady(bob, true, t0))
	must(t, r.Start(alice, 25, t0))
	play := t0.Add(rooms.Countdown)
	must(t, r.Guess(alice, false, play.Add(5*time.Second)))

	bulbasaur := pokedex.Pokemon{ID: 1, Name: "bulbasaur", Types: []string{"grass", "poison"}}
	states := map[string]games.State{
		alice: {ID: "game-a", Mode: games.Wordle, Status: games.Active, Score: 1,
			Guesses: []games.Comparison{{Guess: bulbasaur, Height: "higher", Weight: "lower", Type: "weak"}}},
		bob: {ID: "game-b", Mode: games.Wordle, Status: games.Active},
	}
	return r, states
}

func TestOthersSeeTilesNotGuesses(t *testing.T) {
	r, states := race(t)
	v := ViewFor(r, states, bob)

	if v.You != PublicID(r.ID, bob) || v.Host != PublicID(r.ID, alice) || v.Status != rooms.Playing {
		t.Errorf("you %s, host %s, status %s", v.You, v.Host, v.Status)
	}
	rv := v.Round
	if rv == nil || rv.FinishedAt != nil || rv.Answer != nil || rv.Winner != "" {
		t.Fatalf("round while it's on: %+v", rv)
	}
	if rv.Game == nil || rv.Game.ID != "game-b" {
		t.Errorf("bob's own game: %+v", rv.Game)
	}
	var a *PlayerView
	for i := range rv.Players {
		if rv.Players[i].ID == PublicID(r.ID, alice) {
			a = &rv.Players[i]
		}
	}
	if a == nil || !a.Racing || a.Guesses != 1 || len(a.Tiles) != 1 || a.Comparisons != nil || a.Name != "Alice" {
		t.Fatalf("alice as bob sees her: %+v", a)
	}
	if tile := a.Tiles[0]; tile.Height != "higher" || tile.Type != "weak" {
		t.Errorf("alice's tile: %+v", tile)
	}

	// Nothing secret in what's sent: no player IDs, no one else's guesses.
	data, err := json.Marshal(v)
	must(t, err)
	for _, secret := range []string{"secret", "bulbasaur", "game-a"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("bob's view contains %q: %s", secret, data)
		}
	}
}

func TestEverythingShowsOnceItsOver(t *testing.T) {
	r, states := race(t)
	must(t, r.Guess(bob, true, t0.Add(rooms.Countdown+time.Minute)))
	pikachu := pokedex.Pokemon{ID: 25, Name: "pikachu"}
	for _, p := range []string{alice, bob} {
		st := states[p]
		st.Answer = &pikachu
		states[p] = st
	}

	v := ViewFor(r, states, bob)
	rv := v.Round
	if v.Status != rooms.Lobby || rv.FinishedAt == nil || rv.Winner != PublicID(r.ID, bob) {
		t.Fatalf("after bob's win: %s, %+v", v.Status, rv)
	}
	if rv.Answer == nil || rv.Answer.Name != "pikachu" {
		t.Errorf("answer %+v", rv.Answer)
	}
	if first := rv.Players[0]; first.ID != v.You || first.Rank != 1 || first.TimeMs != time.Minute.Milliseconds() {
		t.Errorf("first place: %+v", first)
	}
	if second := rv.Players[1]; second.Racing || len(second.Comparisons) != 1 || second.Comparisons[0].Guess.Name != "bulbasaur" {
		t.Errorf("alice once it's over: %+v", second)
	}
}

func TestMembers(t *testing.T) {
	r, states := race(t)
	r.Disconnect(bob, t0.Add(time.Minute))
	v := ViewFor(r, states, alice)
	if len(v.Members) != 2 {
		t.Fatalf("%d members", len(v.Members))
	}
	b := v.Members[1]
	if b.Name != "Bob" || b.Connected || b.GraceEndsAt == nil || !b.GraceEndsAt.Equal(t0.Add(time.Minute+rooms.Grace)) {
		t.Errorf("bob disconnected: %+v", b)
	}
	if a := v.Members[0]; !a.Connected || a.GraceEndsAt != nil {
		t.Errorf("alice: %+v", a)
	}

	// Before any round, there's none to show, and no one is anyone else.
	lobby := rooms.New("room-2", "K7QX2N", alice, "Alice", games.AllGens, t0)
	if v := ViewFor(lobby, nil, alice); v.Round != nil || len(v.Members) != 1 {
		t.Errorf("new room: %+v", v)
	}
	if PublicID("room-1", alice) == PublicID("room-2", alice) {
		t.Error("a player has the same public ID in two rooms")
	}
}

func TestASurvivalRaceShowsStreaks(t *testing.T) {
	r := rooms.New("room-3", "K7QX2P", alice, "Alice", games.AllGens, t0)
	must(t, r.Connect(alice, t0))
	must(t, r.Join(bob, "Bob", t0))
	must(t, r.Connect(bob, t0))
	must(t, r.SetSettings(alice, games.WhosThat, games.AllGens, t0))
	must(t, r.SetReady(bob, true, t0))
	must(t, r.Start(alice, 25, t0))
	play := t0.Add(rooms.Countdown)
	must(t, r.Answer(alice, true, play.Add(time.Second)))
	must(t, r.Answer(alice, true, play.Add(2*time.Second)))

	states := map[string]games.State{
		alice: {ID: "game-a", Mode: games.WhosThat, Status: games.Active, Score: 2, Round: &games.PublicRound{N: 3}},
		bob:   {ID: "game-b", Mode: games.WhosThat, Status: games.Active, Round: &games.PublicRound{N: 1}},
	}
	v := ViewFor(r, states, bob)
	if v.Mode != games.WhosThat || v.Round.Mode != games.WhosThat {
		t.Errorf("modes %s, %s", v.Mode, v.Round.Mode)
	}
	if a := v.Round.Players[0]; a.ID != PublicID(r.ID, alice) || a.Score != 2 || !a.Racing || a.EndedOn != nil || len(a.Tiles) != 0 {
		t.Errorf("alice mid-round, as bob sees her: %+v", a)
	}

	// Bob misses: alice is ahead and alone, so she's won.
	must(t, r.Answer(bob, false, play.Add(3*time.Second)))
	missed := pokedex.Pokemon{ID: 25, Name: "pikachu"}
	onto := pokedex.Pokemon{ID: 6, Name: "charizard"}
	states[bob] = games.State{ID: "game-b", Mode: games.WhosThat, Status: games.Lost, Answer: &missed}
	states[alice] = games.State{ID: "game-a", Mode: games.WhosThat, Status: games.Lost, Score: 2, Answer: &onto}
	v = ViewFor(r, states, bob)
	if v.Round.FinishedAt == nil || v.Round.Winner != PublicID(r.ID, alice) || v.Round.Answer != nil {
		t.Fatalf("after bob's miss: %+v", v.Round)
	}
	if b := v.Round.Players[1]; b.Outcome != rooms.Out || b.EndedOn == nil || b.EndedOn.Name != "pikachu" {
		t.Errorf("bob in the results: %+v", b)
	}
}
