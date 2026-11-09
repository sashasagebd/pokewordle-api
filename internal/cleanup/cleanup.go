// Package cleanup tidies the database in the background: it abandons games
// left idle, deletes guests nobody has used in a while, expires logins unused
// for a year, and deletes Versus rooms left idle and expired socket tickets.
//
// It runs in every instance of the app, but takes a Postgres advisory lock
// first, so when two versions overlap (a deploy) only one does the work.
package cleanup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lockKey identifies the cleanup lock among Postgres advisory locks (the
// migrations use 5_150_251_001).
const lockKey int64 = 5_150_251_002

// Config says what counts as stale.
type Config struct {
	// IdleGame: an active game with no move for this long is abandoned.
	IdleGame time.Duration
	// GuestTTL: a guest (no username) created this long ago, with no session
	// used since, is deleted, with its games and bests.
	GuestTTL time.Duration
	// SessionTTL: a session unused this long is deleted (its player logs in
	// again).
	SessionTTL time.Duration
	// RoomTTL: a Versus room with no activity for this long is deleted, with
	// its rounds and their games. (The rules close a room idle for half an
	// hour; this tidies it away.)
	RoomTTL time.Duration
	// Batch is how many rows each statement changes at most, so none holds
	// locks for long.
	Batch int
}

// Defaults are the settings the app runs with.
var Defaults = Config{
	IdleGame:   24 * time.Hour,
	GuestTTL:   30 * 24 * time.Hour,
	SessionTTL: 365 * 24 * time.Hour,
	RoomTTL:    24 * time.Hour,
	Batch:      1000,
}

// Result is what one run changed.
type Result struct {
	Skipped         bool // another instance holds the lock
	GamesAbandoned  int64
	GuestsDeleted   int64
	SessionsExpired int64
	RoomsDeleted    int64
	TicketsExpired  int64
	QueueExpired    int64 // quick-match waits and matches given up on
}

// Run runs Once after firstDelay, then every interval, until ctx ends.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config, firstDelay, interval time.Duration) {
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		res, err := Once(ctx, pool, cfg)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Error("cleanup failed", "err", err)
		case err == nil && !res.Skipped:
			slog.Info("cleanup done", "games_abandoned", res.GamesAbandoned,
				"guests_deleted", res.GuestsDeleted, "sessions_expired", res.SessionsExpired,
				"rooms_deleted", res.RoomsDeleted, "tickets_expired", res.TicketsExpired,
				"queue_expired", res.QueueExpired)
		}
		timer.Reset(interval)
	}
}

// Once runs the cleanup jobs, unless another instance is running them.
func Once(ctx context.Context, pool *pgxpool.Pool, cfg Config) (res Result, err error) {
	// Advisory locks belong to a session, so everything runs on one connection.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return res, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&locked); err != nil {
		conn.Release()
		return res, fmt.Errorf("taking the cleanup lock: %w", err)
	}
	if !locked {
		conn.Release()
		return Result{Skipped: true}, nil
	}
	defer func() {
		// Not ctx: if it ended mid-run, the lock must still be released.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
			// Closing the connection ends the session, which releases the
			// lock; returning it to the pool would keep the lock held.
			_ = conn.Hijack().Close(unlockCtx)
			return
		}
		conn.Release()
	}()

	// batches runs sql (with $1 = an age in seconds, $2 = the batch size)
	// until it changes fewer rows than a batch.
	batches := func(sql string, age time.Duration) (total int64, err error) {
		for {
			tag, err := conn.Exec(ctx, sql, age.Seconds(), cfg.Batch)
			if err != nil {
				return total, err
			}
			total += tag.RowsAffected()
			if tag.RowsAffected() < int64(cfg.Batch) {
				return total, nil
			}
		}
	}

	// Best scores are recorded as games are played, so abandoning one loses
	// nothing.
	if res.GamesAbandoned, err = batches(`
		UPDATE games SET status = 'abandoned', updated_at = now()
		WHERE status = 'active' AND id IN (
			SELECT id FROM games
			WHERE status = 'active' AND updated_at < now() - $1::float8 * interval '1 second'
			LIMIT $2)`, cfg.IdleGame); err != nil {
		return res, fmt.Errorf("abandoning idle games: %w", err)
	}

	if res.SessionsExpired, err = batches(`
		DELETE FROM sessions WHERE token_hash IN (
			SELECT token_hash FROM sessions
			WHERE last_used_at < now() - $1::float8 * interval '1 second'
			LIMIT $2)`, cfg.SessionTTL); err != nil {
		return res, fmt.Errorf("expiring sessions: %w", err)
	}

	// The outer "username IS NULL" is checked again on each row as it's
	// deleted, so a guest claiming a name at that moment is never deleted.
	// The cascades take the guest's sessions, games, rounds and bests.
	if res.GuestsDeleted, err = batches(`
		DELETE FROM players WHERE username IS NULL AND id IN (
			SELECT p.id FROM players p
			WHERE p.username IS NULL AND p.created_at < now() - $1::float8 * interval '1 second'
				AND NOT EXISTS (
					SELECT 1 FROM sessions s
					WHERE s.player_id = p.id AND s.last_used_at >= now() - $1::float8 * interval '1 second')
			LIMIT $2)`, cfg.GuestTTL); err != nil {
		return res, fmt.Errorf("deleting stale guests: %w", err)
	}

	// The cascades take the room's members, rounds, their games and tickets.
	if res.RoomsDeleted, err = batches(`
		DELETE FROM rooms WHERE id IN (
			SELECT id FROM rooms
			WHERE last_activity_at < now() - $1::float8 * interval '1 second'
			LIMIT $2)`, cfg.RoomTTL); err != nil {
		return res, fmt.Errorf("deleting idle rooms: %w", err)
	}

	// A ticket lasts seconds; expired ones are only kept until now.
	if res.TicketsExpired, err = batches(`
		DELETE FROM ws_tickets WHERE hash IN (
			SELECT hash FROM ws_tickets
			WHERE expires_at < now() - $1::float8 * interval '1 second'
			LIMIT $2)`, 0); err != nil {
		return res, fmt.Errorf("expiring socket tickets: %w", err)
	}

	// Quick match: waits not asked about again, and matches not picked up,
	// for a minute (they count as gone well before: rooms.QueueTTL).
	waits, err := batches(`
		DELETE FROM match_queue WHERE player_id IN (
			SELECT player_id FROM match_queue
			WHERE last_seen_at < now() - $1::float8 * interval '1 second'
			LIMIT $2)`, time.Minute)
	if err != nil {
		return res, fmt.Errorf("expiring quick-match waits: %w", err)
	}
	found, err := batches(`
		DELETE FROM match_found WHERE player_id IN (
			SELECT player_id FROM match_found
			WHERE found_at < now() - $1::float8 * interval '1 second'
			LIMIT $2)`, time.Minute)
	if err != nil {
		return res, fmt.Errorf("expiring quick matches: %w", err)
	}
	res.QueueExpired = waits + found
	return res, nil
}
