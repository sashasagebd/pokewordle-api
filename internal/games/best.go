package games

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// betterThanStored is the condition under which an incoming best_scores row
// (EXCLUDED) replaces the stored one: a better score (fewer guesses in
// Wordle, a longer streak otherwise), or the same score reached earlier.
const betterThanStored = `
	(CASE WHEN EXCLUDED.mode = 'wordle' THEN EXCLUDED.best < best_scores.best
	      ELSE EXCLUDED.best > best_scores.best END)
	OR (EXCLUDED.best = best_scores.best AND EXCLUDED.achieved_at < best_scores.achieved_at)`

// improvesBest reports whether the move just played set a score worth
// recording: a won Wordle, or a right streak answer.
func improvesBest(g *Game, answered *Round) bool {
	if g.Mode == Wordle {
		return g.Status == Won
	}
	return answered != nil && answered.Correct
}

// recordBest saves g's score as player's best for its mode and generations,
// if it's better than the one stored.
func recordBest(ctx context.Context, tx pgx.Tx, player string, g *Game) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO best_scores (player_id, mode, gen_mask, best, game_id, achieved_at)
		VALUES ($1::uuid, $2, $3, $4, $5::uuid, now())
		ON CONFLICT (player_id, mode, gen_mask) DO UPDATE SET
			best = EXCLUDED.best, game_id = EXCLUDED.game_id, achieved_at = EXCLUDED.achieved_at
		WHERE `+betterThanStored,
		player, g.Mode, int16(g.Mask), g.Score, g.ID)
	return err
}

// A Best is a player's best score for a mode and set of generations.
type Best struct {
	Mode       Mode      `json:"mode"`
	Gens       []int     `json:"gens"`
	Best       int       `json:"best"`
	GameID     string    `json:"gameId"`
	AchievedAt time.Time `json:"achievedAt"`
}

// Bests returns all of player's best scores.
func (s *Store) Bests(ctx context.Context, player string) ([]Best, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT mode, gen_mask, best, game_id::text, achieved_at FROM best_scores
		WHERE player_id = $1::uuid ORDER BY mode, gen_mask`, player)
	if err != nil {
		return nil, err
	}
	bests := []Best{} // [] rather than null in JSON
	for rows.Next() {
		var b Best
		var mask int16
		if err := rows.Scan(&b.Mode, &mask, &b.Best, &b.GameID, &b.AchievedAt); err != nil {
			return nil, err
		}
		b.Gens = GenMask(mask).Gens()
		bests = append(bests, b)
	}
	return bests, rows.Err()
}

// An Entry is a row of a leaderboard.
type Entry struct {
	Rank       int       `json:"rank"`
	Username   string    `json:"username"`
	Best       int       `json:"best"`
	AchievedAt time.Time `json:"achievedAt"`
}

// MyEntry is the asking player's place. Rank is nil for a player without a
// username: their best counts, but they aren't on the board until they claim
// one.
type MyEntry struct {
	Rank *int `json:"rank"`
	Best int  `json:"best"`
}

// Board is a leaderboard.
type Board struct {
	Entries []Entry  `json:"entries"`
	Me      *MyEntry `json:"me,omitempty"`
}

// ranked ranks the named players' bests for a mode ($1) and generations
// ($2): ascending for Wordle, descending otherwise, earlier first on a tie.
const ranked = `
	WITH ranked AS (
		SELECT b.player_id, p.username, b.best, b.achieved_at,
			rank() OVER (ORDER BY CASE WHEN $1 = 'wordle' THEN b.best ELSE -b.best END, b.achieved_at)::int AS rank
		FROM best_scores b JOIN players p ON p.id = b.player_id
		WHERE b.mode = $1 AND b.gen_mask = $2 AND p.username IS NOT NULL
	)`

// Leaderboard returns the top limit entries for mode and mask, and player's
// own place if player isn't "" and has a best there.
func (s *Store) Leaderboard(ctx context.Context, mode Mode, mask GenMask, limit int, player string) (Board, error) {
	board := Board{Entries: []Entry{}}
	rows, err := s.pool.Query(ctx, ranked+`
		SELECT rank, username, best, achieved_at FROM ranked ORDER BY rank, username LIMIT $3`,
		string(mode), int16(mask), limit)
	if err != nil {
		return board, err
	}
	board.Entries, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Entry])
	if err != nil {
		return board, err
	}
	if player == "" {
		return board, nil
	}

	var me MyEntry
	err = s.pool.QueryRow(ctx, ranked+`
		SELECT (SELECT rank FROM ranked WHERE player_id = $3::uuid), b.best
		FROM best_scores b WHERE b.player_id = $3::uuid AND b.mode = $1 AND b.gen_mask = $2`,
		string(mode), int16(mask), player).Scan(&me.Rank, &me.Best)
	if errors.Is(err, pgx.ErrNoRows) {
		return board, nil
	}
	if err != nil {
		return board, err
	}
	board.Me = &me
	return board, nil
}

// Merge moves an anonymous player's games and best scores to another player
// (the account they just logged in to), keeping whichever best is better,
// then deletes the anonymous player. It does nothing if from has a username
// (merging is only ever from a guest), is to, or is gone.
func (s *Store) Merge(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Locking the guest's row stops it claiming a name mid-merge (that
		// UPDATE touches a unique column, so it needs a FOR UPDATE lock). NO
		// KEY UPDATE, not FOR UPDATE: a move in progress on one of its games
		// inserts into best_scores, whose foreign key takes a KEY SHARE lock
		// on this row while holding the game's, and FOR UPDATE would deadlock
		// with it.
		var anonymous bool
		err := tx.QueryRow(ctx, `SELECT username IS NULL FROM players WHERE id = $1::uuid FOR NO KEY UPDATE`, from).Scan(&anonymous)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !anonymous) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE games SET player_id = $2::uuid WHERE player_id = $1::uuid`, from, to); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO best_scores (player_id, mode, gen_mask, best, game_id, achieved_at)
			SELECT $2::uuid, mode, gen_mask, best, game_id, achieved_at FROM best_scores WHERE player_id = $1::uuid
			ON CONFLICT (player_id, mode, gen_mask) DO UPDATE SET
				best = EXCLUDED.best, game_id = EXCLUDED.game_id, achieved_at = EXCLUDED.achieved_at
			WHERE `+betterThanStored, from, to); err != nil {
			return err
		}
		// Takes the guest's sessions and leftover best_scores rows with it.
		_, err = tx.Exec(ctx, `DELETE FROM players WHERE id = $1::uuid`, from)
		return err
	})
}
