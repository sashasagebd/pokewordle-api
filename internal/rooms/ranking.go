package rooms

import (
	"cmp"
	"slices"
	"time"
)

// An Outcome is how a player's round went.
type Outcome string

const (
	Solved    Outcome = "solved"
	Unsolved  Outcome = "unsolved" // out of time, or the last one racing when the round ended
	Forfeited Outcome = "forfeited"
)

// A Placement is where a player finished in a round.
type Placement struct {
	PlayerID string
	// Rank is 1 for first. Players tied share it, and the next rank skips
	// past them (1, 1, 3).
	Rank    int
	Outcome Outcome
	Guesses int
	// Time is how long they took to solve it, from the end of the countdown.
	Time time.Duration
}

func outcomeOf(p Player) Outcome {
	switch {
	case !p.SolvedAt.IsZero():
		return Solved
	case !p.ForfeitedAt.IsZero():
		return Forfeited
	default:
		return Unsolved
	}
}

var outcomeOrder = map[Outcome]int{Solved: 0, Unsolved: 1, Forfeited: 2}

// compare orders two placements: those who solved it first, by time then by
// guesses; then those who didn't, all tied; then those who forfeited, all
// tied.
func compare(a, b Placement) int {
	if c := cmp.Compare(outcomeOrder[a.Outcome], outcomeOrder[b.Outcome]); c != 0 || a.Outcome != Solved {
		return c
	}
	return cmp.Or(cmp.Compare(a.Time, b.Time), cmp.Compare(a.Guesses, b.Guesses))
}

// Ranking returns the round's placements, first place first (in join order
// among ties). While the round is on, it's the standings so far, with
// everyone still racing as Unsolved.
func (rd *Round) Ranking() []Placement {
	ps := make([]Placement, len(rd.Players))
	for i, p := range rd.Players {
		ps[i] = Placement{PlayerID: p.PlayerID, Outcome: outcomeOf(p), Guesses: p.Guesses}
		if ps[i].Outcome == Solved {
			ps[i].Time = p.SolvedAt.Sub(rd.StartsAt)
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
// place, who solved it first or outlasted everyone else (who all forfeited).
// "" means a draw, or a round still being played.
func (rd *Round) Winner() string {
	if rd.FinishedAt.IsZero() {
		return ""
	}
	ps := rd.Ranking()
	if len(ps) == 0 || ps[0].Outcome == Forfeited || len(ps) > 1 && ps[1].Rank == 1 {
		return ""
	}
	return ps[0].PlayerID
}
