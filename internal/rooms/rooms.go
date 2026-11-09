// Package rooms has the rules of Versus: rooms friends join with a code,
// rounds where everyone races at the same Wordle, and what happens when
// someone disconnects.
//
// It's pure: no database and no clock. Every change takes the time it
// happens at, so the store can use the database's clock and tests any. There
// are no timers inside either: Advance applies whatever deadlines have
// passed, and every change calls it first, so a room is right however late a
// timer fires. Deadline says when the next one is due.
//
// Who guessed what, and whether it was right, is the games package's
// business: each player's race is an ordinary Wordle game. A room only hears
// that a player guessed, and whether they solved it.
package rooms

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/games"
)

const (
	// MaxMembers is how many players fit in a room.
	MaxMembers = 4
	// MinPlayers is how many it takes to start a round.
	MinPlayers = 2
	// Countdown is the time between starting a round and playing it.
	Countdown = 3 * time.Second
	// RoundLength is the longest a round lasts, from the end of its
	// countdown.
	RoundLength = 5 * time.Minute
	// FinishWindow is how long the others have once someone solves it, so
	// the winner isn't kept waiting.
	FinishWindow = time.Minute
	// Grace is how long a disconnected member has to come back before
	// forfeiting the round they're playing, or leaving the lobby.
	Grace = 30 * time.Second
	// IdleTimeout closes a lobby nothing has happened in.
	IdleTimeout = 30 * time.Minute
)

// A Status is where a room is.
type Status string

const (
	Lobby   Status = "lobby"   // between rounds: joining, readying, settings
	Playing Status = "playing" // a round is on (its countdown included)
	Closed  Status = "closed"  // everyone left, or it sat idle; it's done
)

// Errors a change can fail with. Their messages are meant for the player.
var (
	ErrClosed     = errors.New("this room has closed")
	ErrFull       = errors.New("this room is full")
	ErrNotMember  = errors.New("you're not in this room")
	ErrNotHost    = errors.New("only the host can do that")
	ErrInRound    = errors.New("wait for the round to end")
	ErrTooFew     = errors.New("it takes at least 2 players to start")
	ErrNotReady   = errors.New("everyone has to be ready first")
	ErrNoRound    = errors.New("there's no round being played")
	ErrNotStarted = errors.New("the round hasn't started yet")
	ErrNotPlaying = errors.New("you're not playing this round; you're in the next one")
	ErrFinished   = errors.New("you've already finished this round")
	ErrWrongMove  = errors.New("this round isn't played that way")
)

var ruleErrors = []error{
	ErrClosed, ErrFull, ErrNotMember, ErrNotHost, ErrInRound, ErrTooFew,
	ErrNotReady, ErrNoRound, ErrNotStarted, ErrNotPlaying, ErrFinished, ErrWrongMove,
}

// IsRuleError reports whether err is the rules refusing a change: the
// player's mistake, with a message meant for them, rather than a failure.
func IsRuleError(err error) bool {
	for _, e := range ruleErrors {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// A Room is a lobby and its rounds.
type Room struct {
	ID   string
	Code string
	// Host is the member who picks the settings and starts rounds: whoever
	// made the room, then the longest-present member. "" once it's empty.
	Host string
	// Mode is the game the next round plays, Mask its generations.
	Mode    games.Mode
	Mask    games.GenMask
	Status  Status
	Members []Member // in the order they joined
	// Round is the round being played, or else the last one (for its
	// results). Nil before the first.
	Round *Round
	// LastActivity is when a member last did something.
	LastActivity time.Time
	// Seq counts the room's saved changes. The store bumps it; the rules
	// don't use it. A client that has seen the latest seq is up to date.
	Seq int
	// Quick marks a quick-match room: its players queued for it, which is
	// being ready, so its first round starts by itself once they're all
	// connected (StartQuick). After that it's like any room.
	Quick bool
}

// A Member is a player in a room.
type Member struct {
	PlayerID string
	Name     string
	Ready    bool
	JoinedAt time.Time
	// DisconnectedAt is when the member's last socket closed: zero while
	// they're connected. A member who just joined counts as disconnected
	// until their socket opens.
	DisconnectedAt time.Time
}

func (m *Member) connected() bool { return m.DisconnectedAt.IsZero() }

// graceEnds is when a disconnected member runs out of grace.
func (m *Member) graceEnds() time.Time { return m.DisconnectedAt.Add(Grace) }

// A Round is one race.
//
// In Wordle everyone looks for the same Pokémon, and the first to find it
// wins. The streak modes (Higher or Lower, Who's That) are survival races:
// everyone meets the same Pokémon in the same order, one wrong answer ends a
// run, and the longest streak wins, ties going to whoever got there first.
type Round struct {
	ID   string // the store's; "" until saved
	N    int    // 1, 2, ... within the room
	Mode games.Mode
	// AnswerID is Wordle's answer; in the streak modes, the first Pokémon.
	AnswerID int
	// Seed is what every player's Pokémon are drawn from, so they meet the
	// same ones (the store's: the rules don't use it).
	Seed int64
	// StartsAt is the end of the countdown, EndsAt the latest it can end
	// (brought forward by the first solve).
	StartsAt, EndsAt time.Time
	// FinishedAt is when it ended: zero while it's being played.
	FinishedAt time.Time
	// Players are the members when it started, in the order they joined.
	// Someone who joins during a round plays the next one.
	Players []Player
}

// A Player is a member's race in one round.
type Player struct {
	PlayerID string
	Name     string // as a member when the round started
	// GameID is the player's game for the round, filled in by the store.
	GameID string
	// Wordle: their guesses so far, and when they solved it.
	Guesses  int
	SolvedAt time.Time
	// Streak modes: their streak, when they reached it, and when a wrong
	// answer ended their run.
	Score    int
	ScoredAt time.Time
	OutAt    time.Time
	// ForfeitedAt is when they gave up, or their grace ran out. In a streak
	// mode, that ends the run keeping the streak.
	ForfeitedAt time.Time
}

// Done reports whether the player has finished: solved it, been knocked out
// or forfeited.
func (p *Player) Done() bool { return !p.doneAt().IsZero() }

func (p *Player) doneAt() time.Time {
	for _, t := range []time.Time{p.SolvedAt, p.OutAt, p.ForfeitedAt} {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// New makes a room with host in it.
func New(id, code, host, name string, mask games.GenMask, now time.Time) *Room {
	return &Room{
		ID: id, Code: code, Host: host, Mode: games.Wordle, Mask: mask, Status: Lobby,
		Members:      []Member{{PlayerID: host, Name: name, JoinedAt: now, DisconnectedAt: now}},
		LastActivity: now,
	}
}

func (r *Room) member(player string) *Member {
	for i := range r.Members {
		if r.Members[i].PlayerID == player {
			return &r.Members[i]
		}
	}
	return nil
}

// Member returns player's membership, or false if they're not in the room.
func (r *Room) Member(player string) (Member, bool) {
	if m := r.member(player); m != nil {
		return *m, true
	}
	return Member{}, false
}

// player returns player's race in the round being played, or nil.
func (r *Room) player(player string) *Player {
	if r.Status != Playing {
		return nil
	}
	return r.Round.player(player)
}

func (rd *Round) player(player string) *Player {
	for i := range rd.Players {
		if rd.Players[i].PlayerID == player {
			return &rd.Players[i]
		}
	}
	return nil
}

// remove takes player out of the room, handing the host on if it was them.
func (r *Room) remove(player string) {
	r.Members = slices.DeleteFunc(r.Members, func(m Member) bool { return m.PlayerID == player })
	if r.Host == player {
		r.Host = ""
		if len(r.Members) > 0 {
			r.Host = r.Members[0].PlayerID
		}
	}
}

// open does what every change does first: brings the room up to now, and
// refuses if it has closed.
func (r *Room) open(now time.Time) error {
	r.Advance(now)
	if r.Status == Closed {
		return ErrClosed
	}
	return nil
}

// openAs is open for a change only a member can make.
func (r *Room) openAs(player string, now time.Time) (*Member, error) {
	if err := r.open(now); err != nil {
		return nil, err
	}
	m := r.member(player)
	if m == nil {
		return nil, ErrNotMember
	}
	return m, nil
}

// changed records activity and applies anything the change settled (like a
// round with nobody left playing).
func (r *Room) changed(now time.Time) {
	r.LastActivity = now
	r.Advance(now)
}

// Join adds player to the room. Joining a room you're in does nothing, so a
// retried request is harmless. During a round, the newcomer waits for the
// next one.
func (r *Room) Join(player, name string, now time.Time) error {
	if err := r.open(now); err != nil {
		return err
	}
	if r.member(player) != nil {
		return nil
	}
	if len(r.Members) >= MaxMembers {
		return ErrFull
	}
	r.Members = append(r.Members, Member{PlayerID: player, Name: name, JoinedAt: now, DisconnectedAt: now})
	r.changed(now)
	return nil
}

// Leave takes player out of the room, forfeiting the round they're playing.
func (r *Room) Leave(player string, now time.Time) error {
	if _, err := r.openAs(player, now); err != nil {
		return err
	}
	if p := r.player(player); p != nil && !p.Done() {
		p.ForfeitedAt = now
	}
	r.remove(player)
	r.changed(now)
	return nil
}

// Connect records that player's socket opened. A member whose grace ran out
// is no longer in the room, and has to join again.
func (r *Room) Connect(player string, now time.Time) error {
	m, err := r.openAs(player, now)
	if err != nil {
		return err
	}
	m.DisconnectedAt = time.Time{}
	r.changed(now)
	return nil
}

// Disconnect records that player's last socket closed: their grace starts,
// and they're no longer ready.
func (r *Room) Disconnect(player string, now time.Time) {
	m, err := r.openAs(player, now)
	if err != nil {
		return // gone already: nothing to record
	}
	if m.connected() {
		m.DisconnectedAt = now
	}
	m.Ready = false
	r.Advance(now)
}

// SetReady marks player ready for the next round, or not.
func (r *Room) SetReady(player string, ready bool, now time.Time) error {
	m, err := r.openAs(player, now)
	if err != nil {
		return err
	}
	if r.Status != Lobby {
		return ErrInRound
	}
	m.Ready = ready
	r.changed(now)
	return nil
}

// SetGens changes the generations the next round is played in: SetSettings
// keeping the mode.
func (r *Room) SetGens(player string, mask games.GenMask, now time.Time) error {
	return r.SetSettings(player, "", mask, now)
}

// SetSettings changes the mode ("" keeps it) and generations the next round
// is played in. Only the host can, in the lobby, and a change unreadies
// everyone: nobody starts a round they didn't agree to.
func (r *Room) SetSettings(player string, mode games.Mode, mask games.GenMask, now time.Time) error {
	if _, err := r.openAs(player, now); err != nil {
		return err
	}
	if r.Host != player {
		return ErrNotHost
	}
	if r.Status != Lobby {
		return ErrInRound
	}
	if mode == "" {
		mode = r.Mode
	}
	if mode != r.Mode || mask != r.Mask {
		r.Mode, r.Mask = mode, mask
		for i := range r.Members {
			r.Members[i].Ready = false
		}
	}
	r.changed(now)
	return nil
}

// Start begins a round with every member in it, once everyone but the host
// (whose starting it says they're ready) is ready, in the room's mode.
// answerID is the Pokémon to find (in a streak mode, the first one), picked
// by the caller from the room's generations.
func (r *Room) Start(player string, answerID int, now time.Time) error {
	if _, err := r.openAs(player, now); err != nil {
		return err
	}
	if r.Host != player {
		return ErrNotHost
	}
	if r.Status != Lobby {
		return ErrInRound
	}
	if len(r.Members) < MinPlayers {
		return ErrTooFew
	}
	for _, m := range r.Members {
		// Disconnecting unreadies, so ready members are connected.
		if m.PlayerID != r.Host && !m.Ready {
			return ErrNotReady
		}
	}
	rd := &Round{N: 1, Mode: r.Mode, AnswerID: answerID, StartsAt: now.Add(Countdown)}
	if r.Round != nil {
		rd.N = r.Round.N + 1
	}
	rd.EndsAt = rd.StartsAt.Add(RoundLength)
	for i := range r.Members {
		rd.Players = append(rd.Players, Player{PlayerID: r.Members[i].PlayerID, Name: r.Members[i].Name})
		r.Members[i].Ready = false // ready again for the one after
	}
	r.Round, r.Status = rd, Playing
	r.changed(now)
	return nil
}

// QuickReady reports whether a quick-match room should start its first round
// now: it hasn't had one, and everyone in it (two at least) is connected.
func (r *Room) QuickReady() bool {
	if !r.Quick || r.Status != Lobby || r.Round != nil || len(r.Members) < MinPlayers {
		return false
	}
	for i := range r.Members {
		if !r.Members[i].connected() {
			return false
		}
	}
	return true
}

// StartQuick starts a quick-match room's first round, as its host would,
// with everyone counted ready (they queued for it). See Start for answerID.
func (r *Room) StartQuick(answerID int, now time.Time) error {
	r.Advance(now)
	if !r.QuickReady() {
		return ErrNotReady
	}
	for i := range r.Members {
		r.Members[i].Ready = true
	}
	return r.Start(r.Host, answerID, now)
}

// Guess records that player made a guess in a Wordle round, which solved it
// or not. The first solve brings the round's end forward to FinishWindow
// later.
func (r *Room) Guess(player string, solved bool, now time.Time) error {
	if err := r.CanMove(player, games.Wordle, now); err != nil {
		return err
	}
	p := r.player(player)
	p.Guesses++
	if solved {
		if !r.Round.solved() {
			r.Round.EndsAt = minTime(r.Round.EndsAt, now.Add(FinishWindow))
		}
		p.SolvedAt = now
	}
	r.changed(now)
	return nil
}

// Answer records player's answer in a streak round: a right one adds to
// their streak, a wrong one ends their run.
func (r *Room) Answer(player string, correct bool, now time.Time) error {
	if err := r.CanMove(player, games.HigherLower, now); err != nil {
		return err
	}
	p := r.player(player)
	if correct {
		p.Score++
		p.ScoredAt = now
	} else {
		p.OutAt = now
	}
	r.changed(now)
	return nil
}

// Forfeit gives up the round for player, who stays in the room.
func (r *Room) Forfeit(player string, now time.Time) error {
	if err := r.playing(player, now); err != nil {
		return err
	}
	r.player(player).ForfeitedAt = now
	r.changed(now)
	return nil
}

// CanMove checks that player can make a move of mode's kind in the round
// now, without making one: the store asks before the move goes to the game,
// so a move the room would refuse is never played. A Wordle round takes
// guesses (Guess), the streak modes answers (Answer).
func (r *Room) CanMove(player string, mode games.Mode, now time.Time) error {
	if err := r.playing(player, now); err != nil {
		return err
	}
	if (r.Round.Mode == games.Wordle) != (mode == games.Wordle) {
		return ErrWrongMove
	}
	return nil
}

// playing checks that player can make a move in the round now.
func (r *Room) playing(player string, now time.Time) error {
	if _, err := r.openAs(player, now); err != nil {
		return err
	}
	if r.Status != Playing {
		return ErrNoRound
	}
	p := r.player(player)
	switch {
	case p == nil:
		return ErrNotPlaying
	case now.Before(r.Round.StartsAt):
		return ErrNotStarted
	case p.Done():
		return ErrFinished
	}
	return nil
}

// solved reports whether anyone has solved the round.
func (rd *Round) solved() bool {
	return slices.ContainsFunc(rd.Players, func(p Player) bool { return !p.SolvedAt.IsZero() })
}

// Advance applies every deadline passed by now, and reports whether that
// changed anything:
//
//   - A player still racing who has been disconnected for Grace forfeits, as
//     of when their grace ran out (unless the round ended first).
//   - A round ends at EndsAt, or as soon as places can't change any more
//     (see end). The room goes back to its lobby.
//   - In the lobby, a member disconnected for Grace is removed.
//   - A lobby that's empty, or idle for IdleTimeout, closes.
func (r *Room) Advance(now time.Time) bool {
	changed := false
	if r.Status == Playing {
		rd := r.Round
		for i := range rd.Players {
			p := &rd.Players[i]
			m := r.member(p.PlayerID)
			if p.Done() || m == nil || m.connected() {
				continue
			}
			if at := m.graceEnds(); !now.Before(at) && at.Before(rd.EndsAt) {
				p.ForfeitedAt = at
				changed = true
			}
		}
		if at, over := rd.end(now); over {
			rd.FinishedAt = at
			r.Status = Lobby
			changed = true
		}
	}
	if r.Status == Lobby {
		var gone []string
		for _, m := range r.Members {
			if !m.connected() && !now.Before(m.graceEnds()) {
				gone = append(gone, m.PlayerID)
			}
		}
		for _, id := range gone {
			r.remove(id)
			changed = true
		}
		if len(r.Members) == 0 || !now.Before(r.LastActivity.Add(IdleTimeout)) {
			r.Status = Closed
			changed = true
		}
	}
	return changed
}

// end reports whether the round is over by now, and when it ended: at
// EndsAt, or as soon as places can't change any more.
//
//   - Wordle: when at most one player is still racing.
//   - Streak modes: when nobody is, or the only one who is has a longer
//     streak than everyone else (they've won). One still going but behind
//     plays on, to catch up or not.
func (rd *Round) end(now time.Time) (time.Time, bool) {
	racing := 0
	var runner *Player
	var last time.Time // the last player to finish
	for i := range rd.Players {
		p := &rd.Players[i]
		if !p.Done() {
			racing++
			runner = p
		} else if p.doneAt().After(last) {
			last = p.doneAt()
		}
	}
	if rd.Mode == games.Wordle {
		if racing <= 1 && !last.IsZero() {
			return last, true
		}
	} else {
		if racing == 0 && !last.IsZero() {
			return last, true
		}
		if racing == 1 && rd.leads(runner) {
			at := last
			if runner.ScoredAt.After(at) {
				at = runner.ScoredAt
			}
			if at.IsZero() { // nobody else is left in the round at all
				at = now
			}
			return at, true
		}
	}
	if !now.Before(rd.EndsAt) {
		return rd.EndsAt, true
	}
	return time.Time{}, false
}

// leads reports whether p's streak is longer than everyone else's.
func (rd *Round) leads(p *Player) bool {
	for i := range rd.Players {
		if o := &rd.Players[i]; o != p && o.Score >= p.Score {
			return false
		}
	}
	return true
}

// Deadline returns when Advance next has something to do, for a timer to
// call it then. Zero means never: the room has closed.
func (r *Room) Deadline() time.Time {
	var d time.Time
	earliest := func(t time.Time) {
		if d.IsZero() || t.Before(d) {
			d = t
		}
	}
	switch r.Status {
	case Playing:
		earliest(r.Round.EndsAt)
		for _, p := range r.Round.Players {
			if m := r.member(p.PlayerID); !p.Done() && m != nil && !m.connected() {
				earliest(m.graceEnds())
			}
		}
	case Lobby:
		earliest(r.LastActivity.Add(IdleTimeout))
		for _, m := range r.Members {
			if !m.connected() {
				earliest(m.graceEnds())
			}
		}
	}
	return d
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// --- Room codes ---------------------------------------------------------------

// CodeLength is how long a room code is.
const CodeLength = 6

// codeAlphabet leaves out characters easily read as others (0/O, 1/I/L):
// 31 of them, so about 900 million codes.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// NewCode returns a random room code, picking characters with intn (like
// rand.IntN). The store retries if it's taken.
func NewCode(intn func(n int) int) string {
	b := make([]byte, CodeLength)
	for i := range b {
		b[i] = codeAlphabet[intn(len(codeAlphabet))]
	}
	return string(b)
}

// ParseCode tidies a code as a player typed it (any case, spaces around) and
// reports whether it could be one.
func ParseCode(s string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(s))
	if len(code) != CodeLength {
		return "", false
	}
	for _, c := range code {
		if !strings.ContainsRune(codeAlphabet, c) {
			return "", false
		}
	}
	return code, true
}
