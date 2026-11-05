package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sashasagebd/pokewordle-api/internal/auth"
	"github.com/sashasagebd/pokewordle-api/internal/dbtest"
)

func TestTokens(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	s := auth.NewStore(pool)

	token, player, err := s.NewPlayer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 43 {
		t.Errorf("token %q is %d characters, want 43 (32 bytes, base64url)", token, len(token))
	}
	if got, err := s.PlayerFor(ctx, token); err != nil || got != player {
		t.Fatalf("PlayerFor = %q, %v; want %q", got, err, player)
	}

	// The token itself is never stored.
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash = convert_to($1, 'UTF8')`, token).Scan(&stored); err != nil || stored != 0 {
		t.Errorf("found the raw token in sessions (%d, %v)", stored, err)
	}

	// A session unused for over an hour gets its last use recorded.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET last_used_at = now() - interval '2 hours' WHERE player_id = $1::uuid`, player); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlayerFor(ctx, token); err != nil {
		t.Fatal(err)
	}
	var recent bool
	if err := pool.QueryRow(ctx, `SELECT last_used_at > now() - interval '1 minute' FROM sessions WHERE player_id = $1::uuid`, player).Scan(&recent); err != nil || !recent {
		t.Errorf("last_used_at not refreshed (%v, %v)", recent, err)
	}

	other, _, err := s.NewPlayer(ctx)
	if err != nil || other == token {
		t.Fatalf("second player: %q, %v", other, err)
	}

	for _, bad := range []string{"", "not base64!", "c2hvcnQ", token[:42] + "A"} {
		if _, err := s.PlayerFor(ctx, bad); !errors.Is(err, auth.ErrUnknownToken) {
			t.Errorf("PlayerFor(%q) err = %v, want ErrUnknownToken", bad, err)
		}
	}

	// Deleting a player (Phase 4's cleanup) takes its sessions with it.
	if _, err := pool.Exec(ctx, `DELETE FROM players WHERE id = $1::uuid`, player); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlayerFor(ctx, token); !errors.Is(err, auth.ErrUnknownToken) {
		t.Errorf("token of a deleted player: err = %v, want ErrUnknownToken", err)
	}
}
