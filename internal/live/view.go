// Package live runs Versus rooms over WebSockets: a hub in each instance of
// the app holds that instance's sockets, and pushes every change to a room
// to the members connected there.
//
// Changes reach every instance the same way, through Postgres: the room
// store announces each one on rooms.NotifyChannel as it commits, and each
// hub LISTENs, reloads the room and sends each member what they may see of
// it (view.go). So two players in one room can be on different instances
// (during a deploy, there are two), and a hub never trusts its own memory of
// a room: it only remembers sockets.
package live

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
	"github.com/sashasagebd/pokewordle-api/internal/rooms"
)

// PublicID is how a player appears to the others in a room: the same
// throughout the room, but not their player ID, which never leaves the
// server.
func PublicID(roomID, player string) string {
	sum := sha256.Sum256([]byte(roomID + ":" + player))
	return hex.EncodeToString(sum[:6])
}

// A View is a room as one member may see it.
type View struct {
	Code    string       `json:"code"`
	Status  rooms.Status `json:"status"`
	Gens    []int        `json:"gens"`
	You     string       `json:"you"`  // your public ID
	Host    string       `json:"host"` // the host's
	Members []MemberView `json:"members"`
	// Round is the round being played, or else the last one. Absent before
	// the first.
	Round *RoundView `json:"round,omitempty"`
}

// A MemberView is a member of the room.
type MemberView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Ready     bool   `json:"ready"`
	Connected bool   `json:"connected"`
	// GraceEndsAt is when a disconnected member forfeits or leaves.
	GraceEndsAt *time.Time `json:"graceEndsAt,omitempty"`
}

// A RoundView is a round. Its times are the server's: a client works out how
// far its own clock is off from the serverNow sent with each view.
type RoundView struct {
	N          int        `json:"n"`
	StartsAt   time.Time  `json:"startsAt"` // the end of the countdown
	EndsAt     time.Time  `json:"endsAt"`   // the latest it ends
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	// Players are in their places (standings so far, while it's on).
	Players []PlayerView `json:"players"`
	// Winner is the winner's public ID once it's over: absent for a draw.
	Winner string           `json:"winner,omitempty"`
	Answer *pokedex.Pokemon `json:"answer,omitempty"` // once it's over
	// Game is your own race, as a solo Wordle shows it, if you're in the
	// round: everything you've guessed, and the answer once it's over.
	Game *games.State `json:"game,omitempty"`
}

// A PlayerView is someone's race in a round.
type PlayerView struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Rank    int           `json:"rank"`
	Outcome rooms.Outcome `json:"outcome"`
	Racing  bool          `json:"racing"` // still playing it
	Guesses int           `json:"guesses"`
	TimeMs  int64         `json:"timeMs,omitempty"` // how long they took to solve it
	// Tiles are their guesses' results without the Pokémon guessed: while
	// the round is on, which ones they tried would give their deductions
	// away.
	Tiles []Tile `json:"tiles"`
	// Comparisons are their guesses in full, once the round is over.
	Comparisons []games.Comparison `json:"comparisons,omitempty"`
}

// A Tile is a guess's result, as a Wordle row shows it, without the guess.
type Tile struct {
	Height  string `json:"height"`
	Weight  string `json:"weight"`
	Type    string `json:"type"`
	Correct bool   `json:"correct"`
}

// ViewFor returns what player may see of r. states are the round's games,
// by player ID (one missing just shows no guesses).
func ViewFor(r *rooms.Room, states map[string]games.State, player string) View {
	v := View{
		Code: r.Code, Status: r.Status, Gens: r.Mask.Gens(),
		You: PublicID(r.ID, player), Members: []MemberView{},
	}
	if r.Host != "" {
		v.Host = PublicID(r.ID, r.Host)
	}
	for _, m := range r.Members {
		mv := MemberView{ID: PublicID(r.ID, m.PlayerID), Name: m.Name, Ready: m.Ready, Connected: m.DisconnectedAt.IsZero()}
		if !mv.Connected {
			ends := m.DisconnectedAt.Add(rooms.Grace)
			mv.GraceEndsAt = &ends
		}
		v.Members = append(v.Members, mv)
	}
	if r.Round != nil {
		v.Round = roundView(r, states, player)
	}
	return v
}

func roundView(r *rooms.Room, states map[string]games.State, player string) *RoundView {
	rd := r.Round
	over := !rd.FinishedAt.IsZero()
	rv := &RoundView{N: rd.N, StartsAt: rd.StartsAt, EndsAt: rd.EndsAt, Players: []PlayerView{}}
	if over {
		rv.FinishedAt = &rd.FinishedAt
		if w := rd.Winner(); w != "" {
			rv.Winner = PublicID(r.ID, w)
		}
	}

	names := map[string]string{}
	for _, p := range rd.Players {
		names[p.PlayerID] = p.Name
	}
	for _, place := range rd.Ranking() {
		st := states[place.PlayerID]
		pv := PlayerView{
			ID: PublicID(r.ID, place.PlayerID), Name: names[place.PlayerID],
			Rank: place.Rank, Outcome: place.Outcome, Guesses: place.Guesses,
			Racing: !over && place.Outcome == rooms.Unsolved,
			TimeMs: place.Time.Milliseconds(), Tiles: []Tile{},
		}
		for _, c := range st.Guesses {
			pv.Tiles = append(pv.Tiles, Tile{c.Height, c.Weight, c.Type, c.Correct})
		}
		if over {
			pv.Comparisons = st.Guesses
		}
		if st.Answer != nil && over {
			rv.Answer = st.Answer
		}
		rv.Players = append(rv.Players, pv)
	}
	if st, ok := states[player]; ok {
		rv.Game = &st
	}
	return rv
}
