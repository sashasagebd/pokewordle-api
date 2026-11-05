package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/games"
)

// Players is what the API needs from players and sessions (an *auth.Store).
type Players interface {
	NewPlayer(ctx context.Context) (token, playerID string, err error)
	PlayerFor(ctx context.Context, token string) (string, error)
}

// Games is what the API needs from the games (a *games.Store).
type Games interface {
	Start(ctx context.Context, player string, mode games.Mode, mask games.GenMask) (games.State, error)
	Get(ctx context.Context, player, id string) (games.State, error)
	Guess(ctx context.Context, player, id, name string) (games.Comparison, games.State, error)
	Answer(ctx context.Context, player, id string, a games.Answer) (games.AnswerResult, games.State, error)
	GiveUp(ctx context.Context, player, id string) (games.State, error)
	SilhouetteOf(ctx context.Context, player, id string, n int) (int, error)
}

func gameRoutes(mux *http.ServeMux, c Config) {
	// POST /players makes an anonymous player and returns its token, the only
	// time the token is ever sent.
	mux.HandleFunc("POST /players", func(w http.ResponseWriter, r *http.Request) {
		token, _, err := c.Players.NewPlayer(r.Context())
		if err != nil {
			serverError(w, r, err)
			return
		}
		noStore(w)
		writeJSON(w, http.StatusCreated, map[string]string{"token": token})
	})

	authed := func(pattern string, h func(w http.ResponseWriter, r *http.Request, player string)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				writeError(w, http.StatusUnauthorized, "send a token from POST /players as Authorization: Bearer <token>")
				return
			}
			player, err := c.Players.PlayerFor(r.Context(), strings.TrimSpace(token))
			if errors.Is(err, auth.ErrUnknownToken) {
				writeError(w, http.StatusUnauthorized, "unknown token; get a new one from POST /players")
				return
			}
			if err != nil {
				serverError(w, r, err)
				return
			}
			noStore(w)
			h(w, r, player)
		})
	}

	// POST /games {mode, gens} starts a game.
	authed("POST /games", func(w http.ResponseWriter, r *http.Request, player string) {
		var body struct {
			Mode string `json:"mode"`
			Gens []int  `json:"gens"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		mode, err := games.ParseMode(body.Mode)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		mask, err := games.MaskOf(body.Gens)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		st, err := c.Games.Start(r.Context(), player, mode, mask)
		if err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, st)
	})

	// GET /games/{id}: the game as the player may see it, to pick it up again
	// after a reload.
	authed("GET /games/{id}", func(w http.ResponseWriter, r *http.Request, player string) {
		st, err := c.Games.Get(r.Context(), player, r.PathValue("id"))
		if err != nil {
			gameError(w, r, c, player, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	// POST /games/{id}/guess {name}: a Wordle guess.
	authed("POST /games/{id}/guess", func(w http.ResponseWriter, r *http.Request, player string) {
		var body struct {
			Name string `json:"name"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		result, st, err := c.Games.Guess(r.Context(), player, r.PathValue("id"), body.Name)
		if err != nil {
			gameError(w, r, c, player, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": result, "game": st})
	})

	// POST /games/{id}/answer {n, dir, stat} or {n, name}: a streak answer.
	authed("POST /games/{id}/answer", func(w http.ResponseWriter, r *http.Request, player string) {
		var body games.Answer
		if !readJSON(w, r, &body) {
			return
		}
		result, st, err := c.Games.Answer(r.Context(), player, r.PathValue("id"), body)
		if err != nil {
			gameError(w, r, c, player, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"correct": result.Correct, "answer": result.Answer, "game": st})
	})

	// POST /games/{id}/give-up ends the game as lost and reveals the answer.
	authed("POST /games/{id}/give-up", func(w http.ResponseWriter, r *http.Request, player string) {
		st, err := c.Games.GiveUp(r.Context(), player, r.PathValue("id"))
		if err != nil {
			gameError(w, r, c, player, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	// GET /games/{id}/rounds/{n}/image: a Who's That round's silhouette. The
	// round after the open one is served too, for preloading. An <img> can't
	// send the token, so the frontend fetches it and shows a blob URL.
	authed("GET /games/{id}/rounds/{n}/image", func(w http.ResponseWriter, r *http.Request, player string) {
		n, err := strconv.Atoi(r.PathValue("n"))
		if err != nil || n < 1 {
			writeError(w, http.StatusNotFound, "no such round")
			return
		}
		id, err := c.Games.SilhouetteOf(r.Context(), player, r.PathValue("id"), n)
		if errors.Is(err, games.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such round")
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		png, err := c.Silhouette(id)
		if err != nil {
			serverError(w, r, fmt.Errorf("silhouette of #%d: %w", id, err))
			return
		}
		// A round's Pokémon never changes, so the browser may keep it (only
		// the browser: it's the player's).
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})
}

// gameError answers a failed move.
func gameError(w http.ResponseWriter, r *http.Request, c Config, player string, err error) {
	var input *games.InputError
	switch {
	case errors.As(err, &input):
		writeError(w, http.StatusBadRequest, input.Msg)
	case errors.Is(err, games.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such game")
	case errors.Is(err, games.ErrOver):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, games.ErrWrongRound):
		// Most likely a retry of an answer that went through: send the game
		// as it is now, so the frontend can catch up.
		st, getErr := c.Games.Get(r.Context(), player, r.PathValue("id"))
		if getErr != nil {
			serverError(w, r, getErr)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "game": st})
	default:
		serverError(w, r, err)
	}
}

// readJSON decodes a small JSON body into v, or answers 400 and returns false.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("one JSON object only")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

// noStore keeps a player's answers out of every cache.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}
