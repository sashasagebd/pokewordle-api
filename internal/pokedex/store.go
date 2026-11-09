package pokedex

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by Get when no Pokémon matches.
var ErrNotFound = errors.New("no such Pokémon")

// Store reads and writes the pokemon table.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store using pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Load writes ps into the pokemon table and returns how many rows changed.
// Nothing is ever deleted (an older version may still be running beside this
// one during a deploy).
//
// Rows that already match are left alone, and not even locked: Load runs on
// every start, while the version before is still playing games, and a move
// locks the Pokémon its rounds refer to (their foreign keys). An upsert
// (ON CONFLICT DO UPDATE ... WHERE changed) would lock every row it
// conflicts with until the end, changed or not, and deadlock with a move
// holding one Pokémon and wanting another. So new rows are inserted with
// ON CONFLICT DO NOTHING, which doesn't lock, and changed ones updated with
// a plain UPDATE, which locks only the rows it changes.
func (s *Store) Load(ctx context.Context, ps []Pokemon) (changed int64, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, p := range ps {
			batch.Queue(`
				INSERT INTO pokemon (id, name, generation, height, weight, types)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (id) DO NOTHING`,
				p.ID, p.Name, p.Generation, p.Height, p.Weight, p.Types)
			batch.Queue(`
				UPDATE pokemon SET name = $2, generation = $3, height = $4, weight = $5, types = $6
				WHERE id = $1 AND (name, generation, height, weight, types)
					IS DISTINCT FROM ($2::text, $3::smallint, $4::int, $5::int, $6::text[])`,
				p.ID, p.Name, p.Generation, p.Height, p.Weight, p.Types)
		}
		results := tx.SendBatch(ctx, batch)
		for _, p := range ps {
			// The insert, then the update: a new row counts once, as the
			// update finds it matching.
			for range 2 {
				tag, err := results.Exec()
				if err != nil {
					results.Close()
					return fmt.Errorf("#%d %s: %w", p.ID, p.Name, err)
				}
				changed += tag.RowsAffected()
			}
		}
		return results.Close()
	})
	return changed, err
}

// List returns the Pokémon of the given generations (1 to 9) in dex order,
// or all of them if gens is empty.
func (s *Store) List(ctx context.Context, gens []int) ([]Pokemon, error) {
	// pgx sends a nil slice as NULL, not an empty array, and
	// cardinality(NULL) is NULL rather than 0: hence the coalesce.
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, generation, height, weight, types FROM pokemon
		WHERE coalesce(cardinality($1::int[]), 0) = 0 OR generation = ANY($1)
		ORDER BY id`, gens)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Pokemon])
}

// Get returns one Pokémon by dex number ("25") or name ("pikachu", any case).
func (s *Store) Get(ctx context.Context, key string) (Pokemon, error) {
	const cols = `SELECT id, name, generation, height, weight, types FROM pokemon `
	var rows pgx.Rows
	var err error
	if id, convErr := strconv.Atoi(key); convErr == nil {
		rows, err = s.pool.Query(ctx, cols+`WHERE id = $1`, id)
	} else {
		rows, err = s.pool.Query(ctx, cols+`WHERE name = $1`, strings.ToLower(strings.TrimSpace(key)))
	}
	if err != nil {
		return Pokemon{}, err
	}
	p, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Pokemon])
	if errors.Is(err, pgx.ErrNoRows) {
		return Pokemon{}, ErrNotFound
	}
	return p, err
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
