package rooms_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/dbtest"
	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
)

type fixture struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	games *games.Store
	store *rooms.Store
	dex   *games.Dex
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	seed, err := pokedex.Seed()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pokedex.NewStore(pool).Load(ctx, seed); err != nil {
		t.Fatal(err)
	}
	dex := games.NewDex(seed)
	gs := games.NewStore(pool, &games.Engine{Dex: dex})
	return &fixture{t, ctx, pool, gs, rooms.NewStore(pool, gs), dex}
}

func (f *fixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) player() string {
	f.t.Helper()
	_, id, err := auth.NewStore(f.pool).NewPlayer(f.ctx)
	f.must(err)
	return id
}

// lobby makes a room hosted by a new player with others joined, everyone
// connected and the others ready.
func (f *fixture) lobby(others int) (*rooms.Room, []string) {
	f.t.Helper()
	host := f.player()
	r, err := f.store.Create(f.ctx, host, games.AllGens)
	f.must(err)
	_, err = f.store.Connect(f.ctx, host, r.ID)
	f.must(err)
	players := []string{host}
	for range others {
		p := f.player()
		id, err := f.store.IDOf(f.ctx, r.Code)
		f.must(err)
		_, err = f.store.Join(f.ctx, p, id)
		f.must(err)
		_, err = f.store.Connect(f.ctx, p, r.ID)
		f.must(err)
		r, err = f.store.SetReady(f.ctx, p, r.ID, true)
		f.must(err)
		players = append(players, p)
	}
	return r, players
}

// started is lobby with a round started and its countdown over.
func (f *fixture) started(others int) (*rooms.Room, []string) {
	f.t.Helper()
	r, players := f.lobby(others)
	r, err := f.store.Start(f.ctx, players[0], r.ID)
	f.must(err)
	f.fastForward(r.ID, rooms.Countdown)
	return r, players
}

// fastForward moves every time stored for room id back by d, which to the
// store is d passing.
func (f *fixture) fastForward(id string, d time.Duration) {
	f.t.Helper()
	for _, q := range []string{
		`UPDATE rooms SET last_activity_at = last_activity_at - $2::float8 * interval '1 second' WHERE id = $1::uuid`,
		`UPDATE room_members SET joined_at = joined_at - $2::float8 * interval '1 second',
			disconnected_at = disconnected_at - $2::float8 * interval '1 second' WHERE room_id = $1::uuid`,
		`UPDATE room_rounds SET starts_at = starts_at - $2::float8 * interval '1 second',
			ends_at = ends_at - $2::float8 * interval '1 second',
			finished_at = finished_at - $2::float8 * interval '1 second' WHERE room_id = $1::uuid`,
		`UPDATE room_round_players p SET solved_at = solved_at - $2::float8 * interval '1 second',
			forfeited_at = forfeited_at - $2::float8 * interval '1 second'
			FROM room_rounds rd WHERE rd.id = p.round_id AND rd.room_id = $1::uuid`,
	} {
		_, err := f.pool.Exec(f.ctx, q, id, d.Seconds())
		f.must(err)
	}
}

// names returns the round's answer and a wrong guess.
func (f *fixture) names(r *rooms.Room) (right, wrong string) {
	right = f.dex.Get(r.Round.AnswerID).Name
	wrong = "bulbasaur"
	if right == wrong {
		wrong = "ivysaur"
	}
	return right, wrong
}

func (f *fixture) gameStatus(player, gameID string) games.Status {
	f.t.Helper()
	st, err := f.games.Get(f.ctx, player, gameID)
	f.must(err)
	return st.Status
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestARoundInTheDatabase(t *testing.T) {
	f := setup(t)
	r, ps := f.lobby(1)
	a, b := ps[0], ps[1]
	if r.Host != a || len(r.Members) != 2 || r.Members[1].Name == "" {
		t.Fatalf("lobby: host %q, members %+v", r.Host, r.Members)
	}

	r, err := f.store.Start(f.ctx, a, r.ID)
	f.must(err)
	if r.Status != rooms.Playing || len(r.Round.Players) != 2 || r.Round.Players[0].GameID == "" {
		t.Fatalf("started: %s, %+v", r.Status, r.Round)
	}
	right, wrong := f.names(r)
	_, _, err = f.store.Guess(f.ctx, b, r.ID, 1, wrong)
	wantErr(t, err, rooms.ErrNotStarted)
	f.fastForward(r.ID, rooms.Countdown)

	// Each race is a Wordle game of its player's, shown like a solo one.
	res, r, err := f.store.Guess(f.ctx, b, r.ID, 1, wrong)
	f.must(err)
	if res.Comparison.Correct || len(res.Game.Guesses) != 1 || r.Round.Players[1].Guesses != 1 {
		t.Fatalf("b's guess: %+v; room %+v", res, r.Round.Players[1])
	}
	// The same guess sent again is refused, not played twice.
	_, _, err = f.store.Guess(f.ctx, b, r.ID, 1, "pikachu")
	wantErr(t, err, games.ErrWrongRound)

	// A room game can't be played outside its room.
	bGame := r.Round.Players[1].GameID
	_, _, err = f.games.Guess(f.ctx, b, bGame, right)
	wantErr(t, err, games.ErrRoomGame)
	// Nor does a solo game abandon it.
	_, err = f.games.Start(f.ctx, b, games.Wordle, games.AllGens)
	f.must(err)
	if s := f.gameStatus(b, bGame); s != games.Active {
		t.Fatalf("b's room game is %s after a solo Wordle started", s)
	}

	res, r, err = f.store.Guess(f.ctx, a, r.ID, 1, right)
	f.must(err)
	if !res.Comparison.Correct || res.Game.Status != games.Won {
		t.Fatalf("a's winning guess: %+v", res)
	}
	if r.Status != rooms.Lobby || r.Round.Winner() != a {
		t.Fatalf("after the win: %s, winner %q", r.Status, r.Round.Winner())
	}

	// The loser's game is over too, so it shows the answer.
	if s := f.gameStatus(b, bGame); s != games.Lost {
		t.Errorf("b's room game is %s, want lost", s)
	}
	// Versus doesn't count toward best scores.
	bests, err := f.games.Bests(f.ctx, a)
	f.must(err)
	if len(bests) != 0 {
		t.Errorf("a has bests %+v from a room game", bests)
	}

	// It all reads back the same.
	got, err := f.store.Get(f.ctx, r.ID)
	f.must(err)
	if got.Seq != r.Seq || got.Status != rooms.Lobby || got.Round.Winner() != a ||
		!got.Round.FinishedAt.Equal(r.Round.FinishedAt) || got.Round.Players[1].Guesses != 1 {
		t.Errorf("read back %+v (round %+v), saved %+v (round %+v)", got, got.Round, r, r.Round)
	}
}

func TestAnUnknownPokemonIsntAGuess(t *testing.T) {
	f := setup(t)
	r, ps := f.started(1)
	_, r, err := f.store.Guess(f.ctx, ps[1], r.ID, 1, "missingno")
	var input *games.InputError
	if !errors.As(err, &input) {
		t.Fatalf("err = %v, want an InputError", err)
	}
	if r.Round.Players[1].Guesses != 0 {
		t.Errorf("an unknown Pokémon counted as %d guesses", r.Round.Players[1].Guesses)
	}
}

func TestTheRoundEndsByTick(t *testing.T) {
	f := setup(t)
	r, ps := f.started(1)
	if _, saved, err := f.store.Tick(f.ctx, r.ID); err != nil || saved {
		t.Fatalf("Tick mid-round: saved %v, %v; want nothing to do", saved, err)
	}

	f.fastForward(r.ID, rooms.RoundLength)
	due, err := f.store.Due(f.ctx)
	f.must(err)
	if !slices.Contains(due, r.ID) {
		t.Error("a round past its end isn't due")
	}
	r, saved, err := f.store.Tick(f.ctx, r.ID)
	f.must(err)
	if !saved || r.Status != rooms.Lobby || r.Round.Winner() != "" {
		t.Fatalf("Tick after the end: saved %v, %s, winner %q", saved, r.Status, r.Round.Winner())
	}
	for i, p := range r.Round.Players {
		if s := f.gameStatus(ps[i], p.GameID); s != games.Lost {
			t.Errorf("player %d's game is %s, want lost", i, s)
		}
	}
	due, err = f.store.Due(f.ctx)
	f.must(err)
	if slices.Contains(due, r.ID) {
		t.Error("a finished round is still due")
	}
}

func TestARefusedGuessStillSavesTheDeadline(t *testing.T) {
	f := setup(t)
	r, ps := f.started(1)
	right, _ := f.names(r)
	f.fastForward(r.ID, rooms.RoundLength+time.Second)

	_, _, err := f.store.Guess(f.ctx, ps[0], r.ID, 1, right)
	wantErr(t, err, rooms.ErrNoRound)
	got, err := f.store.Get(f.ctx, r.ID)
	f.must(err)
	if got.Status != rooms.Lobby || got.Round.FinishedAt.IsZero() {
		t.Errorf("after the late guess: %s, finished %v; want the round's end saved", got.Status, got.Round.FinishedAt)
	}
	st, err := f.games.Get(f.ctx, ps[0], got.Round.Players[0].GameID)
	f.must(err)
	if len(st.Guesses) != 0 || st.Status != games.Lost {
		t.Errorf("the late guess was played: %+v", st)
	}
}

func TestBothSolvingAtOnceHasOneWinner(t *testing.T) {
	f := setup(t)
	r, ps := f.started(1)
	right, _ := f.names(r)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, p := range ps {
		wg.Go(func() { _, _, errs[i] = f.store.Guess(f.ctx, p, r.ID, 1, right) })
	}
	wg.Wait()

	// The room's lock plays them one after the other, and the first ends
	// the round.
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, rooms.ErrNoRound):
			t.Fatal(err)
		}
	}
	got, err := f.store.Get(f.ctx, r.ID)
	f.must(err)
	if won != 1 || got.Round.Winner() == "" {
		t.Errorf("%d guesses went through, winner %q; want exactly one", won, got.Round.Winner())
	}
}

func TestJoiningByCode(t *testing.T) {
	f := setup(t)
	r, _ := f.lobby(3)
	for _, bad := range []string{"", "nope", "K7QX2O"} {
		if _, err := f.store.IDOf(f.ctx, bad); !errors.Is(err, rooms.ErrNotFound) {
			t.Errorf("IDOf(%q) = %v, want ErrNotFound", bad, err)
		}
	}
	// Any case, spaces around.
	id, err := f.store.IDOf(f.ctx, " "+strings.ToLower(r.Code)+" ")
	f.must(err)
	if id != r.ID {
		t.Errorf("IDOf found %s, want %s", id, r.ID)
	}
	_, err = f.store.Join(f.ctx, f.player(), r.ID)
	wantErr(t, err, rooms.ErrFull)
}

func TestTickets(t *testing.T) {
	f := setup(t)
	r, ps := f.lobby(1)

	_, _, err := f.store.Ticket(f.ctx, f.player(), r.Code)
	wantErr(t, err, rooms.ErrNotMember)
	_, _, err = f.store.Ticket(f.ctx, ps[0], "ZZZZZZ")
	if !errors.Is(err, rooms.ErrNotFound) && !errors.Is(err, rooms.ErrNotMember) {
		t.Errorf("a ticket for an unknown room: %v", err)
	}

	ticket, id, err := f.store.Ticket(f.ctx, ps[1], r.Code)
	f.must(err)
	if id != r.ID {
		t.Errorf("ticket for room %s, want %s", id, r.ID)
	}
	player, room, err := f.store.Redeem(f.ctx, ticket)
	f.must(err)
	if player != ps[1] || room != r.ID {
		t.Errorf("redeemed as %s in %s, want %s in %s", player, room, ps[1], r.ID)
	}
	_, _, err = f.store.Redeem(f.ctx, ticket)
	wantErr(t, err, rooms.ErrBadTicket) // single use

	expired, _, err := f.store.Ticket(f.ctx, ps[0], r.Code)
	f.must(err)
	_, err = f.pool.Exec(f.ctx, `UPDATE ws_tickets SET expires_at = now() - interval '1 second' WHERE room_id = $1::uuid`, r.ID)
	f.must(err)
	_, _, err = f.store.Redeem(f.ctx, expired)
	wantErr(t, err, rooms.ErrBadTicket)

	for _, junk := range []string{"", "not-a-ticket", ticket + "A"} {
		_, _, err := f.store.Redeem(f.ctx, junk)
		wantErr(t, err, rooms.ErrBadTicket)
	}
}

func TestChangesAreAnnounced(t *testing.T) {
	f := setup(t)
	r, ps := f.lobby(1)

	conn, err := f.pool.Acquire(f.ctx)
	f.must(err)
	defer conn.Release()
	_, err = conn.Exec(f.ctx, "LISTEN "+rooms.NotifyChannel)
	f.must(err)
	defer conn.Exec(context.Background(), "UNLISTEN *")

	r, err = f.store.SetReady(f.ctx, ps[1], r.ID, false)
	f.must(err)
	want := fmt.Sprintf("%s:%d", r.ID, r.Seq)

	// Other packages' tests share the database, so skip their rooms.
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			t.Fatalf("no notification %q: %v", want, err)
		}
		if n.Payload == want {
			return
		}
	}
}

func TestADeletedHostIsHandedOn(t *testing.T) {
	f := setup(t)
	r, ps := f.lobby(1)
	_, err := f.pool.Exec(f.ctx, `DELETE FROM players WHERE id = $1::uuid`, ps[0])
	f.must(err)
	got, err := f.store.Get(f.ctx, r.ID)
	f.must(err)
	if got.Host != ps[1] || len(got.Members) != 1 {
		t.Errorf("host %q with %d members, want %s alone", got.Host, len(got.Members), ps[1])
	}
}

// roundsOf returns the Pokémon in each round of a game so far (answer IDs,
// in order), as only a test may see them.
func (f *fixture) roundsOf(gameID string) []int {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT answer_id FROM game_rounds WHERE game_id = $1::uuid ORDER BY n`, gameID)
	f.must(err)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	f.must(err)
	return ids
}

func TestASurvivalRaceInTheDatabase(t *testing.T) {
	f := setup(t)
	r, ps := f.lobby(1)
	a, b := ps[0], ps[1]
	r, err := f.store.SetSettings(f.ctx, a, r.ID, games.WhosThat, games.AllGens)
	f.must(err)
	_, err = f.store.SetReady(f.ctx, b, r.ID, true)
	f.must(err)
	r, err = f.store.Start(f.ctx, a, r.ID)
	f.must(err)
	if r.Round.Mode != games.WhosThat || r.Round.Seed == 0 {
		t.Fatalf("round: %+v", r.Round)
	}
	f.fastForward(r.ID, rooms.Countdown)
	aGame, bGame := r.Round.Players[0].GameID, r.Round.Players[1].GameID

	// Wordle's move doesn't work here.
	_, _, err = f.store.Guess(f.ctx, a, r.ID, 1, "pikachu")
	wantErr(t, err, rooms.ErrWrongMove)

	// a gets two right; then b meets the same first Pokémon, and misses.
	for n := 1; n <= 2; n++ {
		name := f.dex.Get(f.roundsOf(aGame)[n-1]).Name
		res, _, err := f.store.Answer(f.ctx, a, r.ID, games.Answer{N: n, Name: name})
		f.must(err)
		if !res.Correct || res.Game.Score != n {
			t.Fatalf("a's answer %d: %+v", n, res)
		}
	}
	// A resend is refused, not counted.
	_, _, err = f.store.Answer(f.ctx, a, r.ID, games.Answer{N: 2, Name: "pikachu"})
	wantErr(t, err, games.ErrWrongRound)

	if first, theirs := f.roundsOf(aGame)[0], f.roundsOf(bGame)[0]; first != theirs {
		t.Fatalf("a's first Pokémon is #%d, b's #%d", first, theirs)
	}
	wrong := "bulbasaur"
	if f.dex.Get(f.roundsOf(bGame)[0]).Name == wrong {
		wrong = "ivysaur"
	}
	res, r, err := f.store.Answer(f.ctx, b, r.ID, games.Answer{N: 1, Name: wrong})
	f.must(err)
	if res.Correct || res.Game.Status != games.Lost {
		t.Fatalf("b's miss: %+v", res)
	}

	// a is ahead and the only one going: they've won.
	if r.Status != rooms.Lobby || r.Round.Winner() != a {
		t.Fatalf("after b's miss: %s, winner %q", r.Status, r.Round.Winner())
	}
	if s := f.gameStatus(a, aGame); s != games.Lost {
		t.Errorf("a's game is %s once the round ended, want lost (over, showing its Pokémon)", s)
	}
	got, err := f.store.Get(f.ctx, r.ID)
	f.must(err)
	if got.Mode != games.WhosThat || got.Round.Mode != games.WhosThat || got.Round.Players[0].Score != 2 ||
		got.Round.Players[1].OutAt.IsZero() || got.Round.Seed != r.Round.Seed {
		t.Errorf("read back: %+v, round %+v", got, got.Round)
	}

	// The same Pokémon in a's later rounds, whoever plays them: b's second
	// round (made in advance) matches a's.
	if as, bs := f.roundsOf(aGame), f.roundsOf(bGame); as[1] != bs[1] {
		t.Errorf("second Pokémon: a's #%d, b's #%d", as[1], bs[1])
	}
}
