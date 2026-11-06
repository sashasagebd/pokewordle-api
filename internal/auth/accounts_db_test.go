package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/sashasagebd/pokewordle-api/internal/dbtest"
)

// randomName returns a username no other test run has used.
func randomName() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "t" + hex.EncodeToString(b)
}

func TestClaimLoginLogout(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	s := NewStore(pool)

	_, ash, err := s.NewPlayer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name := randomName()
	var invalid *InvalidError
	if err := s.Claim(ctx, ash, "no", "pikachu123"); !errors.As(err, &invalid) {
		t.Errorf("bad name: err = %v, want an InvalidError", err)
	}
	if err := s.Claim(ctx, ash, name, "pikachu123"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Username(ctx, ash); err != nil || got != name {
		t.Errorf("Username = %q, %v; want %q", got, err, name)
	}
	if err := s.Claim(ctx, ash, randomName(), "pikachu123"); !errors.Is(err, ErrAlreadyNamed) {
		t.Errorf("second claim: err = %v, want ErrAlreadyNamed", err)
	}

	_, misty, err := s.NewPlayer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Claim(ctx, misty, strings.ToUpper(name), "pikachu123"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("same name in capitals: err = %v, want ErrNameTaken", err)
	}
	if got, _ := s.Username(ctx, misty); got != "" {
		t.Errorf("a failed claim left the name %q", got)
	}

	for _, bad := range [][2]string{{name, "pikachu124"}, {randomName(), "pikachu123"}} {
		if _, _, _, err := s.Login(ctx, bad[0], bad[1]); !errors.Is(err, ErrBadLogin) {
			t.Errorf("Login(%q, %q): err = %v, want ErrBadLogin", bad[0], bad[1], err)
		}
	}
	token, player, stored, err := s.Login(ctx, strings.ToUpper(name), "pikachu123")
	if err != nil || player != ash || stored != name {
		t.Fatalf("Login = %q, %q, %v; want %q as %q", player, stored, err, ash, name)
	}
	if got, err := s.PlayerFor(ctx, token); err != nil || got != ash {
		t.Errorf("the login's token is for %q (%v), want %q", got, err, ash)
	}

	if err := s.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlayerFor(ctx, token); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("token after logout: err = %v, want ErrUnknownToken", err)
	}
}

func TestLoginRehashesOldHashes(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	s := NewStore(pool)
	_, player, err := s.NewPlayer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name := randomName()
	if err := s.Claim(ctx, player, name, "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	old := argonParams{memory: 8 * 1024, time: 1, threads: 1}
	salt := []byte("0123456789abcdef")
	oldHash := encodeHash(old, salt, argonKey("hunter2hunter2", salt, old, keyLen))
	if _, err := pool.Exec(ctx, `UPDATE players SET password_hash = $2 WHERE id = $1::uuid`, player, oldHash); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := s.Login(ctx, name, "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	var now string
	if err := pool.QueryRow(ctx, `SELECT password_hash FROM players WHERE id = $1::uuid`, player).Scan(&now); err != nil {
		t.Fatal(err)
	}
	if now == oldHash || !strings.HasPrefix(now, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash after login = %q, want it redone with the current settings", now)
	}
	if _, _, _, err := s.Login(ctx, name, "hunter2hunter2"); err != nil {
		t.Errorf("login with the new hash: %v", err)
	}
}
