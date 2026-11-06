package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/games"
)

func (f *fakePlayers) Username(_ context.Context, player string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.names[player], nil
}

func (f *fakePlayers) Claim(_ context.Context, player, username, password string) error {
	if err := auth.CheckUsername(username); err != nil {
		return err
	}
	if err := auth.CheckPassword(password); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names[player] != "" {
		return auth.ErrAlreadyNamed
	}
	if _, ok := f.passwords[strings.ToLower(username)]; ok {
		return auth.ErrNameTaken
	}
	f.names[player] = username
	f.passwords[strings.ToLower(username)] = password
	return nil
}

func (f *fakePlayers) Login(_ context.Context, username, password string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pw, ok := f.passwords[strings.ToLower(username)]; !ok || pw != password {
		return "", "", "", auth.ErrBadLogin
	}
	for player, name := range f.names {
		if strings.EqualFold(name, username) {
			token := fmt.Sprintf("token-%d", len(f.players)+1)
			f.players[token] = player
			return token, player, name, nil
		}
	}
	panic("password without a player")
}

func (f *fakePlayers) Logout(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.players, token)
	return nil
}

func (f *fakeGames) Bests(context.Context, string) ([]games.Best, error) { return []games.Best{}, nil }

func (f *fakeGames) Leaderboard(_ context.Context, _ games.Mode, _ games.GenMask, _ int, player string) (games.Board, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boards = append(f.boards, player)
	return games.Board{Entries: []games.Entry{}}, nil
}

func (f *fakeGames) Merge(_ context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges = append(f.merges, [2]string{from, to})
	return nil
}

func TestClaimAndLogin(t *testing.T) {
	ash, fg := newClient(t)

	var me struct {
		Username *string
		Best     []games.Best
	}
	ash.call("GET", "/me", "", 200, &me)
	if me.Username != nil || me.Best == nil {
		t.Fatalf("a guest's /me = %+v", me)
	}

	ash.call("POST", "/me/username", `{"username":"a","password":"pikachu123"}`, 400, nil)
	ash.call("POST", "/me/username", `{"username":"Ash","password":"short"}`, 400, nil)
	ash.call("POST", "/me/username", `{"username":"Ash","password":"pikachu123"}`, 200, nil)
	ash.call("POST", "/me/username", `{"username":"Ash2","password":"pikachu123"}`, 409, nil)
	ash.call("GET", "/me", "", 200, &me)
	if me.Username == nil || *me.Username != "Ash" {
		t.Fatalf("/me after claiming = %+v", me)
	}

	// Another guest can't take the name, whatever the case.
	var p struct{ Token string }
	ash.call("POST", "/players", "", 201, &p)
	guest := &client{t: t, h: ash.h, token: p.Token}
	guest.call("POST", "/me/username", `{"username":"ASH","password":"pikachu123"}`, 409, nil)

	guest.call("POST", "/sessions", `{"username":"ash","password":"wrong-password"}`, 401, nil)
	guest.call("POST", "/sessions", `{"username":"misty","password":"pikachu123"}`, 401, nil)

	// The guest logs in as Ash: a new token, and the guest's progress merges in.
	var login struct{ Token, Username string }
	guest.call("POST", "/sessions", `{"username":"ash","password":"pikachu123"}`, 201, &login)
	if login.Token == "" || login.Username != "Ash" {
		t.Fatalf("login = %+v", login)
	}
	if len(fg.merges) != 1 || fg.merges[0][1] != "player-1" || fg.merges[0][0] == "player-1" {
		t.Errorf("merges = %v, want the guest into player-1", fg.merges)
	}

	// Logging in without a token merges nothing.
	nobody := &client{t: t, h: ash.h}
	nobody.call("POST", "/sessions", `{"username":"ash","password":"pikachu123"}`, 201, nil)
	if len(fg.merges) != 1 {
		t.Errorf("a login without a guest merged: %v", fg.merges)
	}

	// Logging out ends that token only.
	loggedIn := &client{t: t, h: ash.h, token: login.Token}
	loggedIn.call("DELETE", "/sessions/current", "", 204, nil)
	loggedIn.call("GET", "/me", "", 401, nil)
	ash.call("GET", "/me", "", 200, nil)
}

func TestPasswordEndpointsAreRateLimited(t *testing.T) {
	c, _ := newClient(t)
	codes := map[int]int{}
	for range passwordAttemptsPerMinute + 2 {
		codes[c.do("POST", "/sessions", `{"username":"ash","password":"guess-guess"}`).Code]++
	}
	if codes[401] != passwordAttemptsPerMinute || codes[429] != 2 {
		t.Fatalf("status counts %v, want %d × 401 then 2 × 429", codes, passwordAttemptsPerMinute)
	}
	// Claiming counts against the same limit, and other IPs aren't affected.
	if rec := c.do("POST", "/me/username", `{"username":"ash","password":"pikachu123"}`); rec.Code != 429 {
		t.Errorf("claim after the limit: %d", rec.Code)
	}
}

func TestLeaderboardRequest(t *testing.T) {
	c, fg := newClient(t)
	for _, bad := range []string{"", "?mode=chess", "?mode=wordle&gens=10", "?mode=wordle&limit=0", "?mode=wordle&limit=101"} {
		if rec := c.do("GET", "/leaderboard"+bad, ""); rec.Code != 400 {
			t.Errorf("%q: got %d, want 400", bad, rec.Code)
		}
	}

	rec := c.do("GET", "/leaderboard?mode=whos_that&gens=1", "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Errorf("with a token: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	c.token = ""
	rec = c.do("GET", "/leaderboard?mode=whos_that&gens=1", "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=30" {
		t.Errorf("without a token: %d %v", rec.Code, rec.Header())
	}
	c.token = "unknown"
	c.call("GET", "/leaderboard?mode=whos_that", "", 200, nil) // an unknown token is just ignored
	if !slices.Equal(fg.boards, []string{"player-1", "", ""}) {
		t.Errorf("boards were asked for %q", fg.boards)
	}
}
