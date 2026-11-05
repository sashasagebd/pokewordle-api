package games

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	seed, err := pokedex.Seed()
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Dex: NewDex(seed), Intn: rand.New(rand.NewPCG(1, 2)).IntN}
}

func mask(t *testing.T, gens ...int) GenMask {
	t.Helper()
	m, err := MaskOf(gens)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// otherThan returns a Pokémon in m other than the excluded ones.
func otherThan(e *Engine, m GenMask, exclude ...int) pokedex.Pokemon {
	return e.Dex.Get(e.Dex.random(m, e.intn, exclude...))
}

func isInput(err error) bool {
	var ie *InputError
	return errors.As(err, &ie)
}

func TestGenMask(t *testing.T) {
	if m := mask(t); m != AllGens || len(m.Gens()) != 9 {
		t.Errorf("no gens = %b, want all 9", m)
	}
	m := mask(t, 9, 1, 1)
	if m != 0b1_0000_0001 || !slices.Equal(m.Gens(), []int{1, 9}) || m.Has(2) {
		t.Errorf("gens 9,1,1 = %b (%v)", m, m.Gens())
	}
	for _, bad := range [][]int{{0}, {10}, {1, -1}} {
		if _, err := MaskOf(bad); err == nil {
			t.Errorf("MaskOf(%v) accepted", bad)
		}
	}
}

func TestRandomStaysInGensAndSkipsExcluded(t *testing.T) {
	e := newEngine(t)
	m := mask(t, 2, 7)
	for range 2000 {
		id := e.Dex.random(m, e.intn, 152)
		if g := pokedex.GenerationOf(id); !m.Has(g) || id == 152 {
			t.Fatalf("picked #%d (gen %d)", id, g)
		}
	}
}

func TestEveryTypeHasMatchups(t *testing.T) {
	for _, ty := range pokedex.Types {
		if _, ok := superEffective[ty]; !ok {
			t.Errorf("no matchups for %s", ty)
		}
	}
	for ty, targets := range superEffective {
		for _, target := range targets {
			if !slices.Contains(pokedex.Types, target) {
				t.Errorf("%s: unknown target %s", ty, target)
			}
		}
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct{ guess, answer, want string }{
		{"fire", "fire", SameType},
		{"fire", "grass", Effective},
		{"grass", "fire", Weak},
		{"normal", "ghost", Neutral},
		{"ground", "electric", Effective}, // as the frontend's table had it
	} {
		if got := typeRelation(c.guess, c.answer); got != c.want {
			t.Errorf("typeRelation(%s, %s) = %s, want %s", c.guess, c.answer, got, c.want)
		}
	}

	bulbasaur := pokedex.Pokemon{ID: 1, Height: 7, Weight: 69, Types: []string{"grass", "poison"}}
	charmander := pokedex.Pokemon{ID: 4, Height: 6, Weight: 85, Types: []string{"fire"}}
	got := compare(bulbasaur, charmander)
	want := Comparison{Guess: bulbasaur, Height: Lower, Weight: Higher, Type: Weak}
	if got.Height != want.Height || got.Weight != want.Weight || got.Type != want.Type || got.Correct {
		t.Errorf("compare(bulbasaur, charmander) = %+v, want %+v", got, want)
	}
	if c := compare(bulbasaur, bulbasaur); !c.Correct || c.Height != Equal || c.Type != SameType {
		t.Errorf("compare(bulbasaur, bulbasaur) = %+v", c)
	}
}

func TestHigherLowerTiesCountEitherWay(t *testing.T) {
	a := pokedex.Pokemon{Height: 5, Weight: 10}
	b := pokedex.Pokemon{Height: 5, Weight: 20}
	if !higherLowerCorrect(a, b, Height, Higher) || !higherLowerCorrect(a, b, Height, Lower) {
		t.Error("a tie must count for both answers")
	}
	if !higherLowerCorrect(a, b, Weight, Higher) || higherLowerCorrect(a, b, Weight, Lower) {
		t.Error("20 kg is higher than 10 kg")
	}
}

func TestWordle(t *testing.T) {
	e := newEngine(t)
	m := mask(t, 1)
	g := e.New(Wordle, m)
	answer := e.Dex.Get(g.Rounds[0].AnswerID)
	if s := e.State(g); s.Answer != nil || s.Round != nil || len(s.Guesses) != 0 {
		t.Fatalf("a new game shows %+v", s)
	}

	wrong := otherThan(e, m, answer.ID)
	r, c, err := e.Guess(g, " "+wrong.Name+" ")
	if err != nil || c.Correct || r.N != 1 || g.Score != 1 || len(g.Rounds) != 2 {
		t.Fatalf("wrong guess: round %+v, %+v, %v; score %d, %d rounds", r, c, err, g.Score, len(g.Rounds))
	}

	for name, bad := range map[string]string{
		"repeat":       wrong.Name,
		"unknown":      "missingno",
		"outside gens": "chikorita",
	} {
		if _, _, err := e.Guess(g, bad); !isInput(err) {
			t.Errorf("%s: err = %v, want an InputError", name, err)
		}
	}
	if _, _, err := e.Answer(g, Answer{N: 2, Name: answer.Name}); !isInput(err) {
		t.Errorf("Answer on Wordle: err = %v, want an InputError", err)
	}
	if g.Score != 1 {
		t.Fatalf("bad guesses counted: score %d", g.Score)
	}
	if s := e.State(g); s.Answer != nil || len(s.Guesses) != 1 {
		t.Fatalf("mid-game state shows %+v", s)
	}

	if _, c, err := e.Guess(g, answer.Name); err != nil || !c.Correct {
		t.Fatalf("right guess: %+v, %v", c, err)
	}
	s := e.State(g)
	if s.Status != Won || s.Score != 2 || s.Answer == nil || s.Answer.ID != answer.ID || len(s.Guesses) != 2 {
		t.Fatalf("won state = %+v", s)
	}
	if _, _, err := e.Guess(g, otherThan(e, m, answer.ID, wrong.ID).Name); !errors.Is(err, ErrOver) {
		t.Errorf("guess after winning: err = %v, want ErrOver", err)
	}
}

func TestWordleGiveUpReveals(t *testing.T) {
	e := newEngine(t)
	g := e.New(Wordle, AllGens)
	if err := e.GiveUp(g); err != nil {
		t.Fatal(err)
	}
	s := e.State(g)
	if s.Status != Lost || s.Answer == nil || s.Answer.ID != g.Rounds[0].AnswerID {
		t.Fatalf("given-up state = %+v", s)
	}
	if err := e.GiveUp(g); !errors.Is(err, ErrOver) {
		t.Errorf("second give-up: err = %v, want ErrOver", err)
	}
}

// rightDir answers g's open Higher or Lower round correctly on stat.
func rightDir(e *Engine, g *Game, stat string) string {
	r := g.open()
	if statOf(e.Dex.Get(r.AnswerID), stat) >= statOf(e.Dex.Get(r.OtherID), stat) {
		return Higher
	}
	return Lower
}

func TestHigherLower(t *testing.T) {
	e := newEngine(t)
	g := e.New(HigherLower, AllGens)
	if len(g.Rounds) != 2 || g.Rounds[1].OtherID != g.Rounds[0].AnswerID {
		t.Fatalf("rounds aren't chained: %+v", g.Rounds)
	}

	s := e.State(g)
	r0 := g.Rounds[0]
	if s.Round == nil || s.Round.N != 1 || s.Round.Current.ID != r0.OtherID ||
		s.Round.Next.ID != r0.AnswerID || s.Round.Upcoming.ID != g.Rounds[1].AnswerID {
		t.Fatalf("state round = %+v", s.Round)
	}

	for _, bad := range []Answer{{N: 1, Dir: "up", Stat: Height}, {N: 1, Dir: Higher, Stat: "speed"}} {
		if _, _, err := e.Answer(g, bad); !isInput(err) {
			t.Errorf("%+v: err = %v, want an InputError", bad, err)
		}
	}
	if _, _, err := e.Answer(g, Answer{N: 2, Dir: Higher, Stat: Height}); !errors.Is(err, ErrWrongRound) {
		t.Errorf("answering round 2 first: err = %v, want ErrWrongRound", err)
	}

	for n := 1; n <= 5; n++ {
		stat := []string{Height, Weight}[n%2]
		_, res, err := e.Answer(g, Answer{N: n, Dir: rightDir(e, g, stat), Stat: stat})
		if err != nil || !res.Correct || res.Answer.ID != g.Rounds[n-1].AnswerID {
			t.Fatalf("round %d: %+v, %v", n, res, err)
		}
	}
	s = e.State(g)
	if s.Score != 5 || s.Round.N != 6 || s.Round.Current.ID != g.Rounds[4].AnswerID || len(g.Rounds) != 7 {
		t.Fatalf("after 5 right: %+v, round %+v, %d rounds", s, s.Round, len(g.Rounds))
	}
	if _, _, err := e.Answer(g, Answer{N: 5, Dir: Higher, Stat: Height}); !errors.Is(err, ErrWrongRound) {
		t.Errorf("answering round 5 again: err = %v, want ErrWrongRound", err)
	}

	// Find a round with no tie, then answer it wrong.
	for g.Status == Active {
		r := g.open()
		current, next := e.Dex.Get(r.OtherID), e.Dex.Get(r.AnswerID)
		dir := rightDir(e, g, Height)
		if current.Height != next.Height {
			dir = map[string]string{Higher: Lower, Lower: Higher}[dir]
		}
		if _, _, err := e.Answer(g, Answer{N: r.N, Dir: dir, Stat: Height}); err != nil {
			t.Fatal(err)
		}
	}
	s = e.State(g)
	last := g.final()
	if s.Status != Lost || s.Round != nil || last.Correct || s.Answer.ID != last.AnswerID || s.Score != last.N-1 {
		t.Fatalf("lost state = %+v (final round %+v)", s, last)
	}
}

func TestWhosThat(t *testing.T) {
	e := newEngine(t)
	m := mask(t, 3)
	g := e.New(WhosThat, m)
	s := e.State(g)
	if s.Round == nil || s.Round.N != 1 || s.Round.Current != nil || s.Round.Next != nil || s.Round.Upcoming != nil || s.Answer != nil {
		t.Fatalf("Who's That must show only the round number: %+v", s.Round)
	}

	if _, _, err := e.Answer(g, Answer{N: 1, Name: "pikachu"}); !isInput(err) {
		t.Errorf("outside gens: err = %v, want an InputError", err)
	}
	answer := e.Dex.Get(g.Rounds[0].AnswerID)
	if _, res, err := e.Answer(g, Answer{N: 1, Name: answer.Name}); err != nil || !res.Correct || res.Answer.ID != answer.ID {
		t.Fatalf("right answer: %+v, %v", res, err)
	}
	if g.Rounds[2].AnswerID == g.Rounds[1].AnswerID {
		t.Error("a round repeats the one before it")
	}

	open := g.Rounds[1]
	wrong := otherThan(e, m, open.AnswerID)
	_, res, err := e.Answer(g, Answer{N: 2, Name: wrong.Name})
	if err != nil || res.Correct || res.Answer.ID != open.AnswerID {
		t.Fatalf("wrong answer: %+v, %v", res, err)
	}
	if s := e.State(g); s.Status != Lost || s.Score != 1 || s.Answer.ID != open.AnswerID {
		t.Fatalf("lost state = %+v", s)
	}
}

func TestStreakGiveUpRevealsTheOpenRound(t *testing.T) {
	e := newEngine(t)
	g := e.New(WhosThat, AllGens)
	if _, _, err := e.Answer(g, Answer{N: 1, Name: e.Dex.Get(g.Rounds[0].AnswerID).Name}); err != nil {
		t.Fatal(err)
	}
	if err := e.GiveUp(g); err != nil {
		t.Fatal(err)
	}
	if s := e.State(g); s.Status != Lost || s.Score != 1 || s.Answer.ID != g.Rounds[1].AnswerID {
		t.Fatalf("given-up state = %+v", s)
	}
}
