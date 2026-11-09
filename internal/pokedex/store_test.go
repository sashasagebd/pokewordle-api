package pokedex_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/dbtest"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

// TestLoadLeavesMatchingRowsUnlocked: a version starting up loads the
// Pokémon while the one before is still playing moves, which lock the
// Pokémon their rounds refer to. Load must not wait for them (or worse,
// deadlock with a move that wants a row it holds).
func TestLoadLeavesMatchingRowsUnlocked(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	s := pokedex.NewStore(pool)
	seed, err := pokedex.Seed()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, seed); err != nil {
		t.Fatal(err)
	}

	// Hold Pikachu as strongly as anything can.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM pokemon WHERE id = 25 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	loadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if n, err := s.Load(loadCtx, seed); err != nil || n != 0 {
		t.Fatalf("Load with a matching row locked elsewhere: %d changed, %v; want 0 without waiting", n, err)
	}
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	s := pokedex.NewStore(dbtest.Pool(t))
	seed, err := pokedex.Seed()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, seed); err != nil {
		t.Fatal(err)
	}
	// A second load changes nothing.
	if n, err := s.Load(ctx, seed); err != nil || n != 0 {
		t.Fatalf("second Load changed %d rows (err %v), want 0", n, err)
	}

	// nil (what the API passes without ?gens) and empty both mean all.
	for _, gens := range [][]int{nil, {}} {
		all, err := s.List(ctx, gens)
		if err != nil || len(all) != pokedex.Count {
			t.Fatalf("List(%#v) = %d Pokémon (err %v), want %d", gens, len(all), err, pokedex.Count)
		}
	}
	gen1and9, err := s.List(ctx, []int{1, 9})
	if err != nil {
		t.Fatal(err)
	}
	if want := 151 + 120; len(gen1and9) != want {
		t.Fatalf("List(1, 9) = %d Pokémon, want %d", len(gen1and9), want)
	}

	for _, key := range []string{"25", "pikachu", " Pikachu "} {
		p, err := s.Get(ctx, key)
		if err != nil || p.ID != 25 || p.Name != "pikachu" {
			t.Errorf("Get(%q) = %+v, %v; want pikachu", key, p, err)
		}
	}
	if _, err := s.Get(ctx, "missingno"); !errors.Is(err, pokedex.ErrNotFound) {
		t.Errorf("Get(missingno) err = %v, want ErrNotFound", err)
	}
}
