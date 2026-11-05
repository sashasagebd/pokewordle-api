package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

// fakeDex serves a few Pokémon from memory.
type fakeDex struct {
	ps      []pokedex.Pokemon
	pingErr error
}

func (f *fakeDex) List(_ context.Context, gens []int) ([]pokedex.Pokemon, error) {
	var out []pokedex.Pokemon
	for _, p := range f.ps {
		if len(gens) == 0 || slices.Contains(gens, p.Generation) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeDex) Get(_ context.Context, key string) (pokedex.Pokemon, error) {
	for _, p := range f.ps {
		if p.Name == key {
			return p, nil
		}
	}
	return pokedex.Pokemon{}, pokedex.ErrNotFound
}

func (f *fakeDex) Ping(context.Context) error { return f.pingErr }

const frontend = "https://pokewordle.example"

func newTestAPI(dex *fakeDex) http.Handler {
	if dex == nil {
		dex = &fakeDex{ps: []pokedex.Pokemon{
			{ID: 1, Name: "bulbasaur", Generation: 1, Height: 7, Weight: 69, Types: []string{"grass", "poison"}},
			{ID: 152, Name: "chikorita", Generation: 2, Height: 9, Weight: 64, Types: []string{"grass"}},
		}}
	}
	return New(Config{Dex: dex, Players: newFakePlayers(), Games: newFakeGames(), Silhouette: pokedex.Silhouette, AllowedOrigins: []string{frontend}})
}

func do(h http.Handler, method, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	if rec := do(newTestAPI(nil), "GET", "/healthz", nil); rec.Code != 200 {
		t.Errorf("healthy: got %d", rec.Code)
	}
	down := newTestAPI(&fakeDex{pingErr: errors.New("connection refused")})
	if rec := do(down, "GET", "/healthz", nil); rec.Code != 503 {
		t.Errorf("database down: got %d, want 503", rec.Code)
	}
}

func TestListPokemon(t *testing.T) {
	h := newTestAPI(nil)
	for target, want := range map[string][]string{
		"/pokemon":             {"bulbasaur", "chikorita"},
		"/pokemon?gens=":       {"bulbasaur", "chikorita"},
		"/pokemon?gens=2":      {"chikorita"},
		"/pokemon?gens=1,%202": {"bulbasaur", "chikorita"},
		"/pokemon?gens=3,1,1":  {"bulbasaur"},
	} {
		rec := do(h, "GET", target, nil)
		if rec.Code != 200 {
			t.Errorf("%s: got %d", target, rec.Code)
			continue
		}
		var ps []pokedex.Pokemon
		if err := json.Unmarshal(rec.Body.Bytes(), &ps); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range ps {
			names = append(names, p.Name)
		}
		if !slices.Equal(names, want) {
			t.Errorf("%s: got %v, want %v", target, names, want)
		}
	}
	for _, bad := range []string{"0", "10", "x", "1,,2"} {
		if rec := do(h, "GET", "/pokemon?gens="+bad, nil); rec.Code != 400 {
			t.Errorf("gens=%s: got %d, want 400", bad, rec.Code)
		}
	}
}

func TestGetPokemon(t *testing.T) {
	h := newTestAPI(nil)
	rec := do(h, "GET", "/pokemon/bulbasaur", nil)
	var p pokedex.Pokemon
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &p) != nil || p.ID != 1 {
		t.Errorf("bulbasaur: got %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "GET", "/pokemon/missingno", nil); rec.Code != 404 {
		t.Errorf("missingno: got %d, want 404", rec.Code)
	}
}

func TestCORS(t *testing.T) {
	h := newTestAPI(nil)

	rec := do(h, "GET", "/pokemon", map[string]string{"Origin": frontend})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != frontend {
		t.Errorf("allowed origin: Allow-Origin = %q", got)
	}

	rec = do(h, "GET", "/pokemon", map[string]string{"Origin": "https://evil.example"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("other origin: Allow-Origin = %q, want none", got)
	}

	preflight := map[string]string{"Origin": frontend, "Access-Control-Request-Method": "POST"}
	rec = do(h, "OPTIONS", "/pokemon", preflight)
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Errorf("preflight: got %d, headers %v", rec.Code, rec.Header())
	}
	if !slices.Contains(rec.Header().Values("Vary"), "Origin") {
		t.Error("responses must vary by Origin")
	}
}
