package games_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/games"
)

// named makes a player with a fresh username and returns its id and name.
func (f *fixture) named(t *testing.T) (string, string) {
	t.Helper()
	player := f.player(t)
	b := make([]byte, 6)
	rand.Read(b)
	name := "t" + hex.EncodeToString(b)
	if err := auth.NewStore(f.pool).Claim(f.ctx, player, name, "pikachu123"); err != nil {
		t.Fatal(err)
	}
	return player, name
}

// setBest stores a best directly, achieved ago, against a new game of the
// player's.
func (f *fixture) setBest(t *testing.T, player string, mode games.Mode, mask games.GenMask, best int, ago time.Duration) string {
	t.Helper()
	st, err := f.store.Start(f.ctx, player, mode, mask)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO best_scores (player_id, mode, gen_mask, best, game_id, achieved_at)
		VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6)`,
		player, string(mode), int16(mask), best, st.ID, time.Now().Add(-ago)); err != nil {
		t.Fatal(err)
	}
	return st.ID
}

func bestOf(t *testing.T, f *fixture, player string, mode games.Mode, mask games.GenMask) (games.Best, bool) {
	t.Helper()
	bests, err := f.store.Bests(f.ctx, player)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bests {
		m, _ := games.MaskOf(b.Gens)
		if b.Mode == mode && m == mask {
			return b, true
		}
	}
	return games.Best{}, false
}

func TestBestsAreRecordedAndOnlyImprove(t *testing.T) {
	f := setup(t)
	player := f.player(t)
	all := games.AllGens

	// A streak counts as it grows, not only once it ends.
	st, err := f.store.Start(f.ctx, player, games.WhosThat, all)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		if _, _, err := f.store.Answer(f.ctx, player, st.ID, games.Answer{N: n, Name: f.answerOf(t, st.ID, n).Name}); err != nil {
			t.Fatal(err)
		}
	}
	if b, ok := bestOf(t, f, player, games.WhosThat, all); !ok || b.Best != 2 || b.GameID != st.ID {
		t.Fatalf("after a streak of 2 still going: %+v (found %v)", b, ok)
	}

	// A shorter streak later doesn't replace it.
	short, err := f.store.Start(f.ctx, player, games.WhosThat, all)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.Answer(f.ctx, player, short.ID, games.Answer{N: 1, Name: f.answerOf(t, short.ID, 1).Name}); err != nil {
		t.Fatal(err)
	}
	if b, _ := bestOf(t, f, player, games.WhosThat, all); b.Best != 2 || b.GameID != st.ID {
		t.Errorf("a streak of 1 replaced the 2: %+v", b)
	}

	// Wordle: fewer guesses is better, and only a win counts.
	win := func(wrongFirst bool) string {
		g, err := f.store.Start(f.ctx, player, games.Wordle, all)
		if err != nil {
			t.Fatal(err)
		}
		answer := f.answerOf(t, g.ID, 1)
		if wrongFirst {
			wrong := "bulbasaur"
			if answer.Name == wrong {
				wrong = "ivysaur"
			}
			if _, _, err := f.store.Guess(f.ctx, player, g.ID, wrong); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := f.store.Guess(f.ctx, player, g.ID, answer.Name); err != nil {
			t.Fatal(err)
		}
		return g.ID
	}
	lost, err := f.store.Start(f.ctx, player, games.Wordle, all)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.GiveUp(f.ctx, player, lost.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := bestOf(t, f, player, games.Wordle, all); ok {
		t.Fatal("a given-up Wordle counted")
	}
	win(true)
	if b, _ := bestOf(t, f, player, games.Wordle, all); b.Best != 2 {
		t.Fatalf("Wordle in 2: best %+v", b)
	}
	one := win(false)
	if b, _ := bestOf(t, f, player, games.Wordle, all); b.Best != 1 || b.GameID != one {
		t.Fatalf("Wordle in 1 after 2: best %+v", b)
	}
	win(true)
	if b, _ := bestOf(t, f, player, games.Wordle, all); b.Best != 1 {
		t.Errorf("Wordle in 2 replaced the 1: %+v", b)
	}
}

func TestLeaderboard(t *testing.T) {
	f := setup(t)
	mask, _ := games.MaskOf([]int{4, 7}) // few other tests use this board
	a, aName := f.named(t)
	b, bName := f.named(t)
	c, cName := f.named(t)
	guest := f.player(t)

	f.setBest(t, a, games.WhosThat, mask, 5, 1*time.Minute)
	f.setBest(t, b, games.WhosThat, mask, 7, 2*time.Minute)
	f.setBest(t, c, games.WhosThat, mask, 5, 3*time.Minute) // a's score, reached earlier
	f.setBest(t, guest, games.WhosThat, mask, 9, 0)
	f.setBest(t, a, games.Wordle, mask, 3, 0)
	f.setBest(t, b, games.Wordle, mask, 2, 0)

	ranks := func(mode games.Mode, player string) (map[string]int, *games.MyEntry) {
		t.Helper()
		board, err := f.store.Leaderboard(f.ctx, mode, mask, 100, player)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, e := range board.Entries {
			got[e.Username] = e.Rank
		}
		return got, board.Me
	}

	got, me := ranks(games.WhosThat, a)
	if !(got[bName] < got[cName] && got[cName] < got[aName]) {
		t.Errorf("Who's That ranks %v: want %s (7) above %s (5, earlier) above %s (5)", got, bName, cName, aName)
	}
	if len(got) < 3 {
		t.Fatalf("board is missing players: %v", got)
	}
	if me == nil || me.Rank == nil || *me.Rank != got[aName] || me.Best != 5 {
		t.Errorf("a's own place = %+v, want rank %d with 5", me, got[aName])
	}

	// A guest's best counts for them but isn't on the board.
	_, me = ranks(games.WhosThat, guest)
	if me == nil || me.Rank != nil || me.Best != 9 {
		t.Errorf("guest's own place = %+v, want no rank and 9", me)
	}
	if board, _ := f.store.Leaderboard(f.ctx, games.WhosThat, mask, 100, ""); board.Me != nil {
		t.Error("an anonymous request got a place of its own")
	}
	// No best on this board: no place.
	if _, me := ranks(games.HigherLower, a); me != nil {
		t.Errorf("a's place on a board they haven't played = %+v", me)
	}

	got, _ = ranks(games.Wordle, "")
	if !(got[bName] < got[aName]) {
		t.Errorf("Wordle ranks %v: want %s (2 guesses) above %s (3)", got, bName, aName)
	}
}

func TestMergeGuestIntoAccount(t *testing.T) {
	f := setup(t)
	mask, _ := games.MaskOf([]int{2, 5})
	account, _ := f.named(t)
	guest := f.player(t)

	f.setBest(t, account, games.WhosThat, mask, 3, time.Hour)
	f.setBest(t, account, games.Wordle, mask, 2, time.Hour)
	guestStreak := f.setBest(t, guest, games.WhosThat, mask, 5, 0) // better: it wins
	f.setBest(t, guest, games.Wordle, mask, 4, 0)                  // worse: it loses
	guestHL := f.setBest(t, guest, games.HigherLower, mask, 1, 0)  // new to the account

	if err := f.store.Merge(f.ctx, guest, account); err != nil {
		t.Fatal(err)
	}
	if b, _ := bestOf(t, f, account, games.WhosThat, mask); b.Best != 5 || b.GameID != guestStreak {
		t.Errorf("Who's That after merging: %+v, want the guest's 5", b)
	}
	if b, _ := bestOf(t, f, account, games.Wordle, mask); b.Best != 2 {
		t.Errorf("Wordle after merging: %+v, want the account's 2", b)
	}
	if b, ok := bestOf(t, f, account, games.HigherLower, mask); !ok || b.Best != 1 {
		t.Errorf("Higher or Lower after merging: %+v (%v), want the guest's 1", b, ok)
	}
	// The guest's games are the account's now, and the guest is gone.
	if _, err := f.store.Get(f.ctx, account, guestHL); err != nil {
		t.Errorf("the guest's game isn't the account's: %v", err)
	}
	var left int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM players WHERE id = $1::uuid`, guest).Scan(&left); err != nil || left != 0 {
		t.Errorf("guest still exists (%d, %v)", left, err)
	}

	// Merging never takes from a named player.
	other, _ := f.named(t)
	otherGame := f.setBest(t, other, games.WhosThat, mask, 50, 0)
	if err := f.store.Merge(f.ctx, other, account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Get(f.ctx, other, otherGame); err != nil {
		t.Errorf("a named player's game moved: %v", err)
	}
	if b, _ := bestOf(t, f, account, games.WhosThat, mask); b.Best != 5 {
		t.Errorf("a named player's best merged in: %+v", b)
	}
	if _, err := f.store.Get(f.ctx, account, otherGame); !errors.Is(err, games.ErrNotFound) {
		t.Errorf("account can see the other player's game: %v", err)
	}
}
