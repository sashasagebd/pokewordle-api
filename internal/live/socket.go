package live

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
)

const (
	// pingEvery keeps a quiet socket alive: Cloudflare drops idle ones
	// after about 100 seconds. A pong not back within pingTimeout ends it.
	pingEvery   = 25 * time.Second
	pingTimeout = 10 * time.Second
	// writeTimeout ends a socket whose client stops reading.
	writeTimeout = 10 * time.Second
	// outBuffer is how many messages may wait to be written. A client that
	// falls this far behind is disconnected (it reconnects for the latest).
	outBuffer = 32
	// Messages a client may send: msgRate a second, with a burst of msgBurst.
	msgRate  = 5
	msgBurst = 10
	// maxMessage is the largest message a client may send, in bytes.
	maxMessage = 1 << 10
)

// Messages from the server.
type (
	// roomMsg is the room as its member may see it, sent on every change.
	roomMsg struct {
		Type      string    `json:"type"` // "room"
		Seq       int       `json:"seq"`
		ServerNow time.Time `json:"serverNow"`
		Room      View      `json:"room"`
	}
	// answerMsg answers a streak-mode answer, to the player only.
	answerMsg struct {
		Type    string          `json:"type"` // "answer"
		N       int             `json:"n"`
		Correct bool            `json:"correct"`
		Answer  pokedex.Pokemon `json:"answer"`
		Game    games.State     `json:"game"`
	}
	// guessMsg answers a guess, to the guesser only.
	guessMsg struct {
		Type   string           `json:"type"` // "guess"
		N      int              `json:"n"`
		Result games.Comparison `json:"result"`
		Game   games.State      `json:"game"`
	}
	// errorMsg is a message that couldn't be acted on. Error is meant for
	// the player.
	errorMsg struct {
		Type  string `json:"type"` // "error"
		For   string `json:"for"`  // the type of the message it answers
		N     int    `json:"n,omitempty"`
		Error string `json:"error"`
	}
)

// inbound is a message from a client.
type inbound struct {
	Type  string `json:"type"`
	Ready bool   `json:"ready"` // "ready"
	Mode  string `json:"mode"`  // "settings" ("" keeps it)
	Gens  []int  `json:"gens"`  // "settings"
	N     int    `json:"n"`     // "guess", "answer"
	Name  string `json:"name"`  // "guess", "answer" (Who's That)
	Dir   string `json:"dir"`   // "answer" (Higher or Lower)
	Stat  string `json:"stat"`  // "answer" (Higher or Lower)
}

// A socket is one client's connection to a room.
type socket struct {
	hub    *Hub
	conn   *websocket.Conn
	player string
	roomID string
	out    chan any
	// sentSeq is the room version this socket has been sent.
	sentSeq   atomic.Int64
	closeOnce sync.Once

	tokens float64 // the message rate limit's bucket
	last   time.Time
}

// Serve runs a socket for player in room id, until it closes. The caller has
// checked who it's for (a ticket) and where it's from (Origin).
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, player, roomID string) {
	// Counted under the lock with the check, so Run's wait for sockets never
	// misses one starting.
	h.mu.Lock()
	closing := h.closing || h.ctx.Err() != nil
	if !closing {
		h.sockets.Add(1)
	}
	h.mu.Unlock()
	if closing {
		http.Error(w, "the server is restarting; reconnect", http.StatusServiceUnavailable)
		return
	}
	defer h.sockets.Done()

	// The server's read and write timeouts are for requests: on a socket
	// they would cut it off after seconds.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The API checked Origin against its own list: the library's check
		// only compares hosts.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return // Accept has answered
	}
	conn.SetReadLimit(maxMessage)

	// Not ended by the app's shutdown, which would drop the connection
	// mid-read: Run closes each socket with 1012 instead, so its client
	// knows to reconnect.
	ctx, cancel := context.WithCancel(context.WithoutCancel(h.ctx))
	defer cancel()
	s := &socket{hub: h, conn: conn, player: player, roomID: roomID,
		out: make(chan any, outBuffer), tokens: msgBurst, last: time.Now()}
	go s.writeLoop(ctx)
	go s.pingLoop(ctx)

	if !h.register(s) {
		conn.Close(websocket.StatusServiceRestart, "the server is restarting; reconnect")
		return
	}
	s.readLoop(ctx)
	cancel()
	h.unregister(s)
	conn.CloseNow()
}

func (s *socket) readLoop(ctx context.Context) {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		var m inbound
		if err := json.Unmarshal(data, &m); err != nil {
			s.send(errorMsg{Type: "error", Error: "Messages are JSON objects with a type."})
			continue
		}
		if !s.allow() {
			s.send(errorMsg{Type: "error", For: m.Type, N: m.N, Error: "Slow down a moment."})
			continue
		}
		s.handle(ctx, m)
	}
}

// handle acts on one message. A socket's messages are handled one at a time,
// in order.
func (s *socket) handle(ctx context.Context, m inbound) {
	store := s.hub.rooms
	var err error
	switch m.Type {
	case "ready":
		_, err = store.SetReady(ctx, s.player, s.roomID, m.Ready)
	case "settings":
		err = s.settings(ctx, m)
	case "start":
		_, err = store.Start(ctx, s.player, s.roomID)
	case "guess":
		var res rooms.GuessResult
		if res, _, err = store.Guess(ctx, s.player, s.roomID, m.N, m.Name); err == nil {
			s.send(guessMsg{Type: "guess", N: m.N, Result: res.Comparison, Game: res.Game})
		}
	case "answer":
		var res rooms.AnswerResult
		a := games.Answer{N: m.N, Dir: m.Dir, Stat: m.Stat, Name: m.Name}
		if res, _, err = store.Answer(ctx, s.player, s.roomID, a); err == nil {
			s.send(answerMsg{Type: "answer", N: m.N, Correct: res.Correct, Answer: res.Answer, Game: res.Game})
		}
	case "forfeit":
		_, err = store.Forfeit(ctx, s.player, s.roomID)
	case "leave":
		if _, err = store.Leave(ctx, s.player, s.roomID); err == nil {
			s.close(websocket.StatusNormalClosure, "you left the room")
			return
		}
	default:
		err = &games.InputError{Msg: "Unknown message type."}
	}
	if err == nil {
		return
	}
	msg, ok := playerMessage(err)
	if !ok {
		if ctx.Err() != nil {
			return
		}
		slog.Error("socket message failed", "type", m.Type, "room", s.roomID, "err", err)
		msg = "Something went wrong."
	}
	s.send(errorMsg{Type: "error", For: m.Type, N: m.N, Error: msg})
	if errors.Is(err, rooms.ErrNotMember) || errors.Is(err, rooms.ErrClosed) {
		s.closeFor(err)
	}
}

// settings changes the room's mode (unless m leaves it out) and generations.
func (s *socket) settings(ctx context.Context, m inbound) error {
	var mode games.Mode
	if m.Mode != "" {
		var err error
		if mode, err = games.ParseMode(m.Mode); err != nil {
			return &games.InputError{Msg: err.Error()}
		}
	}
	mask, err := games.MaskOf(m.Gens)
	if err != nil {
		return &games.InputError{Msg: err.Error()}
	}
	_, err = s.hub.rooms.SetSettings(ctx, s.player, s.roomID, mode, mask)
	return err
}

// playerMessage returns what to tell the player about err, if it's their
// mistake rather than a failure.
func playerMessage(err error) (string, bool) {
	var input *games.InputError
	switch {
	case errors.As(err, &input):
		return input.Msg, true
	case rooms.IsRuleError(err), errors.Is(err, games.ErrWrongRound), errors.Is(err, games.ErrOver):
		return err.Error(), true
	}
	return "", false
}

// allow takes a token from the socket's message bucket.
func (s *socket) allow() bool {
	now := time.Now()
	s.tokens = math.Min(msgBurst, s.tokens+now.Sub(s.last).Seconds()*msgRate)
	s.last = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

// send queues msg. A client too far behind is disconnected rather than
// waited for.
func (s *socket) send(msg any) {
	select {
	case s.out <- msg:
	default:
		s.close(websocket.StatusPolicyViolation, "too far behind; reconnect")
	}
}

func (s *socket) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-s.out:
			data, err := json.Marshal(msg)
			if err != nil {
				slog.Error("encoding a socket message", "err", err)
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err = s.conn.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				s.conn.CloseNow()
				return
			}
		}
	}
}

func (s *socket) pingLoop(ctx context.Context) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := s.conn.Ping(pctx)
		cancel()
		if err != nil {
			s.conn.CloseNow()
			return
		}
	}
}

// close ends the socket with code (once; later calls do nothing). It
// doesn't wait for the closing handshake.
func (s *socket) close(code websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		go func() {
			// Let what's queued go first (the room as it closed, say).
			deadline := time.Now().Add(time.Second)
			for len(s.out) > 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			s.conn.Close(code, reason)
		}()
	})
}

// closeFor ends the socket for err from the room store: the room has closed,
// or the player isn't in it.
func (s *socket) closeFor(err error) {
	switch {
	case errors.Is(err, rooms.ErrClosed), errors.Is(err, rooms.ErrNotFound):
		s.close(CloseRoomClosed, "this room has closed")
	case errors.Is(err, rooms.ErrNotMember):
		s.close(CloseNotMember, "you're not in this room; join it again")
	default:
		if s.hub.ctx.Err() == nil {
			slog.Error("socket failed", "room", s.roomID, "err", err)
		}
		s.close(websocket.StatusInternalError, "something went wrong; reconnect")
	}
}
