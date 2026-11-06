// Command pokewordle-api is the backend for pokewordle. It serves Pokémon
// data from its own Postgres (seeded from a file embedded in the binary) and
// runs the games server-side, so scores can't be faked.
//
// It runs on the ops-dashboard platform: port 8080, health check /healthz,
// DATABASE_URL from the app's database.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/api"
	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/cleanup"
	"github.com/sashasagebd/pokewordle-api/internal/db"
	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

const defaultOrigins = "https://pokewordle.sashasagebd.com,https://pokewordle.sashasagebd.workers.dev,http://localhost:5173"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("pokewordle-api exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Docker stops a container with SIGTERM, then SIGKILL after 10s.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	seed, err := pokedex.Seed()
	if err != nil {
		return err // a bad seed file is a build mistake; never start with it
	}
	if err := pokedex.CheckSprites(); err != nil {
		return err // likewise
	}

	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	dex := pokedex.NewStore(pool)

	// The database may still be starting (the first deploy after adding it),
	// so retry until it answers. Until then nothing listens, so the health
	// check fails and the platform keeps the previous version live.
	if err := retry(ctx, "setting up the database", func() error {
		applied, err := db.Migrate(ctx, pool)
		if err != nil {
			return err
		}
		changed, err := dex.Load(ctx, seed)
		if err != nil {
			return err
		}
		slog.Info("database ready", "migrations_applied", applied, "pokemon_changed", changed)
		return nil
	}); err != nil {
		return err
	}

	// Tidies the database hourly, starting a few minutes in (so a deploy's
	// two versions don't both start on it; the lock decides anyway). It stops
	// with ctx, before the pool closes.
	var background sync.WaitGroup
	defer background.Wait()
	defer stop() // runs first: however run returns, the background work ends
	background.Go(func() { cleanup.Run(ctx, pool, cleanup.Defaults, 3*time.Minute, time.Hour) })

	addr := ":" + envOr("PORT", "8080")
	srv := &http.Server{
		Addr: addr,
		Handler: api.New(api.Config{
			Dex:            dex,
			Players:        auth.NewStore(pool),
			Games:          games.NewStore(pool, &games.Engine{Dex: games.NewDex(seed)}),
			Silhouette:     pokedex.Silhouette,
			AllowedOrigins: splitList(envOr("ALLOWED_ORIGINS", defaultOrigins)),
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	shutDown := make(chan struct{})
	go func() {
		defer close(shutDown)
		<-ctx.Done()
		slog.Info("shutting down; finishing requests in progress")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "err", err)
		}
	}()

	slog.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutDown
	return nil
}

// retry calls f until it succeeds or ctx ends, waiting 2s between tries.
func retry(ctx context.Context, what string, f func() error) error {
	for {
		err := f()
		if err == nil {
			return nil
		}
		slog.Warn(what+" failed; retrying in 2s", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// splitList splits a comma-separated list, dropping blanks and trailing
// slashes (an origin never has one, but it's an easy typo in an env var).
func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimRight(strings.TrimSpace(part), "/"); part != "" {
			out = append(out, part)
		}
	}
	return out
}
