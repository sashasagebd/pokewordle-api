package rooms

import (
	"cmp"
	"slices"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/games"
)

// An Outcome is how a player's round went.
type Outcome string

const (
	// Wordle
	Solved   Outcome = "solved"
	Unsolved Outcome = "unsolved" // out of time, or the last one racing when the round ended
	// Streak modes
	Out      Outcome = "out"      // a wrong answer ended their run
	Standing Outcome = "standing" // still going (or was, when the round ended)
	// Either
	Forfeited Outcome = "forfeited"
)

// A Placement is where a player finished in a round.
type Placement struct {
	PlayerID string
	// Rank is 1 for first. Players tied share it, and the next rank skips
	// past them (1, 1, 3).
	Rank    int
	Outcome Outcome
	Guesses int // Wordle
	Score   int // streak modes: the streak
	// Time is, from the end of the countdown, how long they took to solve it
	// (Wordle) or to reach their streak (the others).
	Time time.Duration
}

func outcomeOf(mode games.Mode, p Player) Outcome {
	switch {
	case !p.SolvedAt.IsZero():
		return Solved
	case !p.ForfeitedAt.IsZero():
		return Forfeited
	case !p.OutAt.IsZero():
		return Out
	case mode == games.Wordle:
		return Unsolved
	default:
		return Standing
	}
}

var wordleOrder = map[Outcome]int{Solved: 0, Unsolved: 1, Forfeited: 2}

// compareWordle orders two placements: those who solved it first, by time
// then by guesses; then those who didn't, all tied; then those who
// forfeited, all tied.
func compareWordle(a, b Placement) int {
	if c := cmp.Compare(wordleOrder[a.Outcome], wordleOrder[b.Outcome]); c != 0 || a.Outcome != Solved {
		return c
	}
	return cmp.Or(cmp.Compare(a.Time, b.Time), cmp.Compare(a.Guesses, b.Guesses))
}

// compareStreaks orders two placements: the longer streak first, and between
// equal ones, whoever reached it first. However their runs ended doesn't
// matter: a forfeit keeps its streak. Streaks of 0 are all tied.
func compareStreaks(a, b Placement) int {
	if c := cmp.Compare(b.Score, a.Score); c != 0 || a.Score == 0 {
		return c
	}
	return cmp.Compare(a.Time, b.Time)
}

// Ranking returns the round's placements, first place first (in join order
// among ties). While the round is on, it's the standings so far.
func (rd *Round) Ranking() []Placement {
	compare := compareWordle
	if rd.Mode != games.Wordle {
		compare = compareStreaks
	}
	ps := make([]Placement, len(rd.Players))
	for i, p := range rd.Players {
		ps[i] = Placement{PlayerID: p.PlayerID, Outcome: outcomeOf(rd.Mode, p), Guesses: p.Guesses, Score: p.Score}
		switch {
		case ps[i].Outcome == Solved:
			ps[i].Time = p.SolvedAt.Sub(rd.StartsAt)
		case rd.Mode != games.Wordle && p.Score > 0:
			ps[i].Time = p.ScoredAt.Sub(rd.StartsAt)
		}
	}
	slices.SortStableFunc(ps, compare)
	for i := range ps {
		if i > 0 && compare(ps[i-1], ps[i]) == 0 {
			ps[i].Rank = ps[i-1].Rank
		} else {
			ps[i].Rank = i + 1
		}
	}
	return ps
}

// Winner returns who won the round once it's over: the only one in first
// place. In Wordle, that's who solved it first, or outlasted everyone else
// (who all forfeited); in a streak mode, the longest streak, which can't be
// 0. "" means a draw, or a round still being played.
func (rd *Round) Winner() string {
	if rd.FinishedAt.IsZero() {
		return ""
	}
	ps := rd.Ranking()
	if len(ps) == 0 || len(ps) > 1 && ps[1].Rank == 1 {
		return ""
	}
	if rd.Mode == games.Wordle && ps[0].Outcome == Forfeited || rd.Mode != games.Wordle && ps[0].Score == 0 {
		return ""
	}
	return ps[0].PlayerID
}
