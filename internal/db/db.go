package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoURL is returned by Open when DATABASE_URL is empty.
var ErrNoURL = errors.New("DATABASE_URL is not set (add a database on the app's page, then deploy)")

// Open makes a pool for url. It connects lazily, so it doesn't fail when the
// database is still starting.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	if url == "" {
		return nil, ErrNoURL
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// Not the error's text alone: pgx can echo parts of the URL. Callers
		// must never log the URL itself, since it holds the password.
		return nil, errors.New("DATABASE_URL is not a valid Postgres URL")
	}
	cfg.MaxConns = 8
	return pgxpool.NewWithConfig(ctx, cfg)
}
