-- Quick match: players waiting for an opponent in a mode and set of
-- generations are paired, oldest first, into a room of their own that starts
-- by itself once both are connected.

-- Who's waiting. A player waits for one thing at a time; asking again (each
-- poll does) refreshes last_seen_at, and one not seen for a few seconds has
-- gone (closed the tab) and isn't matched.
CREATE TABLE match_queue (
    player_id    uuid PRIMARY KEY REFERENCES players ON DELETE CASCADE,
    mode         text NOT NULL CHECK (mode IN ('wordle', 'higher_lower', 'whos_that')),
    gen_mask     smallint NOT NULL CHECK (gen_mask BETWEEN 1 AND 511),
    enqueued_at  timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL
);
CREATE INDEX match_queue_bucket_idx ON match_queue (mode, gen_mask, enqueued_at);

-- A match made for a player who wasn't the one asking: their next poll picks
-- up the room's code (and deletes this).
CREATE TABLE match_found (
    player_id uuid PRIMARY KEY REFERENCES players ON DELETE CASCADE,
    room_code text NOT NULL,
    found_at  timestamptz NOT NULL
);

-- A quick-match room starts its first round by itself, once both players
-- are connected.
ALTER TABLE rooms ADD COLUMN quick boolean NOT NULL DEFAULT false;
