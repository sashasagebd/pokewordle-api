// Package auth gives players anonymous bearer tokens and finds the player
// behind one.
//
// A token is 32 random bytes, base64url-encoded. Only its sha256 is stored.
// The frontend keeps it in localStorage and sends it as
// "Authorization: Bearer <token>"; not a cookie, because the frontend and the
// API are on different sites.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnknownToken means the token isn't one this API gave out (or its player
// is gone).
var ErrUnknownToken = errors.New("unknown token")

// Store reads and writes the players and sessions tables.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store using pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// NewPlayer makes an anonymous player and returns its token and id.
func (s *Store) NewPlayer(ctx context.Context) (token, playerID string, err error) {
	raw := make([]byte, 32)
	rand.Read(raw) // never fails (crypto/rand panics instead)
	token = base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256(raw)

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO players DEFAULT VALUES RETURNING id::text`).Scan(&playerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sessions (token_hash, player_id) VALUES ($1, $2::uuid)`, hash[:], playerID)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return token, playerID, nil
}

// PlayerFor returns the id of the player token belongs to.
func (s *Store) PlayerFor(ctx context.Context, token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return "", ErrUnknownToken
	}
	hash := sha256.Sum256(raw)

	var playerID string
	var stale bool
	err = s.pool.QueryRow(ctx, `
		SELECT player_id::text, last_used_at < now() - interval '1 hour'
		FROM sessions WHERE token_hash = $1`, hash[:]).Scan(&playerID, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnknownToken
	}
	if err != nil {
		return "", err
	}
	// Recording every use would be a write per request; hourly is enough to
	// tell an abandoned session (Phase 4's cleanup) from a live one.
	if stale {
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_used_at = now() WHERE token_hash = $1`, hash[:]); err != nil {
			return "", err
		}
	}
	return playerID, nil
}
