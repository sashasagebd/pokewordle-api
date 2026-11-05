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

// Load upserts ps into the pokemon table and returns how many rows changed.
// Rows that already match are left alone, so it's cheap on every start, and
// nothing is ever deleted (an older version may still be running beside this
// one during a deploy). Both versions upsert in id order, so running at the
// same time can't deadlock.
func (s *Store) Load(ctx context.Context, ps []Pokemon) (changed int64, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, p := range ps {
			batch.Queue(`
				INSERT INTO pokemon (id, name, generation, height, weight, types)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (id) DO UPDATE SET
					name = EXCLUDED.name, generation = EXCLUDED.generation,
					height = EXCLUDED.height, weight = EXCLUDED.weight, types = EXCLUDED.types
				WHERE (pokemon.name, pokemon.generation, pokemon.height, pokemon.weight, pokemon.types)
					IS DISTINCT FROM
					(EXCLUDED.name, EXCLUDED.generation, EXCLUDED.height, EXCLUDED.weight, EXCLUDED.types)`,
				p.ID, p.Name, p.Generation, p.Height, p.Weight, p.Types)
		}
		results := tx.SendBatch(ctx, batch)
		for _, p := range ps {
			tag, err := results.Exec()
			if err != nil {
				results.Close()
				return fmt.Errorf("#%d %s: %w", p.ID, p.Name, err)
			}
			changed += tag.RowsAffected()
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
