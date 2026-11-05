// Package dbtest gives tests a migrated database.
package dbtest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sashasagebd/pokewordle-api/internal/db"
)

// Pool connects to DATABASE_URL and migrates it, or skips the test when
// DATABASE_URL isn't set (CI has a Postgres; a dev PC may not). Tests share
// the database, so they must not assume it starts empty.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
