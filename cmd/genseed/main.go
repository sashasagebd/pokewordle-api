// Command genseed fetches every Pokémon's data from PokéAPI and writes the
// seed file the API embeds (internal/pokedex/pokemon.json). Run it from the
// repo root when the data should change (a new generation, a PokéAPI fix),
// then commit the result:
//
//	go run ./cmd/genseed
//
// The API never calls PokéAPI itself; it only reads the committed file.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

func main() {
	out := flag.String("out", "internal/pokedex/pokemon.json", "file to write")
	base := flag.String("base", "https://pokeapi.co/api/v2", "PokéAPI base URL")
	workers := flag.Int("workers", 8, "requests at a time (be gentle: PokéAPI is free)")
	flag.Parse()

	if err := run(context.Background(), *base, *out, *workers); err != nil {
		slog.Error("genseed failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, base, out string, workers int) error {
	client := &http.Client{Timeout: 30 * time.Second}
	ps := make([]pokedex.Pokemon, pokedex.Count)

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for id := 1; id <= pokedex.Count; id++ {
		g.Go(func() error {
			p, err := fetch(ctx, client, base, id)
			if err != nil {
				return fmt.Errorf("#%d: %w", id, err)
			}
			ps[id-1] = p
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	if err := pokedex.Validate(ps); err != nil {
		return fmt.Errorf("fetched data failed validation: %w", err)
	}
	data, err := pokedex.Encode(ps)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	slog.Info("wrote seed", "file", out, "pokemon", len(ps))
	return nil
}

// apiPokemon is the part of PokéAPI's /pokemon/{id} response the seed uses.
type apiPokemon struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Height int    `json:"height"`
	Weight int    `json:"weight"`
	Types  []struct {
		Slot int `json:"slot"`
		Type struct {
			Name string `json:"name"`
		} `json:"type"`
	} `json:"types"`
}

func fetch(ctx context.Context, client *http.Client, base string, id int) (pokedex.Pokemon, error) {
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return pokedex.Pokemon{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		p, retry, err := fetchOnce(ctx, client, base, id)
		if err == nil || !retry {
			return p, err
		}
		last = err
	}
	return pokedex.Pokemon{}, last
}

func fetchOnce(ctx context.Context, client *http.Client, base string, id int) (p pokedex.Pokemon, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/pokemon/%d", base, id), nil)
	if err != nil {
		return p, false, err
	}
	req.Header.Set("User-Agent", "pokewordle-api-genseed")
	res, err := client.Do(req)
	if err != nil {
		return p, true, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		// 429 and 5xx are worth another try; anything else won't change.
		return p, res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500,
			fmt.Errorf("PokéAPI answered %s", res.Status)
	}
	var a apiPokemon
	if err := json.NewDecoder(res.Body).Decode(&a); err != nil {
		return p, true, err
	}
	if a.ID != id {
		return p, false, fmt.Errorf("asked for #%d, got #%d", id, a.ID)
	}
	types := make([]string, 2)
	n := 0
	for _, t := range a.Types {
		if t.Slot < 1 || t.Slot > 2 {
			return p, false, fmt.Errorf("unexpected type slot %d", t.Slot)
		}
		types[t.Slot-1] = t.Type.Name
		n++
	}
	return pokedex.Pokemon{
		ID:         a.ID,
		Name:       a.Name,
		Generation: pokedex.GenerationOf(a.ID),
		Height:     a.Height,
		Weight:     a.Weight,
		Types:      types[:n],
	}, false, nil
}
