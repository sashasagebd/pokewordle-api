// Package games runs pokewordle's games on the server, which holds the
// answers, so a score is only ever what was actually played.
//
// The rules (rules.go, engine.go) are plain Go over the in-memory Pokédex,
// with no database, so they're tested without one. Store (store.go) loads a
// game, lets the engine play a move, and saves what changed.
package games

import (
	"errors"
	"math/bits"
	"slices"
	"strings"

	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

// A GenMask is a set of generations: bit n-1 is generation n.
type GenMask uint16

// AllGens is every generation.
var AllGens = GenMask(1)<<len(pokedex.Generations) - 1

// MaskOf returns the set of gens (1 to 9). No gens means all of them.
func MaskOf(gens []int) (GenMask, error) {
	if len(gens) == 0 {
		return AllGens, nil
	}
	var m GenMask
	for _, g := range gens {
		if g < 1 || g > len(pokedex.Generations) {
			return 0, errors.New("gens must be generation numbers from 1 to 9")
		}
		m |= 1 << (g - 1)
	}
	return m, nil
}

// Has reports whether generation gen is in m.
func (m GenMask) Has(gen int) bool { return gen >= 1 && m&(1<<(gen-1)) != 0 }

// Gens lists m's generations in order.
func (m GenMask) Gens() []int {
	gens := make([]int, 0, bits.OnesCount16(uint16(m)))
	for g := 1; g <= len(pokedex.Generations); g++ {
		if m.Has(g) {
			gens = append(gens, g)
		}
	}
	return gens
}

// Dex is the Pokémon data in memory, from the embedded seed. Games keep only
// dex numbers; Dex turns them back into Pokémon.
type Dex struct {
	byID   []pokedex.Pokemon // byID[id-1]
	byName map[string]int
}

// NewDex indexes ps, which must be a seed that passed pokedex.Validate.
func NewDex(ps []pokedex.Pokemon) *Dex {
	d := &Dex{byID: ps, byName: make(map[string]int, len(ps))}
	for _, p := range ps {
		d.byName[p.Name] = p.ID
	}
	return d
}

// Get returns Pokémon id. Ids come from games the engine made, so an unknown
// one is a bug, not bad input.
func (d *Dex) Get(id int) pokedex.Pokemon {
	return d.byID[id-1]
}

// Named finds a Pokémon by name, ignoring case and surrounding spaces.
func (d *Dex) Named(name string) (pokedex.Pokemon, bool) {
	id, ok := d.byName[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return pokedex.Pokemon{}, false
	}
	return d.byID[id-1], true
}

// random picks a Pokémon from m's generations other than the excluded ids,
// using intn (rand.IntN or a seeded one in tests). Every generation has far
// more Pokémon than a caller excludes, so it always finds one.
func (d *Dex) random(m GenMask, intn func(int) int, exclude ...int) int {
	var ranges []pokedex.Generation
	total := 0
	for _, g := range m.Gens() {
		r := pokedex.Generations[g-1]
		ranges = append(ranges, r)
		total += r.End - r.Start + 1
	}
	for {
		k := intn(total)
		for _, r := range ranges {
			if size := r.End - r.Start + 1; k >= size {
				k -= size
				continue
			}
			if id := r.Start + k; !slices.Contains(exclude, id) {
				return id
			}
			break
		}
	}
}
