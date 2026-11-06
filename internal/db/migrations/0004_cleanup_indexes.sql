-- Phase 4: indexes for the cleanup jobs (internal/cleanup), so they find
-- what's stale without scanning whole tables.

-- Active games with no move for a day are abandoned.
CREATE INDEX games_active_updated_idx ON games (updated_at) WHERE status = 'active';

-- Sessions unused for a year are deleted, and guests are deleted when none of
-- their sessions has been used for 30 days.
CREATE INDEX sessions_last_used_idx ON sessions (last_used_at);

-- Guests created over 30 days ago are the ones checked for deletion.
CREATE INDEX players_guest_created_idx ON players (created_at) WHERE username IS NULL;
