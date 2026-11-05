package games_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/dbtest"
	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

type fixture struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	store *games.Store
	dex   *games.Dex
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	seed, err := pokedex.Seed()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pokedex.NewStore(pool).Load(ctx, seed); err != nil {
		t.Fatal(err)
	}
	dex := games.NewDex(seed)
	return &fixture{ctx, pool, games.NewStore(pool, &games.Engine{Dex: dex}), dex}
}

func (f *fixture) player(t *testing.T) string {
	t.Helper()
	_, id, err := auth.NewStore(f.pool).NewPlayer(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// answerOf peeks at a round's answer, as only a test may.
func (f *fixture) answerOf(t *testing.T, game string, n int) pokedex.Pokemon {
	t.Helper()
	var id int
	if err := f.pool.QueryRow(f.ctx, `SELECT answer_id FROM game_rounds WHERE game_id = $1::uuid AND n = $2`, game, n).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return f.dex.Get(id)
}

func TestWordleInTheDatabase(t *testing.T) {
	f := setup(t)
	player := f.player(t)
	st, err := f.store.Start(f.ctx, player, games.Wordle, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}
	answer := f.answerOf(t, st.ID, 1)
	wrong := "bulbasaur"
	if answer.Name == wrong {
		wrong = "ivysaur"
	}

	if _, _, err := f.store.Guess(f.ctx, player, st.ID, wrong); err != nil {
		t.Fatal(err)
	}
	c, st, err := f.store.Guess(f.ctx, player, st.ID, answer.Name)
	if err != nil || !c.Correct || st.Status != games.Won || st.Score != 2 || st.Answer.ID != answer.ID {
		t.Fatalf("winning guess: %+v, %+v, %v", c, st, err)
	}

	// It all reads back the same.
	got, err := f.store.Get(f.ctx, player, st.ID)
	if err != nil || got.Status != games.Won || len(got.Guesses) != 2 || got.Guesses[0].Guess.Name != wrong {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, _, err := f.store.Guess(f.ctx, player, st.ID, "pikachu"); !errors.Is(err, games.ErrOver) {
		t.Errorf("guess after winning: err = %v, want ErrOver", err)
	}
}

func TestStreakInTheDatabase(t *testing.T) {
	f := setup(t)
	player := f.player(t)
	st, err := f.store.Start(f.ctx, player, games.WhosThat, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		res, next, err := f.store.Answer(f.ctx, player, st.ID, games.Answer{N: n, Name: f.answerOf(t, st.ID, n).Name})
		if err != nil || !res.Correct || next.Score != n || next.Round.N != n+1 {
			t.Fatalf("round %d: %+v, %+v, %v", n, res, next, err)
		}
	}
	// Round 5 was made in advance (for preloading); round 6 wasn't.
	if _, err := f.store.SilhouetteOf(f.ctx, player, st.ID, 5); err != nil {
		t.Errorf("silhouette of the next round: %v", err)
	}
	if _, err := f.store.SilhouetteOf(f.ctx, player, st.ID, 6); !errors.Is(err, games.ErrNotFound) {
		t.Errorf("silhouette two rounds ahead: err = %v, want ErrNotFound", err)
	}
	if _, _, err := f.store.Answer(f.ctx, player, st.ID, games.Answer{N: 3, Name: "pikachu"}); !errors.Is(err, games.ErrWrongRound) {
		t.Errorf("answering round 3 again: err = %v, want ErrWrongRound", err)
	}

	st, err = f.store.GiveUp(f.ctx, player, st.ID)
	if err != nil || st.Status != games.Lost || st.Score != 3 || st.Answer.ID != f.answerOf(t, st.ID, 4).ID {
		t.Fatalf("give up: %+v, %v", st, err)
	}
}

func TestSameRoundAnsweredTwiceAtOnce(t *testing.T) {
	f := setup(t)
	player := f.player(t)
	st, err := f.store.Start(f.ctx, player, games.WhosThat, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}
	right := games.Answer{N: 1, Name: f.answerOf(t, st.ID, 1).Name}

	const tries = 8
	errs := make([]error, tries)
	var wg sync.WaitGroup
	for i := range tries {
		wg.Go(func() { _, _, errs[i] = f.store.Answer(f.ctx, player, st.ID, right) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, games.ErrWrongRound):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d identical answers counted, want exactly 1", ok, tries)
	}
	if got, err := f.store.Get(f.ctx, player, st.ID); err != nil || got.Score != 1 {
		t.Fatalf("score after the race: %+v, %v", got, err)
	}
}

func TestGamesBelongToTheirPlayer(t *testing.T) {
	f := setup(t)
	alice, bob := f.player(t), f.player(t)
	st, err := f.store.Start(f.ctx, alice, games.HigherLower, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Get(f.ctx, bob, st.ID); !errors.Is(err, games.ErrNotFound) {
		t.Errorf("bob reading alice's game: err = %v, want ErrNotFound", err)
	}
	if _, err := f.store.GiveUp(f.ctx, bob, st.ID); !errors.Is(err, games.ErrNotFound) {
		t.Errorf("bob giving up alice's game: err = %v, want ErrNotFound", err)
	}
	if _, err := f.store.Get(f.ctx, alice, "not-a-uuid"); !errors.Is(err, games.ErrNotFound) {
		t.Errorf("bad id: err = %v, want ErrNotFound", err)
	}
	// Higher or Lower has no silhouettes.
	if _, err := f.store.SilhouetteOf(f.ctx, alice, st.ID, 1); !errors.Is(err, games.ErrNotFound) {
		t.Errorf("silhouette of a Higher or Lower round: err = %v, want ErrNotFound", err)
	}

	// A new game of the same mode abandons the old one; other modes are left alone.
	wordle, err := f.store.Start(f.ctx, alice, games.Wordle, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Start(f.ctx, alice, games.HigherLower, games.AllGens); err != nil {
		t.Fatal(err)
	}
	if old, err := f.store.Get(f.ctx, alice, st.ID); err != nil || old.Status != games.Abandoned || old.Answer == nil {
		t.Errorf("replaced game: %+v, %v", old, err)
	}
	if w, err := f.store.Get(f.ctx, alice, wordle.ID); err != nil || w.Status != games.Active {
		t.Errorf("other mode's game: %+v, %v", w, err)
	}

	// Deleting a player takes their games with them.
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM players WHERE id = $1::uuid`, alice); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM game_rounds WHERE game_id = $1::uuid`, st.ID).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rounds left after deleting the player (%v)", left, err)
	}
}
