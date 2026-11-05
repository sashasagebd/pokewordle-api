package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/games"
	"github.com/sashasagebd/pokewordle-api/internal/pokedex"
)

// fakePlayers hands out tokens from memory.
type fakePlayers struct {
	mu      sync.Mutex
	players map[string]string // token → player
}

func newFakePlayers() *fakePlayers { return &fakePlayers{players: map[string]string{}} }

func (f *fakePlayers) NewPlayer(context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.players) + 1
	token, player := fmt.Sprintf("token-%d", n), fmt.Sprintf("player-%d", n)
	f.players[token] = player
	return token, player, nil
}

func (f *fakePlayers) PlayerFor(_ context.Context, token string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.players[token]; ok {
		return p, nil
	}
	return "", auth.ErrUnknownToken
}

// fakeGames keeps games in memory, played by the real engine.
type fakeGames struct {
	mu     sync.Mutex
	engine *games.Engine
	games  map[string]*games.Game
	owner  map[string]string
}

func newFakeGames() *fakeGames {
	seed, err := pokedex.Seed()
	if err != nil {
		panic(err)
	}
	return &fakeGames{engine: &games.Engine{Dex: games.NewDex(seed)}, games: map[string]*games.Game{}, owner: map[string]string{}}
}

func (f *fakeGames) get(player, id string) (*games.Game, error) {
	if g, ok := f.games[id]; ok && f.owner[id] == player {
		return g, nil
	}
	return nil, games.ErrNotFound
}

func (f *fakeGames) Start(_ context.Context, player string, mode games.Mode, mask games.GenMask) (games.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.engine.New(mode, mask)
	g.ID = fmt.Sprintf("game-%d", len(f.games)+1)
	f.games[g.ID], f.owner[g.ID] = g, player
	return f.engine.State(g), nil
}

func (f *fakeGames) Get(_ context.Context, player, id string) (games.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.get(player, id)
	if err != nil {
		return games.State{}, err
	}
	return f.engine.State(g), nil
}

func (f *fakeGames) Guess(_ context.Context, player, id, name string) (games.Comparison, games.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.get(player, id)
	if err != nil {
		return games.Comparison{}, games.State{}, err
	}
	_, c, err := f.engine.Guess(g, name)
	return c, f.engine.State(g), err
}

func (f *fakeGames) Answer(_ context.Context, player, id string, a games.Answer) (games.AnswerResult, games.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.get(player, id)
	if err != nil {
		return games.AnswerResult{}, games.State{}, err
	}
	_, res, err := f.engine.Answer(g, a)
	return res, f.engine.State(g), err
}

func (f *fakeGames) GiveUp(_ context.Context, player, id string) (games.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.get(player, id)
	if err != nil {
		return games.State{}, err
	}
	err = f.engine.GiveUp(g)
	return f.engine.State(g), err
}

func (f *fakeGames) SilhouetteOf(_ context.Context, player, id string, n int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.get(player, id)
	if err != nil || g.Mode != games.WhosThat || n < 1 || n > len(g.Rounds) {
		return 0, games.ErrNotFound
	}
	return g.Rounds[n-1].AnswerID, nil
}

// answer peeks at a game's open round, as only a test may.
func (f *fakeGames) answer(id string) pokedex.Pokemon {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.games[id]
	for _, r := range g.Rounds {
		if !r.Answered {
			return f.engine.Dex.Get(r.AnswerID)
		}
	}
	panic("no open round")
}

type client struct {
	t     *testing.T
	h     http.Handler
	token string
}

func (c *client) do(method, target, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

// call does a request, checks its status and decodes its JSON into out.
func (c *client) call(method, target, body string, want int, out any) {
	c.t.Helper()
	rec := c.do(method, target, body)
	if rec.Code != want {
		c.t.Fatalf("%s %s %s: got %d %s, want %d", method, target, body, rec.Code, rec.Body, want)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			c.t.Fatalf("%s %s: %v", method, target, err)
		}
	}
}

func newClient(t *testing.T) (*client, *fakeGames) {
	fg := newFakeGames()
	h := New(Config{Dex: &fakeDex{}, Players: newFakePlayers(), Games: fg, Silhouette: pokedex.Silhouette, AllowedOrigins: []string{frontend}})
	c := &client{t: t, h: h}
	var p struct{ Token string }
	c.call("POST", "/players", "", 201, &p)
	if p.Token == "" {
		t.Fatal("POST /players returned no token")
	}
	c.token = p.Token
	return c, fg
}

func TestGamesNeedAToken(t *testing.T) {
	c, _ := newClient(t)
	for _, token := range []string{"", "wrong"} {
		c.token = token
		if rec := c.do("POST", "/games", `{"mode":"wordle"}`); rec.Code != 401 {
			t.Errorf("token %q: got %d, want 401", token, rec.Code)
		}
	}
}

func TestStartGameValidates(t *testing.T) {
	c, _ := newClient(t)
	for _, body := range []string{
		`{"mode":"chess"}`,
		`{"mode":"wordle","gens":[10]}`,
		`{"mode":"wordle","extra":1}`,
		`{"mode":"wordle"} {}`,
		`not json`,
		`{"mode":"wordle","gens":[` + strings.Repeat("1,", 600) + `1]}`,
	} {
		if rec := c.do("POST", "/games", body); rec.Code != 400 {
			t.Errorf("%.40s: got %d, want 400", body, rec.Code)
		}
	}
}

func TestWordleOverHTTP(t *testing.T) {
	c, fg := newClient(t)
	var st games.State
	c.call("POST", "/games", `{"mode":"wordle","gens":[1]}`, 201, &st)
	if st.Status != games.Active || st.Answer != nil {
		t.Fatalf("new game = %+v", st)
	}
	if rec := c.do("POST", "/games", `{"mode":"wordle"}`); rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("game responses must not be cached")
	}
	answer := fg.answer(st.ID)
	wrong := "bulbasaur"
	if answer.Name == wrong {
		wrong = "ivysaur"
	}

	var guess struct {
		Result games.Comparison
		Game   games.State
	}
	c.call("POST", "/games/"+st.ID+"/guess", `{"name":"`+wrong+`"}`, 200, &guess)
	if guess.Result.Correct || guess.Game.Score != 1 || guess.Game.Answer != nil {
		t.Fatalf("wrong guess = %+v", guess)
	}

	var e struct{ Error string }
	c.call("POST", "/games/"+st.ID+"/guess", `{"name":"`+wrong+`"}`, 400, &e)
	if !strings.Contains(e.Error, "already guessed") {
		t.Errorf("repeat guess error = %q", e.Error)
	}
	c.call("POST", "/games/"+st.ID+"/guess", `{"name":"chikorita"}`, 400, nil)

	c.call("POST", "/games/"+st.ID+"/guess", `{"name":"`+answer.Name+`"}`, 200, &guess)
	if !guess.Result.Correct || guess.Game.Status != games.Won || guess.Game.Answer.ID != answer.ID {
		t.Fatalf("right guess = %+v", guess)
	}
	c.call("POST", "/games/"+st.ID+"/guess", `{"name":"pikachu"}`, 409, nil)
	c.call("GET", "/games/"+st.ID, "", 200, &st)
	if len(st.Guesses) != 2 {
		t.Errorf("GET shows %d guesses, want 2", len(st.Guesses))
	}
}

func TestStreakOverHTTP(t *testing.T) {
	c, fg := newClient(t)
	var st games.State
	c.call("POST", "/games", `{"mode":"whos_that"}`, 201, &st)

	// The silhouette of the open round and of the next one, not further.
	for n, want := range map[int]int{1: 200, 2: 200, 3: 404, 0: 404} {
		rec := c.do("GET", fmt.Sprintf("/games/%s/rounds/%d/image", st.ID, n), "")
		if rec.Code != want {
			t.Errorf("image %d: got %d, want %d", n, rec.Code, want)
			continue
		}
		if want == 200 {
			if _, err := png.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil || rec.Header().Get("Content-Type") != "image/png" {
				t.Errorf("image %d isn't a PNG: %v", n, err)
			}
		}
	}

	var res struct {
		Correct bool
		Answer  pokedex.Pokemon
		Game    games.State
	}
	right := fg.answer(st.ID)
	c.call("POST", "/games/"+st.ID+"/answer", `{"n":1,"name":"`+right.Name+`"}`, 200, &res)
	if !res.Correct || res.Answer.ID != right.ID || res.Game.Score != 1 || res.Game.Round.N != 2 {
		t.Fatalf("right answer = %+v", res)
	}

	// The same answer again (a retry) gets 409 and the game as it is now.
	var conflict struct {
		Error string
		Game  games.State
	}
	c.call("POST", "/games/"+st.ID+"/answer", `{"n":1,"name":"`+right.Name+`"}`, 409, &conflict)
	if conflict.Game.Round == nil || conflict.Game.Round.N != 2 {
		t.Errorf("409 body = %+v", conflict)
	}
	c.call("POST", "/games/"+st.ID+"/guess", `{"name":"pikachu"}`, 400, nil)

	c.call("POST", "/games/"+st.ID+"/give-up", "", 200, &st)
	if st.Status != games.Lost || st.Score != 1 || st.Answer == nil {
		t.Fatalf("given up = %+v", st)
	}
	c.call("POST", "/games/"+st.ID+"/give-up", "", 409, nil)
}

func TestGamesArePrivate(t *testing.T) {
	alice, _ := newClient(t)
	var st games.State
	alice.call("POST", "/games", `{"mode":"higher_lower"}`, 201, &st)
	if st.Round == nil || st.Round.Current == nil || st.Round.Next == nil {
		t.Fatalf("higher_lower round = %+v", st.Round)
	}
	// Next is only named: its values stay hidden.
	raw := alice.do("GET", "/games/"+st.ID, "").Body.String()
	if strings.Count(raw, `"height"`) != 1 {
		t.Errorf("only the current Pokémon's height may show: %s", raw)
	}

	var p struct{ Token string }
	alice.call("POST", "/players", "", 201, &p)
	bob := &client{t: t, h: alice.h, token: p.Token}
	bob.call("GET", "/games/"+st.ID, "", 404, nil)
	bob.call("POST", "/games/"+st.ID+"/answer", `{"n":1,"dir":"higher","stat":"height"}`, 404, nil)
}
