package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/games"
)

// Password endpoints allow this many attempts per client IP per minute.
const passwordAttemptsPerMinute = 10

func accountRoutes(mux *http.ServeMux, c Config) {
	limiter := newRateLimiter(passwordAttemptsPerMinute, time.Minute)
	limited := func(w http.ResponseWriter, r *http.Request) bool {
		if limiter.allow(clientIP(r)) {
			return true
		}
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "Too many tries. Wait a minute and try again.")
		return false
	}

	// GET /me: the player's username (null for a guest) and best scores.
	mux.HandleFunc("GET /me", requirePlayer(c, func(w http.ResponseWriter, r *http.Request, player string) {
		name, err := c.Players.Username(r.Context(), player)
		if err != nil {
			serverError(w, r, err)
			return
		}
		bests, err := c.Games.Bests(r.Context(), player)
		if err != nil {
			serverError(w, r, err)
			return
		}
		var username *string
		if name != "" {
			username = &name
		}
		writeJSON(w, http.StatusOK, map[string]any{"username": username, "best": bests})
	}))

	// POST /me/username {username, password}: a guest claims a name, keeping
	// everything they've played.
	mux.HandleFunc("POST /me/username", requirePlayer(c, func(w http.ResponseWriter, r *http.Request, player string) {
		if !limited(w, r) {
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		err := c.Players.Claim(r.Context(), player, body.Username, body.Password)
		var invalid *auth.InvalidError
		switch {
		case errors.As(err, &invalid):
			writeError(w, http.StatusBadRequest, invalid.Msg)
		case errors.Is(err, auth.ErrNameTaken):
			writeError(w, http.StatusConflict, "That name is taken.")
		case errors.Is(err, auth.ErrAlreadyNamed):
			writeError(w, http.StatusConflict, "You already have a name.")
		case err != nil:
			serverError(w, r, err)
		default:
			writeJSON(w, http.StatusOK, map[string]string{"username": body.Username})
		}
	}))

	// POST /sessions {username, password} logs in and returns a new token.
	// Sent with a guest's token, the guest's games and bests join the
	// account (and the guest is gone).
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		if !limited(w, r) {
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		guest, err := optionalPlayer(c, r)
		if err != nil {
			serverError(w, r, err)
			return
		}
		token, player, name, err := c.Players.Login(r.Context(), body.Username, body.Password)
		if errors.Is(err, auth.ErrBadLogin) {
			writeError(w, http.StatusUnauthorized, "Wrong username or password.")
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		if guest != "" {
			// The login stands even if this fails; the guest's progress
			// just stays with the guest.
			if err := c.Games.Merge(r.Context(), guest, player); err != nil {
				slog.Error("merging a guest into an account", "err", err)
			}
		}
		writeJSON(w, http.StatusCreated, map[string]string{"token": token, "username": name})
	})

	// DELETE /sessions/current logs out: the token stops working.
	mux.HandleFunc("DELETE /sessions/current", requirePlayer(c, func(w http.ResponseWriter, r *http.Request, _ string) {
		token, _ := bearer(r)
		if err := c.Players.Logout(r.Context(), token); err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// GET /leaderboard?mode=&gens=1,2&limit=20: the named players' bests for
	// that mode and exact set of generations, and with a token, the player's
	// own place.
	mux.HandleFunc("GET /leaderboard", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mode, err := games.ParseMode(q.Get("mode"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		gens, err := parseGens(q.Get("gens"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		mask, err := games.MaskOf(gens)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		limit := 20
		if s := q.Get("limit"); s != "" {
			if limit, err = strconv.Atoi(s); err != nil || limit < 1 || limit > 100 {
				writeError(w, http.StatusBadRequest, "limit must be 1 to 100")
				return
			}
		}
		player, err := optionalPlayer(c, r)
		if err != nil {
			serverError(w, r, err)
			return
		}
		board, err := c.Games.Leaderboard(r.Context(), mode, mask, limit, player)
		if err != nil {
			serverError(w, r, err)
			return
		}
		if _, ok := bearer(r); ok {
			noStore(w) // it has the player's own place in it
		} else {
			w.Header().Set("Cache-Control", "public, max-age=30")
		}
		writeJSON(w, http.StatusOK, board)
	})
}
