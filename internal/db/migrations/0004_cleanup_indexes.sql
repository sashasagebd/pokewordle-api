-- Phase 4: indexes for the cleanup jobs (internal/cleanup), so they find
-- what's stale without scanning whole tables.

-- Active games with no move for a day are abandoned.
CREATE INDEX games_active_updated_idx ON games (updated_at) WHERE status = 'active';

-- Sessions unused for a year are deleted, and guests are deleted when none of
-- their sessions has been used for 30 days.
CREATE INDEX sessions_last_used_idx ON sessions (last_used_at);

-- Guests created over 30 days ago are the ones checked for deletion.
CREATE INDEX players_guest_created_idx ON players (created_at) WHERE username IS NULL;

-- Deleting a row makes Postgres find the rows referencing it (to cascade),
-- and without these each lookup scans the whole table: once per guest deleted
-- (games.player_id; games_player_active_idx only covers active games) and
-- once per game deleted with it (best_scores.game_id). The first also serves
-- the guest-to-account merge's UPDATE games ... WHERE player_id.
CREATE INDEX games_player_idx ON games (player_id);
CREATE INDEX best_scores_game_idx ON best_scores (game_id);
