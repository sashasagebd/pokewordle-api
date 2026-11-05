package pokedex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sashasagebd/pokewordle-api/internal/dbtest"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

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

	all, err := s.List(ctx, nil)
	if err != nil || len(all) != pokedex.Count {
		t.Fatalf("List(all) = %d Pokémon (err %v), want %d", len(all), err, pokedex.Count)
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
