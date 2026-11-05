// Command genseed fetches every Pokémon's data from PokéAPI and its sprite
// from PokéAPI's sprites repo, and writes the files the API embeds
// (internal/pokedex/pokemon.json and internal/pokedex/sprites/{id}.png). Run
// it from the repo root when the data should change (a new generation, a
// PokéAPI fix), then review the diff and commit the result:
//
//	go run ./cmd/genseed               # data and sprites
//	go run ./cmd/genseed -only data    # just pokemon.json
//	go run ./cmd/genseed -only sprites # just the sprites
//
// The API never calls PokéAPI itself; it only reads the committed files.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

type config struct {
	out, base, sprites, spriteBase string
	workers                        int
}

func main() {
	var c config
	flag.StringVar(&c.out, "out", "internal/pokedex/pokemon.json", "seed file to write")
	flag.StringVar(&c.base, "base", "https://pokeapi.co/api/v2", "PokéAPI base URL")
	flag.StringVar(&c.sprites, "sprites", "internal/pokedex/sprites", "directory to write sprites to")
	flag.StringVar(&c.spriteBase, "sprite-base", "https://raw.githubusercontent.com/PokeAPI/sprites/master/sprites/pokemon", "base URL of the sprites ({id}.png)")
	flag.IntVar(&c.workers, "workers", 8, "requests at a time (be gentle: PokéAPI is free)")
	only := flag.String("only", "", `"data" or "sprites" to fetch just one of them`)
	flag.Parse()

	ctx := context.Background()
	client := &http.Client{Timeout: 30 * time.Second}
	var err error
	switch *only {
	case "":
		if err = genData(ctx, client, c); err == nil {
			err = genSprites(ctx, client, c)
		}
	case "data":
		err = genData(ctx, client, c)
	case "sprites":
		err = genSprites(ctx, client, c)
	default:
		err = fmt.Errorf(`-only must be "data" or "sprites", not %q`, *only)
	}
	if err != nil {
		slog.Error("genseed failed", "err", err)
		os.Exit(1)
	}
}

// forEachID runs f for ids 1 to pokedex.Count, workers at a time.
func forEachID(ctx context.Context, workers int, f func(ctx context.Context, id int) error) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for id := 1; id <= pokedex.Count; id++ {
		g.Go(func() error {
			if err := f(ctx, id); err != nil {
				return fmt.Errorf("#%d: %w", id, err)
			}
			return nil
		})
	}
	return g.Wait()
}

func genData(ctx context.Context, client *http.Client, c config) error {
	ps := make([]pokedex.Pokemon, pokedex.Count)
	err := forEachID(ctx, c.workers, func(ctx context.Context, id int) error {
		body, err := get(ctx, client, fmt.Sprintf("%s/pokemon/%d", c.base, id))
		if err != nil {
			return err
		}
		p, err := parsePokemon(body, id)
		ps[id-1] = p
		return err
	})
	if err != nil {
		return err
	}
	if err := pokedex.Validate(ps); err != nil {
		return fmt.Errorf("fetched data failed validation: %w", err)
	}
	data, err := pokedex.Encode(ps)
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.out, data, 0o644); err != nil {
		return err
	}
	slog.Info("wrote seed", "file", c.out, "pokemon", len(ps))
	return nil
}

// genSprites downloads every sprite, checks that it's a PNG the silhouette
// code can use, and writes it as {id}.png.
func genSprites(ctx context.Context, client *http.Client, c config) error {
	if err := os.MkdirAll(c.sprites, 0o755); err != nil {
		return err
	}
	err := forEachID(ctx, c.workers, func(ctx context.Context, id int) error {
		body, err := get(ctx, client, fmt.Sprintf("%s/%d.png", c.spriteBase, id))
		if err != nil {
			return err
		}
		if _, err := pokedex.MakeSilhouette(body); err != nil {
			return fmt.Errorf("unusable sprite: %w", err)
		}
		return os.WriteFile(filepath.Join(c.sprites, fmt.Sprintf("%d.png", id)), body, 0o644)
	})
	if err != nil {
		return err
	}
	slog.Info("wrote sprites", "dir", c.sprites, "sprites", pokedex.Count)
	return nil
}

// get fetches url, retrying a few times on network errors, 429 and 5xx.
func get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		body, retry, err := getOnce(ctx, client, url)
		if err == nil || !retry {
			return body, err
		}
		last = err
	}
	return nil, last
}

func getOnce(ctx context.Context, client *http.Client, url string) (body []byte, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", "pokewordle-api-genseed")
	res, err := client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		// 429 and 5xx are worth another try; anything else won't change.
		return nil, res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500,
			fmt.Errorf("%s answered %s", url, res.Status)
	}
	body, err = io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return body, err != nil, err
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

func parsePokemon(body []byte, id int) (pokedex.Pokemon, error) {
	var a apiPokemon
	if err := json.Unmarshal(body, &a); err != nil {
		return pokedex.Pokemon{}, err
	}
	if a.ID != id {
		return pokedex.Pokemon{}, fmt.Errorf("asked for #%d, got #%d", id, a.ID)
	}
	types := make([]string, 2)
	n := 0
	for _, t := range a.Types {
		if t.Slot < 1 || t.Slot > 2 {
			return pokedex.Pokemon{}, fmt.Errorf("unexpected type slot %d", t.Slot)
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
	}, nil
}
