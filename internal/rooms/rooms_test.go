package rooms

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/games"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// at is t0 plus d: tests speak in offsets.
func at(d time.Duration) time.Time { return t0.Add(d) }

// play is when the first round's countdown ends, plus d.
func play(d time.Duration) time.Time { return at(Countdown + d) }

const answer = 25

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// lobby makes a room hosted by "a" with the other players joined, everyone
// connected and the others ready, all at t0.
func lobby(t *testing.T, others ...string) *Room {
	t.Helper()
	r := New("room", "K7QX2M", "a", "Ash", games.AllGens, t0)
	must(t, r.Connect("a", t0))
	for _, p := range others {
		must(t, r.Join(p, strings.ToUpper(p), t0))
		must(t, r.Connect(p, t0))
		must(t, r.SetReady(p, true, t0))
	}
	return r
}

// started is lobby with the first round started at t0.
func started(t *testing.T, others ...string) *Room {
	t.Helper()
	r := lobby(t, others...)
	must(t, r.Start("a", answer, t0))
	return r
}

// guesses has player make misses wrong guesses and then, if solve, the right
// one, the last at when.
func guesses(t *testing.T, r *Room, player string, misses int, solve bool, when time.Time) {
	t.Helper()
	for i := range misses {
		must(t, r.Guess(player, false, when.Add(-time.Duration(misses-i)*time.Second)))
	}
	if solve {
		must(t, r.Guess(player, true, when))
	}
}

type place struct {
	player  string
	rank    int
	outcome Outcome
}

func wantRanking(t *testing.T, rd *Round, want ...place) {
	t.Helper()
	var got []place
	for _, p := range rd.Ranking() {
		got = append(got, place{p.PlayerID, p.Rank, p.Outcome})
	}
	if !slices.Equal(got, want) {
		t.Errorf("ranking = %v, want %v", got, want)
	}
}

func wantFinished(t *testing.T, r *Room, when time.Time, winner string) {
	t.Helper()
	if r.Status != Lobby {
		t.Fatalf("status = %s, want lobby (round over)", r.Status)
	}
	if !r.Round.FinishedAt.Equal(when) {
		t.Errorf("finished at %v, want %v", r.Round.FinishedAt.Sub(t0), when.Sub(t0))
	}
	if w := r.Round.Winner(); w != winner {
		t.Errorf("winner = %q, want %q", w, winner)
	}
}

func TestCodes(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	seen := map[string]bool{}
	for range 1000 {
		code := NewCode(rng.IntN)
		if parsed, ok := ParseCode(code); !ok || parsed != code {
			t.Fatalf("NewCode made %q, which doesn't parse", code)
		}
		if strings.ContainsAny(code, "0O1IL") {
			t.Fatalf("NewCode made %q, with a look-alike character", code)
		}
		seen[code] = true
	}
	if len(seen) < 999 {
		t.Errorf("only %d different codes in 1000", len(seen))
	}
	if code, ok := ParseCode("  k7qx2m "); !ok || code != "K7QX2M" {
		t.Errorf(`ParseCode("  k7qx2m ") = %q, %v`, code, ok)
	}
	for _, bad := range []string{"", "K7QX2", "K7QX2MM", "K7QX2O", "K7-X2M"} {
		if _, ok := ParseCode(bad); ok {
			t.Errorf("ParseCode(%q) accepted", bad)
		}
	}
}

func TestJoin(t *testing.T) {
	r := lobby(t, "b", "c")
	must(t, r.Join("b", "B", at(time.Second))) // again: a retry
	if len(r.Members) != 3 {
		t.Fatalf("%d members after joining twice, want 3", len(r.Members))
	}
	must(t, r.Join("d", "D", at(time.Second)))
	wantErr(t, r.Join("e", "E", at(time.Second)), ErrFull)

	// Leaving makes room.
	must(t, r.Leave("d", at(2*time.Second)))
	must(t, r.Join("e", "E", at(2*time.Second)))
	wantErr(t, r.Leave("d", at(2*time.Second)), ErrNotMember)
}

func TestStartNeedsTheHostAndEveryoneReady(t *testing.T) {
	r := lobby(t)
	wantErr(t, r.Start("a", answer, t0), ErrTooFew)

	must(t, r.Join("b", "B", t0))
	must(t, r.Connect("b", t0))
	wantErr(t, r.Start("a", answer, t0), ErrNotReady)
	must(t, r.SetReady("b", true, t0))
	wantErr(t, r.Start("b", answer, t0), ErrNotHost)
	wantErr(t, r.Start("z", answer, t0), ErrNotMember)

	// Changing the generations unreadies everyone; only the host may.
	wantErr(t, r.SetGens("b", 1, t0), ErrNotHost)
	must(t, r.SetGens("a", 1, t0))
	if r.Mask != 1 {
		t.Errorf("mask = %b, want 1", r.Mask)
	}
	wantErr(t, r.Start("a", answer, t0), ErrNotReady)

	// So does disconnecting.
	must(t, r.SetReady("b", true, t0))
	r.Disconnect("b", t0)
	wantErr(t, r.Start("a", answer, t0), ErrNotReady)
	must(t, r.Connect("b", t0))
	must(t, r.SetReady("b", true, t0))

	must(t, r.Start("a", answer, t0))
	if r.Status != Playing || r.Round.N != 1 || r.Round.AnswerID != answer {
		t.Fatalf("after Start: %s, round %+v", r.Status, r.Round)
	}
	if !r.Round.StartsAt.Equal(at(Countdown)) || !r.Round.EndsAt.Equal(play(RoundLength)) {
		t.Errorf("round from %v to %v", r.Round.StartsAt.Sub(t0), r.Round.EndsAt.Sub(t0))
	}
	if m, _ := r.Member("b"); m.Ready {
		t.Error("b is still ready once the round started")
	}

	// No settings or readying during a round.
	wantErr(t, r.Start("a", answer, t0), ErrInRound)
	wantErr(t, r.SetGens("a", 3, t0), ErrInRound)
	wantErr(t, r.SetReady("b", true, t0), ErrInRound)
}

func TestNoGuessingDuringTheCountdown(t *testing.T) {
	r := started(t, "b")
	wantErr(t, r.Guess("a", true, at(Countdown-time.Millisecond)), ErrNotStarted)
	must(t, r.Guess("a", false, at(Countdown)))
}

func TestTwoPlayersFirstSolveWins(t *testing.T) {
	r := started(t, "b")
	guesses(t, r, "b", 4, false, play(time.Minute))
	guesses(t, r, "a", 6, true, play(90*time.Second))

	wantFinished(t, r, play(90*time.Second), "a")
	wantRanking(t, r.Round, place{"a", 1, Solved}, place{"b", 2, Unsolved})
	if p := r.Round.Ranking()[0]; p.Guesses != 7 || p.Time != 90*time.Second {
		t.Errorf("a: %d guesses in %v, want 7 in 1m30s", p.Guesses, p.Time)
	}

	// The round's over: no more guesses.
	wantErr(t, r.Guess("b", true, play(91*time.Second)), ErrNoRound)
}

func TestTheRestHaveAMinuteAfterTheFirstSolve(t *testing.T) {
	r := started(t, "b", "c", "d")
	guesses(t, r, "c", 2, true, play(2*time.Minute))
	if want := play(2*time.Minute + FinishWindow); !r.Round.EndsAt.Equal(want) {
		t.Fatalf("ends at %v, want %v", r.Round.EndsAt.Sub(t0), want.Sub(t0))
	}
	// A second solve doesn't bring it forward again.
	guesses(t, r, "a", 1, true, play(2*time.Minute+10*time.Second))
	if want := play(2*time.Minute + FinishWindow); !r.Round.EndsAt.Equal(want) {
		t.Fatalf("ends at %v after a second solve, want %v", r.Round.EndsAt.Sub(t0), want.Sub(t0))
	}
	must(t, r.Guess("b", false, play(2*time.Minute+30*time.Second)))
	if r.Advance(play(2*time.Minute+FinishWindow-time.Millisecond)) || r.Status != Playing {
		t.Fatal("the round ended before its finish window did")
	}

	if !r.Advance(play(2*time.Minute + FinishWindow)) {
		t.Fatal("Advance at the end of the finish window changed nothing")
	}
	wantFinished(t, r, play(2*time.Minute+FinishWindow), "c")
	wantRanking(t, r.Round,
		place{"c", 1, Solved}, place{"a", 2, Solved}, place{"b", 3, Unsolved}, place{"d", 3, Unsolved})
}

func TestTheRoundEndsWhenOneIsLeftRacing(t *testing.T) {
	r := started(t, "b", "c")
	guesses(t, r, "b", 3, true, play(time.Minute))
	if r.Status != Playing {
		t.Fatal("ended with two players still racing")
	}
	must(t, r.Forfeit("c", play(70*time.Second)))

	// a can't do better than second any more.
	wantFinished(t, r, play(70*time.Second), "b")
	wantRanking(t, r.Round, place{"b", 1, Solved}, place{"a", 2, Unsolved}, place{"c", 3, Forfeited})
}

func TestNobodySolvingItIsADraw(t *testing.T) {
	r := started(t, "b")
	guesses(t, r, "a", 10, false, play(4*time.Minute))
	if r.Advance(play(RoundLength - time.Millisecond)) {
		t.Fatal("Advance changed something before the end")
	}
	if !r.Advance(play(RoundLength)) {
		t.Fatal("Advance at the end changed nothing")
	}
	wantFinished(t, r, play(RoundLength), "")
	wantRanking(t, r.Round, place{"a", 1, Unsolved}, place{"b", 1, Unsolved})
}

func TestATieIsADraw(t *testing.T) {
	r := started(t, "b", "c")
	guesses(t, r, "a", 2, true, play(time.Minute))
	guesses(t, r, "b", 2, true, play(time.Minute))
	wantFinished(t, r, play(time.Minute), "")
	wantRanking(t, r.Round, place{"a", 1, Solved}, place{"b", 1, Solved}, place{"c", 3, Unsolved})
}

func TestSameTimeFewerGuessesWins(t *testing.T) {
	r := started(t, "b")
	guesses(t, r, "a", 3, false, play(time.Minute-time.Second))
	must(t, r.Guess("b", false, play(time.Second)))
	// Both solve at the same moment, a on their 5th guess and b their 2nd.
	r.Round.Players[0].Guesses, r.Round.Players[0].SolvedAt = 5, play(time.Minute)
	r.Round.Players[1].Guesses, r.Round.Players[1].SolvedAt = 2, play(time.Minute)
	wantRanking(t, r.Round, place{"b", 1, Solved}, place{"a", 2, Solved})
}

func TestEveryoneElseForfeitingWins(t *testing.T) {
	r := started(t, "b")
	must(t, r.Forfeit("b", play(time.Second)))
	wantFinished(t, r, play(time.Second), "a")
	wantRanking(t, r.Round, place{"a", 1, Unsolved}, place{"b", 2, Forfeited})
}

func TestNoMovesOnceDone(t *testing.T) {
	r := started(t, "b", "c")
	guesses(t, r, "a", 0, true, play(time.Second))
	wantErr(t, r.Guess("a", false, play(2*time.Second)), ErrFinished)
	wantErr(t, r.Forfeit("a", play(2*time.Second)), ErrFinished)
	must(t, r.Forfeit("b", play(2*time.Second)))
}

func TestComingBackWithinGraceChangesNothing(t *testing.T) {
	r := started(t, "b")
	r.Disconnect("b", play(10*time.Second))
	if d := r.Deadline(); !d.Equal(play(10*time.Second + Grace)) {
		t.Errorf("deadline %v, want b's grace", d.Sub(t0))
	}
	must(t, r.Connect("b", play(10*time.Second+Grace-time.Millisecond)))
	if r.Advance(play(time.Minute)) {
		t.Error("Advance changed something after b came back")
	}
	guesses(t, r, "b", 0, true, play(2*time.Minute))
	wantFinished(t, r, play(2*time.Minute), "b")
}

func TestStayingAwayForfeits(t *testing.T) {
	r := started(t, "b")
	r.Disconnect("b", play(10*time.Second))
	// Nobody had to be around at the moment grace ran out: the next look
	// at the room (here, a's guess) applies it, as of when it ran out.
	err := r.Guess("a", false, play(time.Minute))
	wantErr(t, err, ErrNoRound)
	wantFinished(t, r, play(10*time.Second+Grace), "a")
	wantRanking(t, r.Round, place{"a", 1, Unsolved}, place{"b", 2, Forfeited})

	// Back in the lobby, b's grace has run out: they're gone.
	if _, in := r.Member("b"); in {
		t.Error("b is still in the room")
	}
	wantErr(t, r.Connect("b", play(time.Minute)), ErrNotMember)
}

func TestGraceRunningOutAfterTheEndIsntAForfeit(t *testing.T) {
	r := started(t, "b")
	r.Disconnect("b", play(RoundLength-time.Second))
	r.Advance(play(RoundLength + time.Minute))
	wantFinished(t, r, play(RoundLength), "")
	wantRanking(t, r.Round, place{"a", 1, Unsolved}, place{"b", 1, Unsolved})
}

func TestLeavingMidRoundForfeitsAndHandsOnTheHost(t *testing.T) {
	r := started(t, "b", "c")
	must(t, r.Leave("a", play(time.Second)))
	if r.Host != "b" {
		t.Errorf("host = %q, want b (the longest here)", r.Host)
	}
	if r.Status != Playing {
		t.Fatal("the round ended with two still racing")
	}
	guesses(t, r, "c", 0, true, play(time.Minute))
	wantFinished(t, r, play(time.Minute), "c")
	wantRanking(t, r.Round, place{"c", 1, Solved}, place{"b", 2, Unsolved}, place{"a", 3, Forfeited})
}

func TestJoiningMidRoundPlaysTheNextOne(t *testing.T) {
	r := started(t, "b")
	must(t, r.Join("c", "C", play(time.Second)))
	must(t, r.Connect("c", play(time.Second)))
	wantErr(t, r.Guess("c", true, play(2*time.Second)), ErrNotPlaying)
	wantErr(t, r.Forfeit("c", play(2*time.Second)), ErrNotPlaying)

	guesses(t, r, "b", 0, true, play(time.Minute))
	wantFinished(t, r, play(time.Minute), "b")

	// Rematch, with c this time.
	must(t, r.SetReady("b", true, play(2*time.Minute)))
	must(t, r.SetReady("c", true, play(2*time.Minute)))
	must(t, r.Start("a", 150, play(2*time.Minute)))
	if r.Round.N != 2 || r.Round.AnswerID != 150 || len(r.Round.Players) != 3 {
		t.Fatalf("second round: %+v", r.Round)
	}
	if p := r.Round.Players[1]; p.PlayerID != "b" || p.Done() || p.Guesses != 0 {
		t.Errorf("b starts the rematch as %+v", p)
	}
	must(t, r.Guess("c", true, play(2*time.Minute+Countdown+time.Second)))
}

func TestLobbyExpiry(t *testing.T) {
	r := lobby(t, "b")

	// Joining without ever connecting runs out of grace too.
	must(t, r.Join("c", "C", at(time.Second)))
	r.Advance(at(time.Second + Grace))
	if _, in := r.Member("c"); in {
		t.Error("c never connected and is still in the room")
	}

	// The host going hands it on.
	r.Disconnect("a", at(time.Minute))
	r.Advance(at(time.Minute + Grace))
	if r.Host != "b" || len(r.Members) != 1 {
		t.Errorf("host %q, %d members; want b alone", r.Host, len(r.Members))
	}

	// Idle: nothing happens for IdleTimeout after the last activity (c's
	// join; disconnects don't count).
	if d := r.Deadline(); !d.Equal(at(time.Second + IdleTimeout)) {
		t.Errorf("deadline %v, want the idle timeout", d.Sub(t0))
	}
	r.Advance(at(time.Second + IdleTimeout))
	if r.Status != Closed {
		t.Fatalf("status %s after the idle timeout, want closed", r.Status)
	}
	wantErr(t, r.Join("d", "D", at(time.Hour)), ErrClosed)
	if !r.Deadline().IsZero() {
		t.Error("a closed room has a deadline")
	}
}

func TestEmptyRoomCloses(t *testing.T) {
	r := lobby(t, "b")
	must(t, r.Leave("a", at(time.Second)))
	must(t, r.Leave("b", at(time.Second)))
	if r.Status != Closed || r.Host != "" {
		t.Errorf("status %s, host %q; want closed with no host", r.Status, r.Host)
	}
}

func TestEveryoneLeavingMidRound(t *testing.T) {
	r := started(t, "b")
	must(t, r.Leave("a", play(time.Second)))
	// b is the last one racing: they win, and the room's still theirs.
	wantFinished(t, r, play(time.Second), "b")
	must(t, r.Leave("b", play(2*time.Second)))
	if r.Status != Closed {
		t.Errorf("status %s, want closed", r.Status)
	}
}

func TestDeadlineDuringARound(t *testing.T) {
	r := started(t, "b")
	if d := r.Deadline(); !d.Equal(play(RoundLength)) {
		t.Errorf("deadline %v, want the round's end", d.Sub(t0))
	}
	r.Disconnect("b", play(time.Minute))
	if d := r.Deadline(); !d.Equal(play(time.Minute + Grace)) {
		t.Errorf("deadline %v, want b's grace", d.Sub(t0))
	}

	// A grace that runs past the end: the end comes first.
	r = started(t, "b")
	r.Disconnect("a", play(4*time.Minute+50*time.Second))
	if d := r.Deadline(); !d.Equal(play(RoundLength)) {
		t.Errorf("deadline %v, want the round's end", d.Sub(t0))
	}
}

func TestRankingWhilePlaying(t *testing.T) {
	r := started(t, "b", "c")
	guesses(t, r, "c", 1, true, play(time.Minute))
	wantRanking(t, r.Round, place{"c", 1, Solved}, place{"a", 2, Unsolved}, place{"b", 2, Unsolved})
	if w := r.Round.Winner(); w != "" {
		t.Errorf("winner %q while the round is on", w)
	}
}
