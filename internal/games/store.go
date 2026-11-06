package games

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps games in Postgres. Every move runs in one transaction with the
// game's row locked, so two requests for the same game (a double click) are
// played one after the other, never both against the same state.
type Store struct {
	pool   *pgxpool.Pool
	engine *Engine
}

// NewStore returns a Store using pool and engine.
func NewStore(pool *pgxpool.Pool, engine *Engine) *Store {
	return &Store{pool: pool, engine: engine}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Start makes a new game for player. Any game of the same mode the player
// still has going is abandoned.
func (s *Store) Start(ctx context.Context, player string, mode Mode, mask GenMask) (State, error) {
	g := s.engine.New(mode, mask)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE games SET status = 'abandoned', updated_at = now()
			WHERE player_id = $1::uuid AND mode = $2 AND status = 'active'`, player, mode); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO games (player_id, mode, gen_mask) VALUES ($1::uuid, $2, $3)
			RETURNING id::text`, player, mode, int16(mask)).Scan(&g.ID); err != nil {
			return err
		}
		return insertRounds(ctx, tx, g.ID, g.Rounds)
	})
	if err != nil {
		return State{}, err
	}
	return s.engine.State(g), nil
}

// Get returns player's game id.
func (s *Store) Get(ctx context.Context, player, id string) (State, error) {
	var st State
	// One snapshot for the game and its rounds, without locking anything.
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := pgx.BeginTxFunc(ctx, s.pool, opts, func(tx pgx.Tx) error {
		g, err := load(ctx, tx, player, id, false)
		if err != nil {
			return err
		}
		st = s.engine.State(g)
		return nil
	})
	return st, err
}

// Guess plays a Wordle guess.
func (s *Store) Guess(ctx context.Context, player, id, name string) (Comparison, State, error) {
	var c Comparison
	st, err := s.move(ctx, player, id, func(g *Game) (*Round, error) {
		r, comparison, err := s.engine.Guess(g, name)
		c = comparison
		return r, err
	})
	return c, st, err
}

// Answer plays a streak-mode answer.
func (s *Store) Answer(ctx context.Context, player, id string, a Answer) (AnswerResult, State, error) {
	var res AnswerResult
	st, err := s.move(ctx, player, id, func(g *Game) (*Round, error) {
		r, result, err := s.engine.Answer(g, a)
		res = result
		return r, err
	})
	return res, st, err
}

// GiveUp ends a game as lost.
func (s *Store) GiveUp(ctx context.Context, player, id string) (State, error) {
	return s.move(ctx, player, id, func(g *Game) (*Round, error) {
		return nil, s.engine.GiveUp(g)
	})
}

// SilhouetteOf returns the Pokémon in round n of player's Who's That game, for
// its silhouette. Only rounds that exist have one: up to the one after the
// open round.
func (s *Store) SilhouetteOf(ctx context.Context, player, id string, n int) (int, error) {
	if !uuidPattern.MatchString(id) {
		return 0, ErrNotFound
	}
	var answerID int
	err := s.pool.QueryRow(ctx, `
		SELECT r.answer_id FROM game_rounds r JOIN games g ON g.id = r.game_id
		WHERE g.id = $1::uuid AND g.player_id = $2::uuid AND g.mode = 'whos_that' AND r.n = $3`,
		id, player, n).Scan(&answerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return answerID, err
}

// move loads the game locked, lets play make a move (which returns the round
// it answered, if any), and saves what changed: that round, rounds added
// after it, and the game's status and score.
func (s *Store) move(ctx context.Context, player, id string, play func(*Game) (*Round, error)) (State, error) {
	var st State
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		g, err := load(ctx, tx, player, id, true)
		if err != nil {
			return err
		}
		before := len(g.Rounds)
		answered, err := play(g)
		if err != nil {
			return err
		}
		if answered != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE game_rounds SET
					stat = nullif($3, ''), guess_id = nullif($4::int, 0), guess_dir = nullif($5, ''),
					correct = $6, answered_at = now()
				WHERE game_id = $1::uuid AND n = $2`,
				g.ID, answered.N, answered.Stat, answered.GuessID, answered.GuessDir, answered.Correct); err != nil {
				return err
			}
		}
		if err := insertRounds(ctx, tx, g.ID, g.Rounds[before:]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE games SET status = $2, score = $3, updated_at = now() WHERE id = $1::uuid`,
			g.ID, g.Status, g.Score); err != nil {
			return err
		}
		if improvesBest(g, answered) {
			if err := recordBest(ctx, tx, player, g); err != nil {
				return err
			}
		}
		st = s.engine.State(g)
		return nil
	})
	return st, err
}

// load reads player's game id with its rounds, locking the game's row when
// lock is set. Someone else's game is ErrNotFound, the same as a missing one,
// so game ids can't be probed.
func load(ctx context.Context, tx pgx.Tx, player, id string, lock bool) (*Game, error) {
	if !uuidPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	q := `SELECT id::text, mode, gen_mask, status, score FROM games WHERE id = $1::uuid AND player_id = $2::uuid`
	if lock {
		q += ` FOR UPDATE`
	}
	g := &Game{}
	var mask int16
	err := tx.QueryRow(ctx, q, id, player).Scan(&g.ID, &g.Mode, &mask, &g.Status, &g.Score)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.Mask = GenMask(mask)

	rows, err := tx.Query(ctx, `
		SELECT n, answer_id, coalesce(other_id, 0), coalesce(stat, ''), coalesce(guess_id, 0),
			coalesce(guess_dir, ''), coalesce(correct, false), answered_at IS NOT NULL
		FROM game_rounds WHERE game_id = $1::uuid ORDER BY n`, g.ID)
	if err != nil {
		return nil, err
	}
	g.Rounds, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Round])
	if err != nil {
		return nil, err
	}
	for i, r := range g.Rounds {
		if r.N != i+1 {
			return nil, errors.New("game " + g.ID + ": rounds aren't numbered 1, 2, 3...")
		}
	}
	return g, nil
}

// insertRounds adds new (unanswered) rounds.
func insertRounds(ctx context.Context, tx pgx.Tx, gameID string, rounds []Round) error {
	for _, r := range rounds {
		if _, err := tx.Exec(ctx, `
			INSERT INTO game_rounds (game_id, n, answer_id, other_id) VALUES ($1::uuid, $2, $3, nullif($4::int, 0))`,
			gameID, r.N, r.AnswerID, r.OtherID); err != nil {
			return err
		}
	}
	return nil
}
