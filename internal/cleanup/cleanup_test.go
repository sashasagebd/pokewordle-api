package cleanup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

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
	auth  *auth.Store
	games *games.Store
}

func setup(t *testing.T) *fixture {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	seed, err := pokedex.Seed()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pokedex.NewStore(pool).Load(ctx, seed); err != nil {
		t.Fatal(err)
	}
	return &fixture{t, ctx, pool, auth.NewStore(pool), games.NewStore(pool, &games.Engine{Dex: games.NewDex(seed)})}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

// player makes a player (named if named), created createdAgo ago with its
// session last used usedAgo ago (Postgres intervals, e.g. '40 days').
func (f *fixture) player(named bool, createdAgo, usedAgo string) string {
	f.t.Helper()
	_, id, err := f.auth.NewPlayer(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	if named {
		b := make([]byte, 6)
		rand.Read(b)
		if err := f.auth.Claim(f.ctx, id, "t"+hex.EncodeToString(b), "pikachu123"); err != nil {
			f.t.Fatal(err)
		}
	}
	f.exec(`UPDATE players SET created_at = now() - $2::interval WHERE id = $1::uuid`, id, createdAgo)
	f.exec(`UPDATE sessions SET last_used_at = now() - $2::interval WHERE player_id = $1::uuid`, id, usedAgo)
	return id
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) exists(player string) bool {
	return f.count(`SELECT count(*) FROM players WHERE id = $1::uuid`, player) == 1
}

func TestCleanup(t *testing.T) {
	f := setup(t)

	staleGuest := f.player(false, "40 days", "40 days")
	staleGame, err := f.games.Start(f.ctx, staleGuest, games.WhosThat, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}
	returningGuest := f.player(false, "40 days", "1 day")
	newGuest := f.player(false, "0 days", "0 days")
	oldNamed := f.player(true, "400 days", "40 days")
	lapsedNamed := f.player(true, "400 days", "400 days")

	idle, err := f.games.Start(f.ctx, oldNamed, games.Wordle, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE games SET updated_at = now() - interval '25 hours' WHERE id = $1::uuid`, idle.ID)
	fresh, err := f.games.Start(f.ctx, oldNamed, games.HigherLower, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}

	cfg := Defaults
	cfg.Batch = 1 // every job takes several batches
	res, err := Once(f.ctx, f.pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || res.GuestsDeleted < 1 || res.GamesAbandoned < 1 || res.SessionsExpired < 1 {
		t.Errorf("result = %+v, want at least one of each", res)
	}

	if f.exists(staleGuest) {
		t.Error("a guest unused for 40 days is still there")
	}
	if n := f.count(`SELECT count(*) FROM game_rounds WHERE game_id = $1::uuid`, staleGame.ID); n != 0 {
		t.Errorf("the deleted guest's game left %d rounds", n)
	}
	for name, p := range map[string]string{"returning guest": returningGuest, "new guest": newGuest, "named player": oldNamed, "lapsed named player": lapsedNamed} {
		if !f.exists(p) {
			t.Errorf("%s was deleted", name)
		}
	}
	if n := f.count(`SELECT count(*) FROM sessions WHERE player_id = $1::uuid`, lapsedNamed); n != 0 {
		t.Error("a session unused for 400 days wasn't expired")
	}
	if n := f.count(`SELECT count(*) FROM sessions WHERE player_id = $1::uuid`, oldNamed); n != 1 {
		t.Error("a session used 40 days ago was expired")
	}

	status := func(id string) string {
		var s string
		if err := f.pool.QueryRow(f.ctx, `SELECT status FROM games WHERE id = $1::uuid`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := status(idle.ID); s != "abandoned" {
		t.Errorf("game idle for 25 hours is %s, want abandoned", s)
	}
	if s := status(fresh.ID); s != "active" {
		t.Errorf("game just started is %s, want active", s)
	}

	// Nothing left to do on a second run. (Other packages' tests share the
	// database, but never age rows this far.)
	again, err := Once(f.ctx, f.pool, cfg)
	if err != nil || again.Skipped || again.GuestsDeleted+again.GamesAbandoned+again.SessionsExpired != 0 {
		t.Errorf("second run = %+v, %v; want a run that changes nothing", again, err)
	}
}

func TestCleanupRooms(t *testing.T) {
	f := setup(t)
	store := rooms.NewStore(f.pool, f.games)
	host := f.player(true, "0 days", "0 days")

	idle, err := store.Create(f.ctx, host, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE rooms SET last_activity_at = now() - interval '25 hours' WHERE id = $1::uuid`, idle.ID)
	fresh, err := store.Create(f.ctx, host, games.AllGens)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Ticket(f.ctx, host, fresh.Code); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE ws_tickets SET expires_at = now() - interval '1 minute' WHERE room_id = $1::uuid`, fresh.ID)

	res, err := Once(f.ctx, f.pool, Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if res.RoomsDeleted < 1 || res.TicketsExpired < 1 {
		t.Errorf("result = %+v, want a room deleted and a ticket expired", res)
	}
	if n := f.count(`SELECT count(*) FROM rooms WHERE id = $1::uuid`, idle.ID); n != 0 {
		t.Error("a room idle for 25 hours is still there")
	}
	if n := f.count(`SELECT count(*) FROM room_members WHERE room_id = $1::uuid`, idle.ID); n != 0 {
		t.Error("the deleted room left its members")
	}
	if n := f.count(`SELECT count(*) FROM rooms WHERE id = $1::uuid`, fresh.ID); n != 1 {
		t.Error("a room just made was deleted")
	}
	if n := f.count(`SELECT count(*) FROM ws_tickets WHERE room_id = $1::uuid`, fresh.ID); n != 0 {
		t.Error("an expired ticket is still there")
	}
}

func TestOnlyOneInstanceRunsIt(t *testing.T) {
	f := setup(t)
	other, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if _, err := other.Exec(f.ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	res, err := Once(f.ctx, f.pool, Defaults)
	if err != nil || !res.Skipped {
		t.Errorf("with the lock held elsewhere: %+v, %v; want skipped", res, err)
	}
	if _, err := other.Exec(f.ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	if res, err := Once(f.ctx, f.pool, Defaults); err != nil || res.Skipped {
		t.Errorf("after it's released: %+v, %v; want a run", res, err)
	}
}
