-- Phase 5: Versus rooms. Friends join a room with a code and race at the
-- same Wordle. The rules are in internal/rooms; these tables keep a room's
-- state between changes, and each player's race is an ordinary Wordle game.

CREATE TABLE rooms (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code             text NOT NULL UNIQUE,  -- 6 characters, see rooms.ParseCode
    -- NULL once the host's player is deleted; the store hands it on.
    host_id          uuid REFERENCES players ON DELETE SET NULL,
    gen_mask         smallint NOT NULL CHECK (gen_mask BETWEEN 1 AND 511),
    status           text NOT NULL CHECK (status IN ('lobby', 'playing', 'closed')),
    seq              integer NOT NULL DEFAULT 0,  -- bumped on every change
    last_activity_at timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
-- Rooms idle for a day are deleted (internal/cleanup).
CREATE INDEX rooms_last_activity_idx ON rooms (last_activity_at);

CREATE TABLE room_members (
    room_id         uuid NOT NULL REFERENCES rooms ON DELETE CASCADE,
    player_id       uuid NOT NULL REFERENCES players ON DELETE CASCADE,
    name            text NOT NULL,  -- the username when they joined, or "Guest 1A2B"
    ready           boolean NOT NULL DEFAULT false,
    joined_at       timestamptz NOT NULL,
    disconnected_at timestamptz,  -- NULL while connected
    PRIMARY KEY (room_id, player_id)
);
CREATE INDEX room_members_player_idx ON room_members (player_id);

CREATE TABLE room_rounds (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    room_id     uuid NOT NULL REFERENCES rooms ON DELETE CASCADE,
    n           integer NOT NULL CHECK (n >= 1),
    answer_id   integer NOT NULL REFERENCES pokemon,
    starts_at   timestamptz NOT NULL,  -- the end of the countdown
    ends_at     timestamptz NOT NULL,  -- the latest it can end
    finished_at timestamptz,           -- NULL while it's being played
    UNIQUE (room_id, n)
);

-- A round's games go with it: a room game is never played anywhere else.
ALTER TABLE games ADD COLUMN room_round_id uuid REFERENCES room_rounds ON DELETE CASCADE;
CREATE INDEX games_room_round_idx ON games (room_round_id) WHERE room_round_id IS NOT NULL;

CREATE TABLE room_round_players (
    round_id     uuid NOT NULL REFERENCES room_rounds ON DELETE CASCADE,
    player_id    uuid NOT NULL REFERENCES players ON DELETE CASCADE,
    position     smallint NOT NULL,  -- the order they joined the room in
    game_id      uuid NOT NULL REFERENCES games ON DELETE CASCADE,
    guesses      integer NOT NULL DEFAULT 0 CHECK (guesses >= 0),
    solved_at    timestamptz,
    forfeited_at timestamptz,
    PRIMARY KEY (round_id, player_id)
);
CREATE INDEX room_round_players_player_idx ON room_round_players (player_id);
CREATE INDEX room_round_players_game_idx ON room_round_players (game_id);

-- A WebSocket can't send an Authorization header, so a socket opens with a
-- single-use ticket instead, got with the player's token just before. Only
-- its sha256 is stored, like a session's.
CREATE TABLE ws_tickets (
    hash       bytea PRIMARY KEY CHECK (length(hash) = 32),
    player_id  uuid NOT NULL REFERENCES players ON DELETE CASCADE,
    room_id    uuid NOT NULL REFERENCES rooms ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
CREATE INDEX ws_tickets_expires_idx ON ws_tickets (expires_at);
CREATE INDEX ws_tickets_player_idx ON ws_tickets (player_id);
CREATE INDEX ws_tickets_room_idx ON ws_tickets (room_id);
