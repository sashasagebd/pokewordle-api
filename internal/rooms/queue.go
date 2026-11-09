package rooms

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sashasagebd/pokewordle-api/internal/games"
)

// Quick match: a player asks for an opponent in a mode and set of
// generations, and is paired with whoever has waited longest for the same.
// The pair get a room of their own (Room.Quick), hosted by the one who
// waited, which starts its first round by itself once both are connected.
//
// Waiting is asking again every couple of seconds (Queue): each ask tries to
// match, and one not seen for QueueTTL has gone (closed the tab, lost the
// connection) and isn't matched with. Asking is all there is: no socket
// until there's a room.

// QueueTTL is how long a waiting player may go without asking again before
// they count as gone.
const QueueTTL = 10 * time.Second

// foundTTL is how long a match waits to be picked up by the player it was
// made for, who asks again within QueueTTL if they're still there.
const foundTTL = time.Minute

// queueLockClass is the first key of the advisory locks matching takes, one
// per mode and set of generations (the second key). Two-key advisory locks
// don't collide with the one-key ones the migrations and cleanup take.
const queueLockClass int32 = 5150

var modeKeys = map[games.Mode]int32{games.Wordle: 0, games.HigherLower: 1, games.WhosThat: 2}

// Queue asks for an opponent for player in mode and mask's generations, and
// returns the code of their room once there's one, or "" while they wait.
// Ask again to keep waiting; asking for something else waits for that
// instead.
//
// Two locks keep it straight across instances. Matching in one mode and set
// of generations happens one ask at a time (an advisory lock), so two asking
// at once can't both miss each other, each waiting for the other to be in the
// queue. And everything about one player's wait happens one thing at a time
// (their players row, locked), so they can't be matched twice: by someone
// else's ask while their own is matching them, or in two modes at once.
func (s *Store) Queue(ctx context.Context, player string, mode games.Mode, mask games.GenMask) (string, error) {
	// Two players switching modes towards each other at the same instant
	// can deadlock on each other's lock; Postgres ends one, which tries
	// again.
	for try := 0; ; try++ {
		code, made, err := s.queueOnce(ctx, player, mode, mask)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" && try < 3 {
			continue
		}
		if err != nil {
			return "", err
		}
		if made != nil {
			slog.Info("quick match", "room", made.Code, "mode", made.Mode)
		}
		return code, nil
	}
}

func (s *Store) queueOnce(ctx context.Context, player string, mode games.Mode, mask games.GenMask) (code string, made *Room, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		code, made = "", nil
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`,
			queueLockClass, modeKeys[mode]<<10|int32(mask)); err != nil {
			return err
		}
		if err := lockPlayer(ctx, tx, player); err != nil {
			return err
		}

		// Matched while waiting, by someone else's ask. (One older than
		// foundTTL is from a wait given up on: forgotten.)
		var fresh bool
		err := tx.QueryRow(ctx, `
			DELETE FROM match_found WHERE player_id = $1::uuid
			RETURNING room_code, found_at > clock_timestamp() - $2::float8 * interval '1 second'`,
			player, foundTTL.Seconds()).Scan(&code, &fresh)
		if err == nil && fresh {
			_, err := tx.Exec(ctx, `DELETE FROM match_queue WHERE player_id = $1::uuid`, player)
			return err
		}
		code = ""
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		now, err := clock(ctx, tx)
		if err != nil {
			return err
		}
		partner, err := s.partner(ctx, tx, player, mode, mask, now)
		if err != nil {
			return err
		}
		if partner == "" {
			// Nobody yet: wait (or keep waiting, keeping one's place).
			_, err := tx.Exec(ctx, `
				INSERT INTO match_queue (player_id, mode, gen_mask, enqueued_at, last_seen_at)
				VALUES ($1::uuid, $2, $3, $4, $4)
				ON CONFLICT (player_id) DO UPDATE SET
					last_seen_at = EXCLUDED.last_seen_at,
					enqueued_at = CASE WHEN (match_queue.mode, match_queue.gen_mask) = (EXCLUDED.mode, EXCLUDED.gen_mask)
						THEN match_queue.enqueued_at ELSE EXCLUDED.enqueued_at END,
					mode = EXCLUDED.mode, gen_mask = EXCLUDED.gen_mask`,
				player, mode, int16(mask), now)
			return err
		}

		// A match: a room for the two, hosted by the one who waited.
		if _, err := tx.Exec(ctx, `DELETE FROM match_queue WHERE player_id IN ($1::uuid, $2::uuid)`, player, partner); err != nil {
			return err
		}
		r, err := s.createIn(ctx, tx, partner, mask, now)
		if err != nil {
			return err
		}
		r.Mode, r.Quick = mode, true
		name, err := nameOf(ctx, tx, player)
		if err != nil {
			return err
		}
		if err := r.Join(player, name, now); err != nil {
			return err
		}
		if err := save(ctx, tx, r); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO match_found (player_id, room_code, found_at) VALUES ($1::uuid, $2, $3)
			ON CONFLICT (player_id) DO UPDATE SET room_code = EXCLUDED.room_code, found_at = EXCLUDED.found_at`,
			partner, r.Code, now); err != nil {
			return err
		}
		code, made = r.Code, r
		return nil
	})
	return code, made, err
}

// partner finds who player should be matched with, and locks them: whoever
// has waited longest in mode and mask, and is still there. "" for nobody.
func (s *Store) partner(ctx context.Context, tx pgx.Tx, player string, mode games.Mode, mask games.GenMask, now time.Time) (string, error) {
	waiting := func(q string, args ...any) (string, error) {
		var id string
		err := tx.QueryRow(ctx, q, args...).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return id, err
	}
	for range 5 {
		candidate, err := waiting(`
			SELECT player_id::text FROM match_queue
			WHERE mode = $1 AND gen_mask = $2 AND player_id <> $3::uuid AND last_seen_at > $4
			ORDER BY enqueued_at, player_id LIMIT 1`,
			mode, int16(mask), player, now.Add(-QueueTTL))
		if candidate == "" || err != nil {
			return "", err
		}
		// Once they're ours, nothing else can take them; but something may
		// have while we waited for them (their own ask, in another mode).
		if err := lockPlayer(ctx, tx, candidate); err != nil {
			return "", err
		}
		still, err := waiting(`
			SELECT player_id::text FROM match_queue
			WHERE player_id = $1::uuid AND mode = $2 AND gen_mask = $3 AND last_seen_at > $4`,
			candidate, mode, int16(mask), now.Add(-QueueTTL))
		if still != "" || err != nil {
			return still, err
		}
	}
	return "", nil // the queue keeps changing under us: wait, and ask again
}

// lockPlayer locks player's row for the rest of tx, so their quick-match
// wait changes one thing at a time. NO KEY UPDATE, not UPDATE: it doesn't
// stop rows referring to the player being written meanwhile.
func lockPlayer(ctx context.Context, tx pgx.Tx, player string) error {
	_, err := tx.Exec(ctx, `SELECT 1 FROM players WHERE id = $1::uuid FOR NO KEY UPDATE`, player)
	return err
}

// Unqueue stops player waiting for an opponent (it's fine if they weren't),
// and forgets a match made for them they hadn't picked up.
func (s *Store) Unqueue(ctx context.Context, player string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockPlayer(ctx, tx, player); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM match_queue WHERE player_id = $1::uuid`, player); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM match_found WHERE player_id = $1::uuid`, player)
		return err
	})
}
