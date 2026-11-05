// Package pokedex holds the Pokémon reference data: the seed file embedded in
// the binary, loading it into Postgres, and reading it back.
package pokedex

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
)

// Pokemon is one Pokémon as the API serves it. Height and weight keep
// PokéAPI's units (decimetres and hectograms), which the frontend already
// converts.
type Pokemon struct {
	ID         int      `json:"id"`
	Name       string   `json:"name"`
	Generation int      `json:"generation"`
	Height     int      `json:"height"`
	Weight     int      `json:"weight"`
	Types      []string `json:"types"`
}

// Generation is a range of national dex numbers. The ranges match
// pokewordle's src/generations.ts.
type Generation struct{ Start, End int }

// Generations lists generations 1 to 9 in order (Generations[0] is gen 1).
var Generations = []Generation{
	{1, 151}, {152, 251}, {252, 386}, {387, 493}, {494, 649},
	{650, 721}, {722, 809}, {810, 905}, {906, 1025},
}

// Count is the number of Pokémon in the seed: the last of the last generation.
var Count = Generations[len(Generations)-1].End

// GenerationOf returns the generation (1 to 9) of dex number id, or 0 if it's
// out of range.
func GenerationOf(id int) int {
	for i, g := range Generations {
		if id >= g.Start && id <= g.End {
			return i + 1
		}
	}
	return 0
}

// Types are the 18 types a Pokémon can have.
var Types = []string{
	"normal", "fire", "water", "grass", "electric", "ice", "fighting", "poison", "ground",
	"flying", "psychic", "bug", "rock", "ghost", "dragon", "dark", "steel", "fairy",
}

//go:embed pokemon.json
var seedJSON []byte

// Seed returns the embedded seed data, checked by Validate.
func Seed() ([]Pokemon, error) {
	var ps []Pokemon
	dec := json.NewDecoder(bytes.NewReader(seedJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ps); err != nil {
		return nil, fmt.Errorf("pokemon.json: %w", err)
	}
	if err := Validate(ps); err != nil {
		return nil, fmt.Errorf("pokemon.json: %w", err)
	}
	return ps, nil
}

var validName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Validate checks that ps is a complete, sensible seed: ids 1 to Count in
// order, unique well-formed names, generations matching the ranges, and one or
// two known types.
func Validate(ps []Pokemon) error {
	if len(ps) != Count {
		return fmt.Errorf("has %d Pokémon, want %d", len(ps), Count)
	}
	names := make(map[string]bool, len(ps))
	for i, p := range ps {
		if p.ID != i+1 {
			return fmt.Errorf("entry %d has id %d, want %d (ids must be 1 to %d in order)", i, p.ID, i+1, Count)
		}
		if !validName.MatchString(p.Name) {
			return fmt.Errorf("#%d: bad name %q", p.ID, p.Name)
		}
		if names[p.Name] {
			return fmt.Errorf("#%d: duplicate name %q", p.ID, p.Name)
		}
		names[p.Name] = true
		if want := GenerationOf(p.ID); p.Generation != want {
			return fmt.Errorf("#%d %s: generation %d, want %d", p.ID, p.Name, p.Generation, want)
		}
		if p.Height <= 0 || p.Weight <= 0 {
			return fmt.Errorf("#%d %s: height and weight must be positive", p.ID, p.Name)
		}
		if len(p.Types) < 1 || len(p.Types) > 2 {
			return fmt.Errorf("#%d %s: has %d types, want 1 or 2", p.ID, p.Name, len(p.Types))
		}
		for _, t := range p.Types {
			if !slices.Contains(Types, t) {
				return fmt.Errorf("#%d %s: unknown type %q", p.ID, p.Name, t)
			}
		}
		if len(p.Types) == 2 && p.Types[0] == p.Types[1] {
			return fmt.Errorf("#%d %s: the same type twice", p.ID, p.Name)
		}
	}
	return nil
}

// Encode writes ps as the seed file's JSON: one Pokémon per line, so a change
// to one shows as a one-line diff.
func Encode(ps []Pokemon) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("[\n")
	for i, p := range ps {
		line, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		b.WriteString("  ")
		b.Write(line)
		if i < len(ps)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]\n")
	return b.Bytes(), nil
}
