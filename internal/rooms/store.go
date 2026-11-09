package rooms

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sashasagebd/pokewordle-api/internal/games"
)

// Errors from the store, besides the rules' own.
var (
	// ErrNotFound: no room has that code or ID.
	ErrNotFound = errors.New("no such room")
	// ErrBadTicket: the ticket isn't one we gave out, was used already, or
	// has expired.
	ErrBadTicket = errors.New("that ticket is unknown, used or expired")
)

// TicketTTL is how long a socket ticket is good for.
const TicketTTL = 30 * time.Second

// NotifyChannel is the Postgres channel every saved change is announced on,
// as "<room id>:<seq>", so each instance can push it to its own sockets.
const NotifyChannel = "room_events"

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Store keeps rooms in Postgres.
//
// Every change runs in one transaction with the room's row locked: load the
// room, let the rules change it, save it whole (a room is a handful of rows)
// and announce it on NotifyChannel, which Postgres delivers on commit. Two
// changes to a room at once are played one after the other.
//
// The time a change happens at is the database's clock_timestamp(), read
// once the lock is held. Not now(), which is when the transaction began: a
// guess that waited for the lock would otherwise be timed before the change
// it waited for.
type Store struct {
	pool  *pgxpool.Pool
	games *games.Store
	// Intn picks room codes (rand.IntN when nil). Tests seed it.
	Intn func(n int) int
}

// NewStore returns a Store using pool, playing races as games in gs.
func NewStore(pool *pgxpool.Pool, gs *games.Store) *Store {
	return &Store{pool: pool, games: gs}
}

func (s *Store) intn(n int) int {
	if s.Intn != nil {
		return s.Intn(n)
	}
	return mrand.IntN(n)
}

var errCodeTaken = errors.New("room code taken")

// Create makes a room with player as host, in the lobby.
func (s *Store) Create(ctx context.Context, player string, mask games.GenMask) (*Room, error) {
	// A taken code is retried with another. With hundreds of millions of
	// codes and rooms deleted after a day, a second try is already rare.
	for range 5 {
		code := NewCode(s.intn)
		var room *Room
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			now, err := clock(ctx, tx)
			if err != nil {
				return err
			}
			name, err := nameOf(ctx, tx, player)
			if err != nil {
				return err
			}
			var id string
			err = tx.QueryRow(ctx, `
				INSERT INTO rooms (code, host_id, gen_mask, status, last_activity_at)
				VALUES ($1, $2::uuid, $3, 'lobby', $4)
				ON CONFLICT (code) DO NOTHING
				RETURNING id::text`, code, player, int16(mask), now).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return errCodeTaken
			}
			if err != nil {
				return err
			}
			room = New(id, code, player, name, mask, now)
			return save(ctx, tx, room)
		})
		if errors.Is(err, errCodeTaken) {
			continue
		}
		return room, err
	}
	return nil, errors.New("no free room code in 5 tries")
}

// IDOf returns the ID of the room with code, as a player typed it.
func (s *Store) IDOf(ctx context.Context, code string) (string, error) {
	code, ok := ParseCode(code)
	if !ok {
		return "", ErrNotFound
	}
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM rooms WHERE code = $1`, code).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// Get returns room id as last saved (deadlines since then not applied: Tick
// does that).
func (s *Store) Get(ctx context.Context, id string) (*Room, error) {
	var room *Room
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := pgx.BeginTxFunc(ctx, s.pool, opts, func(tx pgx.Tx) error {
		var err error
		room, err = load(ctx, tx, id, false)
		return err
	})
	return room, err
}

// Join adds player to room id. Their name is their username, or "Guest"
// and a few characters of their ID.
func (s *Store) Join(ctx context.Context, player, id string) (*Room, error) {
	room, _, err := s.change(ctx, id, func(tx pgx.Tx, r *Room, now time.Time) (bool, error) {
		name, err := nameOf(ctx, tx, player)
		if err != nil {
			return false, err
		}
		return true, r.Join(player, name, now)
	})
	return room, err
}

// Leave takes player out of room id.
func (s *Store) Leave(ctx context.Context, player, id string) (*Room, error) {
	return s.simple(ctx, id, func(r *Room, now time.Time) error { return r.Leave(player, now) })
}

// Connect records that player's socket to room id opened.
func (s *Store) Connect(ctx context.Context, player, id string) (*Room, error) {
	return s.simple(ctx, id, func(r *Room, now time.Time) error { return r.Connect(player, now) })
}

// Disconnect records that player's last socket to room id closed.
func (s *Store) Disconnect(ctx context.Context, player, id string) (*Room, error) {
	return s.simple(ctx, id, func(r *Room, now time.Time) error {
		r.Disconnect(player, now)
		return nil
	})
}

// SetReady marks player ready, or not, in room id.
func (s *Store) SetReady(ctx context.Context, player, id string, ready bool) (*Room, error) {
	return s.simple(ctx, id, func(r *Room, now time.Time) error { return r.SetReady(player, ready, now) })
}

// SetGens changes room id's generations (the host only).
func (s *Store) SetGens(ctx context.Context, player, id string, mask games.GenMask) (*Room, error) {
	return s.simple(ctx, id, func(r *Room, now time.Time) error { return r.SetGens(player, mask, now) })
}

// Forfeit gives up the round for player in room id.
func (s *Store) Forfeit(ctx context.Context, player, id string) (*Room, error) {
	return s.simple(ctx, id, func(r *Room, now time.Time) error { return r.Forfeit(player, now) })
}

// Tick applies room id's deadlines that have passed, saving it if that
// changed anything (and reporting whether it did).
func (s *Store) Tick(ctx context.Context, id string) (*Room, bool, error) {
	return s.change(ctx, id, func(pgx.Tx, *Room, time.Time) (bool, error) { return false, nil })
}

// Start begins a round in room id (the host only): a random answer from the
// room's generations, and a Wordle game with it for every player.
func (s *Store) Start(ctx context.Context, player, id string) (*Room, error) {
	room, _, err := s.change(ctx, id, func(tx pgx.Tx, r *Room, now time.Time) (bool, error) {
		if err := r.Start(player, s.games.Pick(r.Mask), now); err != nil {
			return false, err
		}
		rd := r.Round
		if err := tx.QueryRow(ctx, `
			INSERT INTO room_rounds (room_id, n, answer_id, starts_at, ends_at)
			VALUES ($1::uuid, $2, $3, $4, $5) RETURNING id::text`,
			r.ID, rd.N, rd.AnswerID, rd.StartsAt, rd.EndsAt).Scan(&rd.ID); err != nil {
			return false, err
		}
		for i := range rd.Players {
			p := &rd.Players[i]
			gameID, err := s.games.StartInRound(ctx, tx, p.PlayerID, rd.ID, r.Mask, rd.AnswerID)
			if err != nil {
				return false, err
			}
			p.GameID = gameID
			if _, err := tx.Exec(ctx, `
				INSERT INTO room_round_players (round_id, player_id, position, name, game_id)
				VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid)`, rd.ID, p.PlayerID, i, p.Name, gameID); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	return room, err
}

// A GuessResult is a Versus guess as its player sees it: the same as a solo
// Wordle guess.
type GuessResult struct {
	Comparison games.Comparison
	Game       games.State
}

// Guess plays player's guess number n (1 for the first) in room id's round.
// A mistake that isn't a move (an unknown Pokémon, a repeat) is a
// *games.InputError, and a guess sent twice is games.ErrWrongRound.
func (s *Store) Guess(ctx context.Context, player, id string, n int, name string) (GuessResult, *Room, error) {
	var res GuessResult
	room, _, err := s.change(ctx, id, func(tx pgx.Tx, r *Room, now time.Time) (bool, error) {
		// Asked first, so a guess the room would refuse is never played.
		if err := r.CanMove(player, now); err != nil {
			return false, err
		}
		c, st, err := s.games.GuessInRound(ctx, tx, player, r.player(player).GameID, n, name)
		if err != nil {
			return false, err
		}
		res = GuessResult{c, st}
		return true, r.Guess(player, c.Correct, now)
	})
	return res, room, err
}

// simple is change for rules that don't touch the database themselves.
func (s *Store) simple(ctx context.Context, id string, fn func(r *Room, now time.Time) error) (*Room, error) {
	room, _, err := s.change(ctx, id, func(_ pgx.Tx, r *Room, now time.Time) (bool, error) {
		return true, fn(r, now)
	})
	return room, err
}

// change loads room id locked, brings it up to now (Advance), and runs fn,
// which says whether it changed the room. If anything did, the room is saved.
//
// fn runs in a savepoint. When it fails (a move the rules refuse, an unknown
// Pokémon), whatever it wrote is undone and the room is read again, but
// deadlines that had passed are still saved, and fn's error is returned with
// the room as saved.
func (s *Store) change(ctx context.Context, id string, fn func(tx pgx.Tx, r *Room, now time.Time) (bool, error)) (*Room, bool, error) {
	var room *Room
	var saved bool
	var fnErr error
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := load(ctx, tx, id, true)
		if err != nil {
			return err
		}
		now, err := clock(ctx, tx)
		if err != nil {
			return err
		}
		openRound := ""
		if r.Status == Playing {
			openRound = r.Round.ID
		}
		advanced := r.Advance(now)

		changed := false
		fnErr = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			var err error
			changed, err = fn(sp, r, now)
			return err
		})
		if fnErr != nil {
			// fn may have changed r before failing (Start's rules passing,
			// then an insert failing): start again from what's stored.
			changed = false
			if r, err = load(ctx, tx, id, false); err != nil { // still locked
				return err
			}
			r.Advance(now)
		}
		room = r
		if !advanced && !changed {
			return nil
		}
		// The round that was on has ended: so do the games of those who
		// didn't solve it.
		if openRound != "" && r.Round.ID == openRound && !r.Round.FinishedAt.IsZero() {
			if err := s.games.EndRound(ctx, tx, openRound); err != nil {
				return err
			}
		}
		saved = true
		return save(ctx, tx, r)
	})
	if err != nil {
		return nil, false, err
	}
	return room, saved, fnErr
}

// clock returns the database's time right now.
func clock(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

// nameOf returns the name player shows in a room.
func nameOf(ctx context.Context, tx pgx.Tx, player string) (string, error) {
	var name string
	err := tx.QueryRow(ctx, `
		SELECT coalesce(username, 'Guest ' || upper(left(replace(id::text, '-', ''), 4)))
		FROM players WHERE id = $1::uuid`, player).Scan(&name)
	return name, err
}

// nullTime is t for the database, with the zero time as NULL.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// orZero is a nullable time from the database, with NULL as the zero time.
func orZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// load reads room id with its members and its latest round, locking the
// room's row when lock is set.
func load(ctx context.Context, tx pgx.Tx, id string, lock bool) (*Room, error) {
	if !uuidPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	q := `SELECT id::text, code, coalesce(host_id::text, ''), gen_mask, status, last_activity_at, seq
		FROM rooms WHERE id = $1::uuid`
	if lock {
		q += ` FOR UPDATE`
	}
	r := &Room{}
	var mask int16
	err := tx.QueryRow(ctx, q, id).Scan(&r.ID, &r.Code, &r.Host, &mask, &r.Status, &r.LastActivity, &r.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Mask = games.GenMask(mask)

	rows, err := tx.Query(ctx, `
		SELECT player_id::text, name, ready, joined_at, disconnected_at FROM room_members
		WHERE room_id = $1::uuid ORDER BY joined_at, player_id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m Member
		var disconnected *time.Time
		if err := rows.Scan(&m.PlayerID, &m.Name, &m.Ready, &m.JoinedAt, &disconnected); err != nil {
			return nil, err
		}
		m.DisconnectedAt = orZero(disconnected)
		r.Members = append(r.Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The host's player was deleted (the host's row is gone, host_id set
	// NULL): the rules would have handed it on, so do the same.
	if _, ok := r.Member(r.Host); !ok {
		r.Host = ""
		if len(r.Members) > 0 {
			r.Host = r.Members[0].PlayerID
		}
	}

	rd := &Round{}
	var finished *time.Time
	err = tx.QueryRow(ctx, `
		SELECT id::text, n, answer_id, starts_at, ends_at, finished_at FROM room_rounds
		WHERE room_id = $1::uuid ORDER BY n DESC LIMIT 1`, id).
		Scan(&rd.ID, &rd.N, &rd.AnswerID, &rd.StartsAt, &rd.EndsAt, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		if r.Status == Playing {
			return nil, fmt.Errorf("room %s is playing without a round", id)
		}
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	rd.FinishedAt = orZero(finished)
	rows, err = tx.Query(ctx, `
		SELECT player_id::text, name, game_id::text, guesses, solved_at, forfeited_at FROM room_round_players
		WHERE round_id = $1::uuid ORDER BY position`, rd.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p Player
		var solved, forfeited *time.Time
		if err := rows.Scan(&p.PlayerID, &p.Name, &p.GameID, &p.Guesses, &solved, &forfeited); err != nil {
			return nil, err
		}
		p.SolvedAt, p.ForfeitedAt = orZero(solved), orZero(forfeited)
		rd.Players = append(rd.Players, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	r.Round = rd
	return r, nil
}

// save writes r back (its round's rows must exist already: Start makes
// them), bumps its seq and announces the change.
func save(ctx context.Context, tx pgx.Tx, r *Room) error {
	if err := tx.QueryRow(ctx, `
		UPDATE rooms SET host_id = nullif($2, '')::uuid, gen_mask = $3, status = $4,
			last_activity_at = $5, seq = seq + 1
		WHERE id = $1::uuid RETURNING seq`,
		r.ID, r.Host, int16(r.Mask), r.Status, r.LastActivity).Scan(&r.Seq); err != nil {
		return err
	}

	ids := make([]string, 0, len(r.Members)) // not nil: that would be NULL, matching nothing
	for _, m := range r.Members {
		ids = append(ids, m.PlayerID)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM room_members WHERE room_id = $1::uuid AND NOT (player_id::text = ANY($2::text[]))`,
		r.ID, ids); err != nil {
		return err
	}
	for _, m := range r.Members {
		if _, err := tx.Exec(ctx, `
			INSERT INTO room_members (room_id, player_id, name, ready, joined_at, disconnected_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)
			ON CONFLICT (room_id, player_id) DO UPDATE SET
				name = EXCLUDED.name, ready = EXCLUDED.ready, disconnected_at = EXCLUDED.disconnected_at`,
			r.ID, m.PlayerID, m.Name, m.Ready, m.JoinedAt, nullTime(m.DisconnectedAt)); err != nil {
			return err
		}
	}

	if rd := r.Round; rd != nil {
		if _, err := tx.Exec(ctx, `UPDATE room_rounds SET ends_at = $2, finished_at = $3 WHERE id = $1::uuid`,
			rd.ID, rd.EndsAt, nullTime(rd.FinishedAt)); err != nil {
			return err
		}
		for _, p := range rd.Players {
			if _, err := tx.Exec(ctx, `
				UPDATE room_round_players SET guesses = $3, solved_at = $4, forfeited_at = $5
				WHERE round_id = $1::uuid AND player_id = $2::uuid`,
				rd.ID, p.PlayerID, p.Guesses, nullTime(p.SolvedAt), nullTime(p.ForfeitedAt)); err != nil {
				return err
			}
		}
	}

	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, fmt.Sprintf("%s:%d", r.ID, r.Seq))
	return err
}

// Due returns rooms with a deadline that may have passed, for a sweeper to
// Tick: one nobody is connected to has no timer running anywhere. It errs on
// the side of including a room (Tick then changes nothing).
func (s *Store) Due(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id::text FROM rooms r
		WHERE r.status = 'lobby' AND r.last_activity_at <= now() - $1::float8 * interval '1 second'
		UNION
		SELECT rd.room_id::text FROM room_rounds rd JOIN rooms r ON r.id = rd.room_id
		WHERE r.status = 'playing' AND rd.finished_at IS NULL AND rd.ends_at <= now()
		UNION
		SELECT m.room_id::text FROM room_members m JOIN rooms r ON r.id = m.room_id
		WHERE r.status <> 'closed' AND m.disconnected_at <= now() - $2::float8 * interval '1 second'`,
		IdleTimeout.Seconds(), Grace.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// --- Socket tickets -------------------------------------------------------------

// Ticket returns a single-use ticket for player to open a socket to the room
// with code, and the room's ID. Only a member of a room still open gets one.
func (s *Store) Ticket(ctx context.Context, player, code string) (ticket, roomID string, err error) {
	roomID, err = s.IDOf(ctx, code)
	if err != nil {
		return "", "", err
	}
	raw := make([]byte, 32)
	rand.Read(raw) // never fails (crypto/rand panics instead)
	hash := sha256.Sum256(raw)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO ws_tickets (hash, player_id, room_id, expires_at)
		SELECT $1::bytea, m.player_id, m.room_id, now() + $3::float8 * interval '1 second'
		FROM room_members m JOIN rooms r ON r.id = m.room_id
		WHERE m.room_id = $2::uuid AND m.player_id = $4::uuid AND r.status <> 'closed'`,
		hash[:], roomID, TicketTTL.Seconds(), player)
	if err != nil {
		return "", "", err
	}
	if tag.RowsAffected() == 0 {
		return "", "", ErrNotMember
	}
	return base64.RawURLEncoding.EncodeToString(raw), roomID, nil
}

// Redeem uses up ticket and returns whose it was and for which room.
func (s *Store) Redeem(ctx context.Context, ticket string) (player, roomID string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil || len(raw) != 32 {
		return "", "", ErrBadTicket
	}
	hash := sha256.Sum256(raw)
	err = s.pool.QueryRow(ctx, `
		DELETE FROM ws_tickets WHERE hash = $1 AND expires_at > now()
		RETURNING player_id::text, room_id::text`, hash[:]).Scan(&player, &roomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrBadTicket
	}
	return player, roomID, err
}
