// Package auth gives players anonymous bearer tokens and finds the player
// behind one.
//
// A token is 32 random bytes, base64url-encoded. Only its sha256 is stored.
// The frontend keeps it in localStorage and sends it as
// "Authorization: Bearer <token>"; not a cookie, because the frontend and the
// API are on different sites.
//
// A player can claim a username and password (argon2id), then log in with
// them elsewhere for a new token to the same player.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrUnknownToken means the token isn't one this API gave out (or its
	// player is gone).
	ErrUnknownToken = errors.New("unknown token")
	// ErrNameTaken: another player has that username (ignoring case).
	ErrNameTaken = errors.New("that name is taken")
	// ErrAlreadyNamed: the player already has a username.
	ErrAlreadyNamed = errors.New("this player already has a name")
	// ErrBadLogin: no such username, or the wrong password (never which).
	ErrBadLogin = errors.New("wrong username or password")
)

// Store reads and writes the players and sessions tables.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store using pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// newToken returns a new token and its hash.
func newToken() (token string, hash []byte) {
	raw := make([]byte, 32)
	rand.Read(raw) // never fails (crypto/rand panics instead)
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:]
}

// tokenHash returns token's hash, or false if it can't be one of ours.
func tokenHash(token string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

// NewPlayer makes an anonymous player and returns its token and id.
func (s *Store) NewPlayer(ctx context.Context) (token, playerID string, err error) {
	token, hash := newToken()
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO players DEFAULT VALUES RETURNING id::text`).Scan(&playerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sessions (token_hash, player_id) VALUES ($1, $2::uuid)`, hash, playerID)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return token, playerID, nil
}

// PlayerFor returns the id of the player token belongs to.
func (s *Store) PlayerFor(ctx context.Context, token string) (string, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return "", ErrUnknownToken
	}

	var playerID string
	var stale bool
	err := s.pool.QueryRow(ctx, `
		SELECT player_id::text, last_used_at < now() - interval '1 hour'
		FROM sessions WHERE token_hash = $1`, hash).Scan(&playerID, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnknownToken
	}
	if err != nil {
		return "", err
	}
	// Recording every use would be a write per request; hourly is enough to
	// tell an abandoned session (Phase 4's cleanup) from a live one.
	if stale {
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_used_at = now() WHERE token_hash = $1`, hash); err != nil {
			return "", err
		}
	}
	return playerID, nil
}

// Username returns player's username, or "" if they haven't claimed one.
func (s *Store) Username(ctx context.Context, player string) (string, error) {
	var name *string
	err := s.pool.QueryRow(ctx, `SELECT username FROM players WHERE id = $1::uuid`, player).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnknownToken
	}
	if err != nil || name == nil {
		return "", err
	}
	return *name, nil
}

// Claim gives an anonymous player a username and password.
func (s *Store) Claim(ctx context.Context, player, username, password string) error {
	if err := CheckUsername(username); err != nil {
		return err
	}
	if err := CheckPassword(password); err != nil {
		return err
	}
	hash := hashPassword(password) // slow, so outside any transaction
	tag, err := s.pool.Exec(ctx, `
		UPDATE players SET username = $2, password_hash = $3
		WHERE id = $1::uuid AND username IS NULL`, player, username, hash)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrNameTaken
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyNamed
	}
	return nil
}

// Login checks username and password and returns a new token for that
// player, with the username as stored.
func (s *Store) Login(ctx context.Context, username, password string) (token, playerID, name string, err error) {
	var hash string
	err = s.pool.QueryRow(ctx, `
		SELECT id::text, username, password_hash FROM players
		WHERE lower(username) = lower($1) AND password_hash IS NOT NULL`, username).Scan(&playerID, &name, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		verifyPassword(password, dummyHash) // as slow as a real check
		return "", "", "", ErrBadLogin
	}
	if err != nil {
		return "", "", "", err
	}
	ok, rehash, err := verifyPassword(password, hash)
	if err != nil {
		return "", "", "", err
	}
	if !ok {
		return "", "", "", ErrBadLogin
	}
	if rehash {
		if _, err := s.pool.Exec(ctx, `UPDATE players SET password_hash = $2 WHERE id = $1::uuid`, playerID, hashPassword(password)); err != nil {
			return "", "", "", err
		}
	}
	token, tokenHash := newToken()
	if _, err := s.pool.Exec(ctx, `INSERT INTO sessions (token_hash, player_id) VALUES ($1, $2::uuid)`, tokenHash, playerID); err != nil {
		return "", "", "", err
	}
	return token, playerID, name, nil
}

// Logout ends token's session. Its player and games stay.
func (s *Store) Logout(ctx context.Context, token string) error {
	hash, ok := tokenHash(token)
	if !ok {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash)
	return err
}
