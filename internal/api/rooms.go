package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
)

// Rooms is what the API needs from Versus rooms (a *rooms.Store). Everything
// after joining happens over the room's socket.
type Rooms interface {
	Create(ctx context.Context, player string, mask games.GenMask) (*rooms.Room, error)
	IDOf(ctx context.Context, code string) (string, error)
	Join(ctx context.Context, player, id string) (*rooms.Room, error)
	Ticket(ctx context.Context, player, code string) (ticket, roomID string, err error)
	Redeem(ctx context.Context, ticket string) (player, roomID string, err error)
	Queue(ctx context.Context, player string, mode games.Mode, mask games.GenMask) (code string, err error)
	Unqueue(ctx context.Context, player string) error
}

// Sockets runs a room's sockets (a *live.Hub).
type Sockets interface {
	Serve(w http.ResponseWriter, r *http.Request, player, roomID string)
}

func roomRoutes(mux *http.ServeMux, c Config) {
	if c.Rooms == nil {
		return // a test API without Versus
	}
	authed := func(pattern string, h func(w http.ResponseWriter, r *http.Request, player string)) {
		mux.HandleFunc(pattern, requirePlayer(c, h))
	}

	// POST /rooms {gens} makes a room with the player as host.
	authed("POST /rooms", func(w http.ResponseWriter, r *http.Request, player string) {
		var body struct {
			Gens []int `json:"gens"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		mask, err := games.MaskOf(body.Gens)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		room, err := c.Rooms.Create(r.Context(), player, mask)
		if err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"code": room.Code})
	})

	// POST /rooms/{code}/join joins a room (again, harmlessly, if you're in).
	authed("POST /rooms/{code}/join", func(w http.ResponseWriter, r *http.Request, player string) {
		id, err := c.Rooms.IDOf(r.Context(), r.PathValue("code"))
		if err == nil {
			_, err = c.Rooms.Join(r.Context(), player, id)
		}
		if err != nil {
			roomError(w, r, err)
			return
		}
		code, _ := rooms.ParseCode(r.PathValue("code"))
		writeJSON(w, http.StatusOK, map[string]string{"code": code})
	})

	// POST /rooms/{code}/ticket: a single-use ticket for the room's socket
	// (a WebSocket can't send the Authorization header).
	authed("POST /rooms/{code}/ticket", func(w http.ResponseWriter, r *http.Request, player string) {
		ticket, _, err := c.Rooms.Ticket(r.Context(), player, r.PathValue("code"))
		if err != nil {
			roomError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"ticket": ticket})
	})

	// POST /queue {mode, gens}: quick match. Waits for an opponent, or keeps
	// waiting: ask again every couple of seconds (one who stops counts as
	// gone). {"status": "waiting"}, or {"status": "matched", "code"} once
	// there's a room, which starts by itself once both are connected.
	authed("POST /queue", func(w http.ResponseWriter, r *http.Request, player string) {
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
		code, err := c.Rooms.Queue(r.Context(), player, mode, mask)
		if err != nil {
			serverError(w, r, err)
			return
		}
		if code == "" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "waiting"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "matched", "code": code})
	})

	// DELETE /queue stops waiting (204).
	authed("DELETE /queue", func(w http.ResponseWriter, r *http.Request, player string) {
		if err := c.Rooms.Unqueue(r.Context(), player); err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// GET /ws?ticket= opens a room's socket.
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		// CORS doesn't cover WebSockets: any page can open one, so the
		// origin is checked here. No Origin means not a browser, which the
		// ticket alone covers.
		if origin := r.Header.Get("Origin"); origin != "" && !slices.Contains(c.AllowedOrigins, origin) {
			writeError(w, http.StatusForbidden, "origin not allowed")
			return
		}
		player, roomID, err := c.Rooms.Redeem(r.Context(), r.URL.Query().Get("ticket"))
		if errors.Is(err, rooms.ErrBadTicket) {
			writeError(w, http.StatusUnauthorized, "unknown or used ticket; get one from POST /rooms/{code}/ticket")
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		c.Sockets.Serve(w, r, player, roomID)
	})
}

// roomError answers a failed room request.
func roomError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, rooms.ErrNotFound), errors.Is(err, rooms.ErrClosed):
		writeError(w, http.StatusNotFound, "No room has that code.")
	case errors.Is(err, rooms.ErrFull):
		writeError(w, http.StatusConflict, "That room is full.")
	case errors.Is(err, rooms.ErrNotMember):
		writeError(w, http.StatusForbidden, "Join the room first.")
	default:
		serverError(w, r, err)
	}
}
