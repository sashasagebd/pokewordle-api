package pokedex

import (
	"bytes"
	"strings"
	"testing"
)

func TestSeedIsValid(t *testing.T) {
	ps, err := Seed()
	if err != nil {
		t.Fatal(err)
	}
	// Spot checks against well-known entries.
	for _, want := range []struct {
		id    int
		name  string
		types string
	}{
		{1, "bulbasaur", "grass,poison"},
		{25, "pikachu", "electric"},
		{151, "mew", "psychic"},
		{1025, "pecharunt", "poison,ghost"},
	} {
		p := ps[want.id-1]
		if p.Name != want.name || strings.Join(p.Types, ",") != want.types {
			t.Errorf("#%d = %s %v, want %s %s", want.id, p.Name, p.Types, want.name, want.types)
		}
	}
}

func TestSeedFileIsCanonical(t *testing.T) {
	ps, err := Seed()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := Encode(ps)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc, bytes.ReplaceAll(seedJSON, []byte("\r\n"), []byte("\n"))) {
		t.Error("pokemon.json isn't in Encode's format; regenerate it with go run ./cmd/genseed")
	}
}

func TestValidateCatchesMistakes(t *testing.T) {
	good, err := Seed()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(ps []Pokemon) []Pokemon{
		"missing one":    func(ps []Pokemon) []Pokemon { return ps[:len(ps)-1] },
		"wrong gen":      func(ps []Pokemon) []Pokemon { ps[0].Generation = 2; return ps },
		"duplicate name": func(ps []Pokemon) []Pokemon { ps[1].Name = ps[0].Name; return ps },
		"unknown type":   func(ps []Pokemon) []Pokemon { ps[0].Types = []string{"sound"}; return ps },
		"no types":       func(ps []Pokemon) []Pokemon { ps[0].Types = nil; return ps },
		"zero weight":    func(ps []Pokemon) []Pokemon { ps[0].Weight = 0; return ps },
		"bad name":       func(ps []Pokemon) []Pokemon { ps[0].Name = "Bulbasaur"; return ps },
		"same type 2x":   func(ps []Pokemon) []Pokemon { ps[0].Types = []string{"grass", "grass"}; return ps },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ps := mutate(append([]Pokemon(nil), good...))
			if Validate(ps) == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestGenerationOf(t *testing.T) {
	for id, want := range map[int]int{0: 0, 1: 1, 151: 1, 152: 2, 905: 8, 906: 9, 1025: 9, 1026: 0} {
		if got := GenerationOf(id); got != want {
			t.Errorf("GenerationOf(%d) = %d, want %d", id, got, want)
		}
	}
}
