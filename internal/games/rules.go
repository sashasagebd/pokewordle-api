package games

import (
	"slices"

	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

// Directions in a Wordle comparison: where the answer is relative to the
// guess.
const (
	Higher = "higher"
	Lower  = "lower"
	Equal  = "equal"
)

// Type relations in a Wordle comparison, between the guess's first type and
// the answer's.
const (
	SameType  = "same"      // the same type
	Effective = "effective" // the guess's type is super effective against the answer's
	Weak      = "weak"      // the answer's type is super effective against the guess's
	Neutral   = "neutral"
)

// Stats Higher or Lower compares.
const (
	Height = "height"
	Weight = "weight"
)

// superEffective lists, for each type, the types it's super effective
// against. It's the table the frontend used before Phase 2 (Guesses.tsx),
// kept as it was so the game plays the same.
var superEffective = map[string][]string{
	"normal":   {},
	"fire":     {"grass", "ice", "bug", "steel"},
	"water":    {"fire", "ground", "rock"},
	"grass":    {"water", "ground", "rock"},
	"electric": {"water", "flying"},
	"ice":      {"grass", "ground", "flying", "dragon"},
	"fighting": {"normal", "ice", "rock", "dark", "steel"},
	"poison":   {"grass", "fairy"},
	"ground":   {"poison", "rock", "steel", "fire", "electric"},
	"flying":   {"fighting", "bug", "grass"},
	"psychic":  {"fighting", "poison"},
	"bug":      {"grass", "psychic", "dark"},
	"rock":     {"flying", "bug", "fire", "ice"},
	"ghost":    {"psychic"},
	"dragon":   {},
	"dark":     {"ghost", "psychic"},
	"steel":    {"rock", "ice", "fairy"},
	"fairy":    {"fighting", "dragon", "dark"},
}

// direction says where answer is relative to guess.
func direction(guess, answer int) string {
	switch {
	case answer > guess:
		return Higher
	case answer < guess:
		return Lower
	default:
		return Equal
	}
}

// typeRelation compares two types the way the Wordle type tile does.
func typeRelation(guess, answer string) string {
	switch {
	case guess == answer:
		return SameType
	case slices.Contains(superEffective[guess], answer):
		return Effective
	case slices.Contains(superEffective[answer], guess):
		return Weak
	default:
		return Neutral
	}
}

// A Comparison is one Wordle guess against the answer. Only the first types
// are compared, as before Phase 2.
type Comparison struct {
	Guess   pokedex.Pokemon `json:"guess"`
	Height  string          `json:"height"`
	Weight  string          `json:"weight"`
	Type    string          `json:"type"`
	Correct bool            `json:"correct"`
}

func compare(guess, answer pokedex.Pokemon) Comparison {
	return Comparison{
		Guess:   guess,
		Height:  direction(guess.Height, answer.Height),
		Weight:  direction(guess.Weight, answer.Weight),
		Type:    typeRelation(guess.Types[0], answer.Types[0]),
		Correct: guess.ID == answer.ID,
	}
}

// statOf returns p's height or weight.
func statOf(p pokedex.Pokemon, stat string) int {
	if stat == Height {
		return p.Height
	}
	return p.Weight
}

// higherLowerCorrect reports whether saying next's stat is dir ("higher" or
// "lower") than current's is right. A tie counts for either answer.
func higherLowerCorrect(current, next pokedex.Pokemon, stat, dir string) bool {
	c, n := statOf(current, stat), statOf(next, stat)
	if dir == Higher {
		return n >= c
	}
	return n <= c
}
