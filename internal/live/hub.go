package live

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
)

// Close codes a socket can end with, besides the standard ones. 1012
// (service restart) means this instance is stopping: reconnect.
const (
	// CloseRoomClosed: the room has closed (or is gone). Don't reconnect.
	CloseRoomClosed websocket.StatusCode = 4000
	// CloseNotMember: you're not in the room (your grace ran out, or you
	// left). Join it again for a new ticket.
	CloseNotMember websocket.StatusCode = 4001
)

// ListenerName is the application_name of each hub's LISTEN connection.
const ListenerName = "pokewordle-api hub"

// sweepEvery is how often the hub looks for rooms with a deadline passed
// that no timer covers (nobody connected here).
const sweepEvery = time.Second

// A Hub is one instance's sockets, by room.
type Hub struct {
	ctx   context.Context // the app's: ends at shutdown
	pool  *pgxpool.Pool
	rooms *rooms.Store
	games *games.Store

	mu      sync.Mutex
	byRoom  map[string]*roomConns
	closing bool
	sockets sync.WaitGroup // every socket's handler
}

// roomConns are the sockets to one room here. At most one worker at a time
// sends a room's changes to them, so they go out in order.
type roomConns struct {
	id      string
	sockets map[*socket]struct{}
	pending bool // a change to send that the worker hasn't loaded yet
	running bool // the worker is on it
	timer   *time.Timer
}

// NewHub returns a hub for rooms in store, with each race a game in gs. It
// does nothing until Run.
func NewHub(ctx context.Context, pool *pgxpool.Pool, store *rooms.Store, gs *games.Store) *Hub {
	return &Hub{ctx: ctx, pool: pool, rooms: store, games: gs, byRoom: map[string]*roomConns{}}
}

// Run listens for changes and applies deadlines until the hub's context
// ends, then closes every socket with 1012 (service restart), so their
// clients reconnect to the version taking over, and waits for them to be
// done.
func (h *Hub) Run() {
	var wg sync.WaitGroup
	wg.Go(h.listen)
	wg.Go(h.sweep)
	<-h.ctx.Done()
	wg.Wait()

	h.mu.Lock()
	h.closing = true
	var all []*socket
	for _, rc := range h.byRoom {
		for s := range rc.sockets {
			all = append(all, s)
		}
	}
	h.mu.Unlock()
	for _, s := range all {
		s.close(websocket.StatusServiceRestart, "the server is restarting; reconnect")
	}
	h.sockets.Wait()
}

// listen LISTENs on rooms.NotifyChannel on a connection of its own, and has
// each change sent to the sockets here. If the connection drops, it
// reconnects, and resends every room here (it may have missed changes).
func (h *Hub) listen() {
	for {
		err := h.listenOnce()
		if h.ctx.Err() != nil {
			return
		}
		slog.Warn("listening for room changes failed; retrying in 1s", "err", err)
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (h *Hub) listenOnce() error {
	c, err := h.pool.Acquire(h.ctx)
	if err != nil {
		return err
	}
	// Ours alone from now on: a connection still LISTENing mustn't go back
	// to the pool.
	conn := c.Hijack()
	defer conn.Close(context.WithoutCancel(h.ctx))
	// Named, to be told apart from the pool's connections (pg_stat_activity).
	if _, err := conn.Exec(h.ctx, "SET application_name = '"+ListenerName+"'"); err != nil {
		return err
	}
	if _, err := conn.Exec(h.ctx, "LISTEN "+rooms.NotifyChannel); err != nil {
		return err
	}
	h.scheduleAll()
	for {
		n, err := conn.WaitForNotification(h.ctx)
		if err != nil {
			return err
		}
		id, _, _ := strings.Cut(n.Payload, ":")
		h.schedule(id)
	}
}

// sweep ticks rooms whose deadlines have passed. A room with sockets here has
// a timer for its next deadline; this covers the rooms nobody is connected
// to. Every instance sweeps: a second Tick finds nothing to do.
func (h *Hub) sweep() {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
		}
		ids, err := h.rooms.Due(h.ctx)
		if err != nil {
			if h.ctx.Err() == nil {
				slog.Error("finding rooms due", "err", err)
			}
			continue
		}
		for _, id := range ids {
			h.tick(id)
		}
	}
}

// tick applies room id's deadlines. A change is announced, and so comes back
// through listen.
func (h *Hub) tick(id string) {
	if _, _, err := h.rooms.Tick(h.ctx, id); err != nil && !errors.Is(err, rooms.ErrNotFound) && h.ctx.Err() == nil {
		slog.Error("ticking a room", "room", id, "err", err)
	}
}

// register adds s to its room here, and records the player as connected
// when it's their first socket here.
func (h *Hub) register(s *socket) bool {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return false
	}
	rc := h.byRoom[s.roomID]
	if rc == nil {
		rc = &roomConns{id: s.roomID, sockets: map[*socket]struct{}{}}
		h.byRoom[s.roomID] = rc
	}
	first := !rc.has(s.player)
	rc.sockets[s] = struct{}{}
	h.mu.Unlock()

	if first {
		if _, err := h.rooms.Connect(h.ctx, s.player, s.roomID); err != nil {
			s.closeFor(err)
			return true
		}
	}
	// The new socket needs the room even if connecting changed nothing.
	h.schedule(s.roomID)
	return true
}

// unregister takes s out of its room here, and records the player as
// disconnected when it was their last socket here.
func (h *Hub) unregister(s *socket) {
	h.mu.Lock()
	rc := h.byRoom[s.roomID]
	if rc == nil {
		h.mu.Unlock()
		return
	}
	delete(rc.sockets, s)
	last := !rc.has(s.player)
	if len(rc.sockets) == 0 {
		if rc.timer != nil {
			rc.timer.Stop()
		}
		delete(h.byRoom, s.roomID)
	}
	h.mu.Unlock()

	if last {
		h.unregisterPlayer(s)
	}
}

// unregisterPlayer records that s's player has no socket to its room here.
func (h *Hub) unregisterPlayer(s *socket) {
	// Even during shutdown: their grace has to start.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 5*time.Second)
	defer cancel()
	if _, err := h.rooms.Disconnect(ctx, s.player, s.roomID); err != nil && !errors.Is(err, rooms.ErrNotFound) {
		slog.Error("recording a disconnect", "room", s.roomID, "err", err)
	}
}

// playerHere reports whether player has a socket to room id here.
func (h *Hub) playerHere(id, player string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	rc := h.byRoom[id]
	return rc != nil && rc.has(player)
}

func (rc *roomConns) has(player string) bool {
	for s := range rc.sockets {
		if s.player == player {
			return true
		}
	}
	return false
}

// schedule has room id's latest state sent to its sockets here, if it has
// any. Changes that arrive while one is being sent are sent together after.
func (h *Hub) schedule(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rc := h.byRoom[id]
	if rc == nil {
		return
	}
	rc.pending = true
	if !rc.running {
		rc.running = true
		go h.work(rc)
	}
}

func (h *Hub) scheduleAll() {
	h.mu.Lock()
	ids := make([]string, 0, len(h.byRoom))
	for id := range h.byRoom {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.schedule(id)
	}
}

func (h *Hub) work(rc *roomConns) {
	for {
		h.mu.Lock()
		if !rc.pending || h.ctx.Err() != nil {
			rc.running = false
			h.mu.Unlock()
			return
		}
		rc.pending = false
		h.mu.Unlock()
		if err := h.refresh(rc); err != nil && h.ctx.Err() == nil {
			slog.Error("sending a room's change", "room", rc.id, "err", err)
		}
	}
}

// refresh loads the room and sends each socket here what its player may see
// of it, unless it has seen this version already. It also sets the timer for
// the room's next deadline.
func (h *Hub) refresh(rc *roomConns) error {
	room, err := h.rooms.Get(h.ctx, rc.id)
	if errors.Is(err, rooms.ErrNotFound) {
		h.closeAll(rc, CloseRoomClosed, "this room has closed")
		return nil
	}
	if err != nil {
		return err
	}
	states := map[string]games.State{}
	if room.Round != nil {
		for _, p := range room.Round.Players {
			st, err := h.games.Get(h.ctx, p.PlayerID, p.GameID)
			if errors.Is(err, games.ErrNotFound) {
				continue // the player was deleted since
			}
			if err != nil {
				return err
			}
			states[p.PlayerID] = st
		}
	}

	h.mu.Lock()
	sockets := make([]*socket, 0, len(rc.sockets))
	for s := range rc.sockets {
		sockets = append(sockets, s)
	}
	if rc.timer != nil {
		rc.timer.Stop()
		rc.timer = nil
	}
	if d := room.Deadline(); !d.IsZero() && len(rc.sockets) > 0 {
		// A little after, so the database's clock has surely passed it too.
		rc.timer = time.AfterFunc(time.Until(d)+20*time.Millisecond, func() { h.tick(rc.id) })
	}
	h.mu.Unlock()

	now := time.Now()
	healed := map[string]bool{}
	for _, s := range sockets {
		m, ok := room.Member(s.player)
		if !ok {
			s.closeFor(rooms.ErrNotMember)
			continue
		}
		// Connected here but recorded as disconnected: their socket on
		// another instance closed (a deploy). Put it right.
		if !m.DisconnectedAt.IsZero() && !healed[s.player] && room.Status != rooms.Closed {
			healed[s.player] = true
			if _, err := h.rooms.Connect(h.ctx, s.player, room.ID); err != nil {
				s.closeFor(err)
				continue
			}
			// Their sockets here may have closed meanwhile, the disconnect
			// saved before this connect: then they're disconnected after all.
			if !h.playerHere(s.roomID, s.player) {
				h.unregisterPlayer(s)
				continue
			}
		}
		if int64(room.Seq) > s.sentSeq.Load() {
			s.sentSeq.Store(int64(room.Seq))
			s.send(roomMsg{Type: "room", Seq: room.Seq, ServerNow: now, Room: ViewFor(room, states, s.player)})
		}
	}
	if room.Status == rooms.Closed {
		h.closeAll(rc, CloseRoomClosed, "this room has closed")
	}
	return nil
}

func (h *Hub) closeAll(rc *roomConns, code websocket.StatusCode, reason string) {
	h.mu.Lock()
	sockets := make([]*socket, 0, len(rc.sockets))
	for s := range rc.sockets {
		sockets = append(sockets, s)
	}
	h.mu.Unlock()
	for _, s := range sockets {
		s.close(code, reason)
	}
}
