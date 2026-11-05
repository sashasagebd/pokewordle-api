-- Phase 2: anonymous players and server-side games.

-- A player is anonymous until Phase 3 lets them claim a username.
CREATE TABLE players (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text UNIQUE,   -- Phase 3
    password_hash text,          -- Phase 3 (argon2id)
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- A session is a bearer token. Only its sha256 is stored, so a leaked table
-- can't be used to play as anyone.
CREATE TABLE sessions (
    token_hash   bytea PRIMARY KEY CHECK (length(token_hash) = 32),
    player_id    uuid NOT NULL REFERENCES players ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now()  -- updated at most hourly
);
CREATE INDEX sessions_player_idx ON sessions (player_id);

CREATE TABLE games (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    player_id  uuid NOT NULL REFERENCES players ON DELETE CASCADE,
    mode       text NOT NULL CHECK (mode IN ('wordle', 'higher_lower', 'whos_that')),
    gen_mask   smallint NOT NULL CHECK (gen_mask BETWEEN 1 AND 511),  -- bit n-1 is generation n
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'won', 'lost', 'abandoned')),
    score      integer NOT NULL DEFAULT 0 CHECK (score >= 0),  -- Wordle: guesses; others: streak
    started_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX games_player_active_idx ON games (player_id, mode) WHERE status = 'active';

-- One row per thing to answer: a Wordle guess (all rounds share the answer),
-- a Higher or Lower comparison, a Who's That silhouette. In the streak modes
-- the round after the open one is made in advance, so it can be preloaded.
CREATE TABLE game_rounds (
    game_id     uuid NOT NULL REFERENCES games ON DELETE CASCADE,
    n           integer NOT NULL CHECK (n >= 1),
    answer_id   integer NOT NULL REFERENCES pokemon,
    other_id    integer REFERENCES pokemon,  -- Higher or Lower: the Pokémon shown with its values
    stat        text CHECK (stat IN ('height', 'weight')),
    guess_id    integer REFERENCES pokemon,
    guess_dir   text CHECK (guess_dir IN ('higher', 'lower')),
    correct     boolean,
    answered_at timestamptz,  -- NULL while open
    PRIMARY KEY (game_id, n)
);
