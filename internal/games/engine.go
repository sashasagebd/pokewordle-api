package games

import (
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

// A Mode is one of the three games.
type Mode string

const (
	// Wordle: guess the mystery Pokémon, with hints on height, weight and
	// type. Unlimited guesses; fewer is better.
	Wordle Mode = "wordle"
	// HigherLower: is the next Pokémon's height or weight higher or lower?
	// A streak: one wrong answer ends it.
	HigherLower Mode = "higher_lower"
	// WhosThat: name the Pokémon from its silhouette. A streak.
	WhosThat Mode = "whos_that"
)

// ParseMode checks that s names a mode.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case Wordle, HigherLower, WhosThat:
		return m, nil
	}
	return "", errors.New(`mode must be "wordle", "higher_lower" or "whos_that"`)
}

// A Status is where a game is.
type Status string

const (
	Active    Status = "active"
	Won       Status = "won"       // Wordle only: found the answer
	Lost      Status = "lost"      // a wrong answer in a streak mode, or giving up
	Abandoned Status = "abandoned" // replaced by a new game of the same mode
)

// Errors a move can fail with. An *InputError is the player's mistake and
// doesn't count as a move.
var (
	ErrNotFound   = errors.New("no such game")
	ErrOver       = errors.New("this game is over")
	ErrWrongRound = errors.New("that round has already been answered")
	ErrRoomGame   = errors.New("this game is played in its Versus room")
)

// An InputError is a move the game can't take, like an unknown Pokémon. Its
// message is meant for the player.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func inputErr(format string, a ...any) error { return &InputError{fmt.Sprintf(format, a...)} }

// A Game is the whole game, secrets included. It never leaves the server;
// players see State.
type Game struct {
	ID     string
	Mode   Mode
	Mask   GenMask
	Status Status
	// Score: the number of guesses in Wordle, the streak in the others.
	Score  int
	Rounds []Round // Rounds[i].N == i+1
	// RoomRound is the Versus round this game is a player's race in, or ""
	// for a solo game. A room game is only played through its room, and
	// never counts toward best scores.
	RoomRound string
	// Seed, when not 0, is what the game's Pokémon are drawn from: round k's
	// from the seed and k alone (and the one before it, never repeated). So
	// in a Versus round, every player meets the same Pokémon in the same
	// order, however fast each goes.
	Seed int64
}

// A Round is one thing to answer.
//
//   - Wordle: one per guess, all with the same AnswerID. The last is open
//     until the game ends.
//   - HigherLower: OtherID is the Pokémon shown with its values, AnswerID
//     the one whose value is hidden.
//   - WhosThat: AnswerID is the Pokémon in the silhouette.
//
// In the streak modes the round after the open one already exists, so the
// player can preload it: HigherLower shows its Pokémon anyway, and WhosThat
// serves only its silhouette.
type Round struct {
	N        int
	AnswerID int
	OtherID  int    // 0 = none
	Stat     string // HigherLower: the stat answered on ("height" or "weight")
	GuessID  int    // Wordle, WhosThat: the Pokémon guessed
	GuessDir string // HigherLower: "higher" or "lower"
	Correct  bool
	Answered bool
}

// open returns the round being played, or nil if there's none (the game is
// over, or a streak game hasn't been set up).
func (g *Game) open() *Round {
	for i := range g.Rounds {
		if !g.Rounds[i].Answered {
			return &g.Rounds[i]
		}
	}
	return nil
}

// final returns the round a finished game ended on: the answer to reveal.
func (g *Game) final() *Round {
	if g.Mode == Wordle {
		return &g.Rounds[len(g.Rounds)-1]
	}
	// The first round not answered correctly: the wrong answer, or the round
	// open when the player gave up.
	for i := range g.Rounds {
		if r := &g.Rounds[i]; !r.Answered || !r.Correct {
			return r
		}
	}
	return &g.Rounds[len(g.Rounds)-1]
}

// Engine plays games over a Dex.
type Engine struct {
	Dex *Dex
	// Intn picks random numbers (rand.IntN when nil). Tests seed it.
	Intn func(n int) int
}

func (e *Engine) intn(n int) int {
	if e.Intn != nil {
		return e.Intn(n)
	}
	return rand.IntN(n)
}

// New sets up a game (without an ID: the store gives it one).
func (e *Engine) New(mode Mode, mask GenMask) *Game { return e.NewSeeded(mode, mask, 0) }

// NewSeeded sets up a game whose Pokémon are drawn from seed (see
// Game.Seed); 0 means at random, as New.
func (e *Engine) NewSeeded(mode Mode, mask GenMask, seed int64) *Game {
	g := &Game{Mode: mode, Mask: mask, Status: Active, Seed: seed}
	switch mode {
	case Wordle:
		g.Rounds = []Round{{N: 1, AnswerID: e.draw(g, 1)}}
	case HigherLower:
		current := e.draw(g, 0)
		g.Rounds = []Round{{N: 1, OtherID: current, AnswerID: e.draw(g, 1, current)}}
		e.addRound(g)
	case WhosThat:
		g.Rounds = []Round{{N: 1, AnswerID: e.draw(g, 1)}}
		e.addRound(g)
	}
	return g
}

// draw picks the Pokémon for round k of g (other than the excluded ones):
// from g's seed and k when it has one, otherwise at random.
func (e *Engine) draw(g *Game, k int, exclude ...int) int {
	intn := e.intn
	if g.Seed != 0 {
		intn = rand.New(rand.NewPCG(uint64(g.Seed), uint64(k))).IntN
	}
	return e.Dex.random(g.Mask, intn, exclude...)
}

// addRound adds the next streak round. It never repeats the Pokémon just
// before it.
func (e *Engine) addRound(g *Game) {
	last := g.Rounds[len(g.Rounds)-1]
	r := Round{N: last.N + 1}
	if g.Mode == HigherLower {
		r.OtherID = last.AnswerID // the hidden Pokémon becomes the shown one
	}
	r.AnswerID = e.draw(g, r.N, last.AnswerID)
	g.Rounds = append(g.Rounds, r)
}

// inGame finds the Pokémon called name among the game's generations.
func (e *Engine) inGame(g *Game, name string) (pokedex.Pokemon, error) {
	p, ok := e.Dex.Named(name)
	if !ok {
		return p, inputErr("No Pokémon called %q.", name)
	}
	if !g.Mask.Has(p.Generation) {
		return p, inputErr("%s isn't in the selected generations.", p.Name)
	}
	return p, nil
}

// Guess plays a Wordle guess and returns the round it answered.
func (e *Engine) Guess(g *Game, name string) (*Round, Comparison, error) {
	if g.Mode != Wordle {
		return nil, Comparison{}, inputErr("Only Wordle takes guesses; this game takes answers.")
	}
	if g.Status != Active {
		return nil, Comparison{}, ErrOver
	}
	p, err := e.inGame(g, name)
	if err != nil {
		return nil, Comparison{}, err
	}
	for _, r := range g.Rounds {
		if r.Answered && r.GuessID == p.ID {
			return nil, Comparison{}, inputErr("You already guessed %s.", p.Name)
		}
	}
	r := g.open()
	c := compare(p, e.Dex.Get(r.AnswerID))
	r.GuessID, r.Correct, r.Answered = p.ID, c.Correct, true
	n := r.N
	g.Score = n
	if c.Correct {
		g.Status = Won
	} else {
		g.Rounds = append(g.Rounds, Round{N: n + 1, AnswerID: r.AnswerID})
	}
	return &g.Rounds[n-1], c, nil // not r: the append may have moved it
}

// An Answer is a streak-mode move. N is the round it answers, so a request
// sent twice can't answer the next round by accident.
type Answer struct {
	N    int    `json:"n"`
	Dir  string `json:"dir,omitempty"`  // HigherLower: "higher" or "lower"
	Stat string `json:"stat,omitempty"` // HigherLower: "height" or "weight"
	Name string `json:"name,omitempty"` // WhosThat
}

// AnswerResult is what an answer revealed.
type AnswerResult struct {
	Correct bool            `json:"correct"`
	Answer  pokedex.Pokemon `json:"answer"` // with the hidden value, or the Pokémon in the silhouette
}

// Answer plays a streak-mode answer and returns the round it answered.
func (e *Engine) Answer(g *Game, a Answer) (*Round, AnswerResult, error) {
	if g.Mode == Wordle {
		return nil, AnswerResult{}, inputErr("Wordle takes guesses, not answers.")
	}
	if g.Status != Active {
		return nil, AnswerResult{}, ErrOver
	}
	r := g.open()
	if a.N != r.N {
		return nil, AnswerResult{}, ErrWrongRound
	}
	answer := e.Dex.Get(r.AnswerID)
	switch g.Mode {
	case HigherLower:
		if a.Dir != Higher && a.Dir != Lower {
			return nil, AnswerResult{}, inputErr(`dir must be "higher" or "lower".`)
		}
		if a.Stat != Height && a.Stat != Weight {
			return nil, AnswerResult{}, inputErr(`stat must be "height" or "weight".`)
		}
		r.Stat, r.GuessDir = a.Stat, a.Dir
		r.Correct = higherLowerCorrect(e.Dex.Get(r.OtherID), answer, a.Stat, a.Dir)
	case WhosThat:
		p, err := e.inGame(g, a.Name)
		if err != nil {
			return nil, AnswerResult{}, err
		}
		r.GuessID, r.Correct = p.ID, p.ID == answer.ID
	}
	r.Answered = true
	n := r.N
	if r.Correct {
		g.Score++
		e.addRound(g)
	} else {
		g.Status = Lost
	}
	return &g.Rounds[n-1], AnswerResult{Correct: g.Rounds[n-1].Correct, Answer: answer}, nil
}

// GiveUp ends the game as lost, keeping its score.
func (e *Engine) GiveUp(g *Game) error {
	if g.Status != Active {
		return ErrOver
	}
	g.Status = Lost
	return nil
}

// State is what the player sees of a game: never an answer they haven't
// earned.
type State struct {
	ID     string `json:"id"`
	Mode   Mode   `json:"mode"`
	Gens   []int  `json:"gens"`
	Status Status `json:"status"`
	Score  int    `json:"score"`
	// Round is the round to play, in the streak modes while the game is on.
	Round *PublicRound `json:"round,omitempty"`
	// Guesses are Wordle's guesses so far, oldest first.
	Guesses []Comparison `json:"guesses,omitempty"`
	// Answer is revealed once the game is over.
	Answer *pokedex.Pokemon `json:"answer,omitempty"`
}

// PublicRound is the visible part of a streak round. WhosThat shows only N:
// its silhouette is at GET /games/{id}/rounds/{n}/image, and so is the next
// round's, for preloading.
type PublicRound struct {
	N int `json:"n"`
	// HigherLower: Current is shown with its values; Next only by name.
	// Upcoming is the Pokémon after Next, for preloading its sprite.
	Current  *pokedex.Pokemon `json:"current,omitempty"`
	Next     *Named           `json:"next,omitempty"`
	Upcoming *Named           `json:"upcoming,omitempty"`
}

// Named is a Pokémon without its values.
type Named struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func (e *Engine) named(id int) *Named {
	p := e.Dex.Get(id)
	return &Named{ID: p.ID, Name: p.Name}
}

// State returns what the player may see of g.
func (e *Engine) State(g *Game) State {
	s := State{ID: g.ID, Mode: g.Mode, Gens: g.Mask.Gens(), Status: g.Status, Score: g.Score}
	if g.Mode == Wordle {
		answer := e.Dex.Get(g.Rounds[0].AnswerID)
		for _, r := range g.Rounds {
			if r.Answered {
				s.Guesses = append(s.Guesses, compare(e.Dex.Get(r.GuessID), answer))
			}
		}
	}
	if g.Status != Active {
		p := e.Dex.Get(g.final().AnswerID)
		s.Answer = &p
		return s
	}
	if g.Mode == Wordle {
		return s
	}
	r := g.open()
	s.Round = &PublicRound{N: r.N}
	if g.Mode == HigherLower {
		current := e.Dex.Get(r.OtherID)
		s.Round.Current = &current
		s.Round.Next = e.named(r.AnswerID)
		if r.N < len(g.Rounds) {
			s.Round.Upcoming = e.named(g.Rounds[r.N].AnswerID)
		}
	}
	return s
}
