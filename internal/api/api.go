// Package api is the HTTP API pokewordle's frontend calls.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

// Pokedex is what the API needs from the Pokémon data (a *pokedex.Store).
type Pokedex interface {
	List(ctx context.Context, gens []int) ([]pokedex.Pokemon, error)
	Get(ctx context.Context, key string) (pokedex.Pokemon, error)
	Ping(ctx context.Context) error
}

// Config is what the API is built from.
type Config struct {
	Dex     Pokedex
	Players Players
	Games   Games
	// Silhouette renders a Pokémon's silhouette (pokedex.Silhouette).
	Silhouette func(id int) ([]byte, error)
	// AllowedOrigins are the browser origins that may call the API (CORS),
	// e.g. "https://pokewordle.sashasagebd.com".
	AllowedOrigins []string
}

// New returns the API's handler.
func New(c Config) http.Handler {
	mux := http.NewServeMux()
	dex := c.Dex

	// The platform's health check: the app is up and its database answers.
	// A version that can't reach the database never goes live.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := dex.Ping(ctx); err != nil {
			slog.Warn("health check: database unreachable", "err", err)
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})

	// GET /pokemon?gens=1,2 lists Pokémon (all generations without gens).
	mux.HandleFunc("GET /pokemon", func(w http.ResponseWriter, r *http.Request) {
		gens, err := parseGens(r.URL.Query().Get("gens"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ps, err := dex.List(r.Context(), gens)
		if err != nil {
			serverError(w, r, err)
			return
		}
		cacheable(w)
		writeJSON(w, http.StatusOK, ps)
	})

	// GET /pokemon/{key}: one Pokémon by dex number or name.
	mux.HandleFunc("GET /pokemon/{key}", func(w http.ResponseWriter, r *http.Request) {
		p, err := dex.Get(r.Context(), r.PathValue("key"))
		if errors.Is(err, pokedex.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such Pokémon")
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		cacheable(w)
		writeJSON(w, http.StatusOK, p)
	})

	gameRoutes(mux, c)
	return cors(c.AllowedOrigins, mux)
}

// parseGens reads a comma-separated list of generation numbers (1 to 9).
// Empty means all generations, and so does nil in the result.
func parseGens(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var gens []int
	for part := range strings.SplitSeq(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > len(pokedex.Generations) {
			return nil, errors.New("gens must be generation numbers from 1 to 9, separated by commas")
		}
		if !slices.Contains(gens, n) {
			gens = append(gens, n)
		}
	}
	return gens, nil
}

// cacheable lets browsers keep reference data for an hour.
func cacheable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("writing response", "err", err)
	}
}

// writeError answers {"error": msg}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the client went away; nothing to answer
	}
	slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "something went wrong")
}
