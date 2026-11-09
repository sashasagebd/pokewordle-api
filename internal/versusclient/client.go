// Package versusclient plays Versus over the API the way the frontend does:
// HTTP for players and rooms, then a room's socket, which it reconnects
// whenever it drops (a deploy closes every socket with 1012). The resilience
// tests and cmd/versuscheck play with it.
package versusclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/sashasagebd/pokewordle-api/internal/live"
)

// A Client is one player.
type Client struct {
	Base  string // the API's URL, e.g. https://pokewordle-api.example.com
	HTTP  *http.Client
	Token string // from NewPlayer; share it to be the same player twice
}

// New returns a client for the API at base, without a player yet.
func New(base string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// A StatusError is the API answering a request with an error.
type StatusError struct {
	Status int
	Msg    string

	retryAfter time.Duration // a 429's Retry-After
}

func (e *StatusError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Msg) }

// post sends a request, waiting out the API's rate limit (429, Retry-After)
// rather than failing: a load test from one address runs into it by design.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	for {
		wait, err := c.postOnce(ctx, path, body, out)
		if wait == 0 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// postOnce sends a request once. A rate-limited one comes back with how long
// to wait before trying again.
func (c *Client) postOnce(ctx context.Context, path string, body, out any) (time.Duration, error) {
	err := c.do(ctx, path, body, out)
	var status *StatusError
	if errors.As(err, &status) && status.Status == http.StatusTooManyRequests {
		return status.retryAfter, err
	}
	return 0, err
}

func (c *Client) do(ctx context.Context, path string, body, out any) error {
	var r *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e struct{ Error string }
		json.NewDecoder(res.Body).Decode(&e)
		se := &StatusError{Status: res.StatusCode, Msg: e.Error}
		if res.StatusCode == http.StatusTooManyRequests {
			secs, _ := strconv.Atoi(res.Header.Get("Retry-After"))
			se.retryAfter = time.Duration(max(secs, 1)) * time.Second
		}
		return se
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// NewPlayer makes the client a new guest.
func (c *Client) NewPlayer(ctx context.Context) error {
	var body struct{ Token string }
	if err := c.post(ctx, "/players", nil, &body); err != nil {
		return err
	}
	c.Token = body.Token
	return nil
}

// CreateRoom makes a room of the player's (gens: none means all) and returns
// its code.
func (c *Client) CreateRoom(ctx context.Context, gens []int) (string, error) {
	var body struct{ Code string }
	err := c.post(ctx, "/rooms", map[string]any{"gens": gens}, &body)
	return body.Code, err
}

// Join joins the room with code (harmless if already in).
func (c *Client) Join(ctx context.Context, code string) error {
	return c.post(ctx, "/rooms/"+url.PathEscape(code)+"/join", nil, nil)
}

// Ticket returns a single-use ticket for the room's socket.
func (c *Client) Ticket(ctx context.Context, code string) (string, error) {
	var body struct{ Ticket string }
	err := c.post(ctx, "/rooms/"+url.PathEscape(code)+"/ticket", nil, &body)
	return body.Ticket, err
}

// A Message is anything the server sends on a socket.
type Message struct {
	Type      string    `json:"type"` // room, guess, answer, error
	Seq       int       `json:"seq"`
	ServerNow time.Time `json:"serverNow"`
	Room      live.View `json:"room"`
	N         int       `json:"n"`
	Correct   bool      `json:"correct"` // answer
	For       string    `json:"for"`     // error
	Error     string    `json:"error"`   // error
	Raw       []byte    `json:"-"`
}

// ErrRoomClosed: the room has closed (or there's no such room), so a socket
// stops reconnecting.
var ErrRoomClosed = errors.New("the room has closed")

// A Socket is a room's socket that reconnects whenever it drops, joining the
// room again first (so a player whose grace ran out is back in).
type Socket struct {
	c    *Client
	code string
	msgs chan Message

	mu    sync.Mutex
	conn  *websocket.Conn
	ready chan struct{} // closed while connected
	room  live.View     // the newest room seen
	seq   int

	// Reconnects counts the times it connected again after a drop.
	Reconnects atomic.Int64

	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Open opens the room's socket, and keeps it open until ctx ends, Close, or
// the room closing.
func (c *Client) Open(ctx context.Context, code string) *Socket {
	ctx, cancel := context.WithCancel(ctx)
	s := &Socket{c: c, code: code, msgs: make(chan Message, 256), ready: make(chan struct{}), cancel: cancel, done: make(chan struct{})}
	go s.run(ctx)
	return s
}

func (s *Socket) run(ctx context.Context) {
	defer close(s.done)
	defer close(s.msgs)
	attempt := 0
	for {
		got, err := s.session(ctx)
		if ctx.Err() != nil {
			s.err = ctx.Err()
			return
		}
		var status *StatusError
		if errors.Is(err, ErrRoomClosed) || websocket.CloseStatus(err) == live.CloseRoomClosed ||
			errors.As(err, &status) && status.Status == http.StatusNotFound {
			s.err = ErrRoomClosed
			return
		}
		if got {
			attempt = 0 // it was working: come back quickly
		}
		// A growing, jittered wait, so a restart doesn't bring everyone back
		// at the same instant.
		wait := time.Duration(float64(min(5*time.Second, 200*time.Millisecond<<attempt)) * (0.5 + rand.Float64()))
		attempt++
		select {
		case <-ctx.Done():
			s.err = ctx.Err()
			return
		case <-time.After(wait):
		}
		s.Reconnects.Add(1)
	}
}

// session joins, connects and reads until the socket drops. got says whether
// any message arrived.
func (s *Socket) session(ctx context.Context) (got bool, err error) {
	if err := s.c.Join(ctx, s.code); err != nil {
		return false, err
	}
	ticket, err := s.c.Ticket(ctx, s.code)
	if err != nil {
		return false, err
	}
	u := "ws" + strings.TrimPrefix(s.c.Base, "http") + "/ws?ticket=" + url.QueryEscape(ticket)
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dialCtx, u, &websocket.DialOptions{HTTPClient: s.c.HTTP})
	cancel()
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.conn = conn
	close(s.ready)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.conn = nil
		s.ready = make(chan struct{})
		s.mu.Unlock()
		conn.CloseNow()
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return got, err
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			return got, err
		}
		m.Raw = data
		got = true
		if m.Type == "room" {
			s.mu.Lock()
			if m.Seq > s.seq {
				s.seq, s.room = m.Seq, m.Room
			}
			s.mu.Unlock()
		}
		select {
		case s.msgs <- m:
		case <-ctx.Done():
			return got, ctx.Err()
		}
	}
}

// Send sends a message, waiting for the socket to be connected. A write to
// a connection that has just dropped is sent again once it's back: moves
// carry their round number, so one that did get through is refused, not
// played twice.
func (s *Socket) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	var failed *websocket.Conn
	for {
		s.mu.Lock()
		conn, ready := s.conn, s.ready
		s.mu.Unlock()
		if conn != nil && conn != failed {
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageText, data)
			cancel()
			if err == nil || ctx.Err() != nil {
				return err
			}
			failed = conn // dropped under us: wait for the next one
			continue
		}
		if conn == failed {
			// Still the dropped one: wait for session to notice.
			select {
			case <-s.done:
				return s.Err()
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}
		select {
		case <-ready:
		case <-s.done:
			return s.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Messages are the socket's messages, in order, across reconnects. It closes
// when the socket stops for good.
func (s *Socket) Messages() <-chan Message { return s.msgs }

// Room returns the newest room seen and its seq (0 before the first).
func (s *Socket) Room() (live.View, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.room, s.seq
}

// Await returns the next message ok accepts, skipping others.
func (s *Socket) Await(ctx context.Context, ok func(Message) bool) (Message, error) {
	for {
		select {
		case m, open := <-s.msgs:
			if !open {
				return Message{}, fmt.Errorf("socket stopped: %w", s.Err())
			}
			if ok(m) {
				return m, nil
			}
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
}

// AwaitRoom returns the room once ok accepts it: the newest seen, if it does
// already, or the next that does.
func (s *Socket) AwaitRoom(ctx context.Context, ok func(live.View) bool) (live.View, error) {
	if v, seq := s.Room(); seq > 0 && ok(v) {
		return v, nil
	}
	m, err := s.Await(ctx, func(m Message) bool { return m.Type == "room" && ok(m.Room) })
	return m.Room, err
}

// Close stops the socket for good.
func (s *Socket) Close() {
	s.cancel()
	<-s.done
}

// Done is closed once the socket has stopped for good; Err then says why.
func (s *Socket) Done() <-chan struct{} { return s.done }

func (s *Socket) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// PokemonNames lists the names of generation gen's Pokémon, from the API at
// base, for guesses.
func PokemonNames(ctx context.Context, base string, gen int) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/pokemon?gens=%d", strings.TrimRight(base, "/"), gen), nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: res.StatusCode, Msg: "listing Pokémon"}
	}
	var ps []struct{ Name string }
	if err := json.NewDecoder(res.Body).Decode(&ps); err != nil {
		return nil, err
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return names, nil
}
