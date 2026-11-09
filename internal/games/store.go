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
// still has going is abandoned (but not a Versus game: that's the room's).
func (s *Store) Start(ctx context.Context, player string, mode Mode, mask GenMask) (State, error) {
	g := s.engine.New(mode, mask)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE games SET status = 'abandoned', updated_at = now()
			WHERE player_id = $1::uuid AND mode = $2 AND status = 'active' AND room_round_id IS NULL`, player, mode); err != nil {
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

// move plays a move on one of player's solo games: see moveIn. A Versus game
// is refused (ErrRoomGame), since only its room may play it.
func (s *Store) move(ctx context.Context, player, id string, play func(*Game) (*Round, error)) (State, error) {
	var st State
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		st, err = s.moveIn(ctx, tx, player, id, false, play)
		return err
	})
	return st, err
}

// moveIn loads the game locked, lets play make a move (which returns the
// round it answered, if any), and saves what changed: that round, rounds
// added after it, and the game's status and score. It runs in tx, which a
// Versus move shares with its room's changes. room says which kind of game
// it must be.
func (s *Store) moveIn(ctx context.Context, tx pgx.Tx, player, id string, room bool, play func(*Game) (*Round, error)) (State, error) {
	g, err := load(ctx, tx, player, id, true)
	if err != nil {
		return State{}, err
	}
	if isRoom := g.RoomRound != ""; isRoom != room {
		if isRoom {
			return State{}, ErrRoomGame
		}
		return State{}, ErrNotFound // a bug in the room store
	}
	before := len(g.Rounds)
	answered, err := play(g)
	if err != nil {
		return State{}, err
	}
	if answered != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE game_rounds SET
				stat = nullif($3, ''), guess_id = nullif($4::int, 0), guess_dir = nullif($5, ''),
				correct = $6, answered_at = now()
			WHERE game_id = $1::uuid AND n = $2`,
			g.ID, answered.N, answered.Stat, answered.GuessID, answered.GuessDir, answered.Correct); err != nil {
			return State{}, err
		}
	}
	if err := insertRounds(ctx, tx, g.ID, g.Rounds[before:]); err != nil {
		return State{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE games SET status = $2, score = $3, updated_at = now() WHERE id = $1::uuid`,
		g.ID, g.Status, g.Score); err != nil {
		return State{}, err
	}
	if improvesBest(g, answered) {
		if err := recordBest(ctx, tx, player, g); err != nil {
			return State{}, err
		}
	}
	return s.engine.State(g), nil
}

// --- Versus ---------------------------------------------------------------
//
// The room store (internal/rooms) plays each player's race as a Wordle game
// here, inside its own transaction, so a guess and the room's record of it
// commit together or not at all.

// FirstAnswer returns the first round's answer of a game drawn from seed:
// a Versus round's Wordle answer, or its first Pokémon in the streak modes.
func (s *Store) FirstAnswer(mode Mode, mask GenMask, seed int64) int {
	return s.engine.NewSeeded(mode, mask, seed).Rounds[0].AnswerID
}

// StartInRound makes player's game for a Versus round, its Pokémon drawn
// from the round's seed, and returns its ID.
func (s *Store) StartInRound(ctx context.Context, tx pgx.Tx, player, roundID string, mode Mode, mask GenMask, seed int64) (string, error) {
	g := s.engine.NewSeeded(mode, mask, seed)
	if err := tx.QueryRow(ctx, `
		INSERT INTO games (player_id, mode, gen_mask, room_round_id, seed) VALUES ($1::uuid, $2, $3, $4::uuid, $5)
		RETURNING id::text`, player, mode, int16(mask), roundID, seed).Scan(&g.ID); err != nil {
		return "", err
	}
	return g.ID, insertRounds(ctx, tx, g.ID, g.Rounds)
}

// GuessInRound plays a guess in player's game for a Versus round. n is the
// guess's number (1 for the first), so a guess sent twice is refused
// (ErrWrongRound) rather than played twice.
func (s *Store) GuessInRound(ctx context.Context, tx pgx.Tx, player, gameID string, n int, name string) (Comparison, State, error) {
	var c Comparison
	st, err := s.moveIn(ctx, tx, player, gameID, true, func(g *Game) (*Round, error) {
		if g.Status == Active && g.open().N != n {
			return nil, ErrWrongRound
		}
		r, comparison, err := s.engine.Guess(g, name)
		c = comparison
		return r, err
	})
	return c, st, err
}

// AnswerInRound plays an answer in player's game for a streak-mode Versus
// round. Its round number makes a resend harmless (ErrWrongRound), as in
// solo.
func (s *Store) AnswerInRound(ctx context.Context, tx pgx.Tx, player, gameID string, a Answer) (AnswerResult, State, error) {
	var res AnswerResult
	st, err := s.moveIn(ctx, tx, player, gameID, true, func(g *Game) (*Round, error) {
		r, result, err := s.engine.Answer(g, a)
		res = result
		return r, err
	})
	return res, st, err
}

// EndRound ends the Versus round's games still going, as lost, which
// reveals their answer.
func (s *Store) EndRound(ctx context.Context, tx pgx.Tx, roundID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE games SET status = 'lost', updated_at = now()
		WHERE room_round_id = $1::uuid AND status = 'active'`, roundID)
	return err
}

// load reads player's game id with its rounds, locking the game's row when
// lock is set. Someone else's game is ErrNotFound, the same as a missing one,
// so game ids can't be probed.
func load(ctx context.Context, tx pgx.Tx, player, id string, lock bool) (*Game, error) {
	if !uuidPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	q := `SELECT id::text, mode, gen_mask, status, score, coalesce(room_round_id::text, ''), coalesce(seed, 0)
		FROM games WHERE id = $1::uuid AND player_id = $2::uuid`
	if lock {
		q += ` FOR UPDATE`
	}
	g := &Game{}
	var mask int16
	err := tx.QueryRow(ctx, q, id, player).Scan(&g.ID, &g.Mode, &mask, &g.Status, &g.Score, &g.RoomRound, &g.Seed)
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
