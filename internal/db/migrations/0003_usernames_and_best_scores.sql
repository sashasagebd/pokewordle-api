-- Phase 3: usernames and leaderboards.

-- Usernames are shown as typed but unique ignoring case. (players.username's
-- own UNIQUE stays: it's implied by this one, and dropping it isn't worth a
-- deploy.)
CREATE UNIQUE INDEX players_username_lower_idx ON players (lower(username));

-- Each player's best for each mode and exact set of generations: fewest
-- guesses in a won Wordle, longest streak in the others. Written in the same
-- transaction as the move that set it, and only ever improved.
CREATE TABLE best_scores (
    player_id   uuid NOT NULL REFERENCES players ON DELETE CASCADE,
    mode        text NOT NULL CHECK (mode IN ('wordle', 'higher_lower', 'whos_that')),
    gen_mask    smallint NOT NULL CHECK (gen_mask BETWEEN 1 AND 511),
    best        integer NOT NULL CHECK (best >= 1),
    game_id     uuid NOT NULL REFERENCES games ON DELETE CASCADE,
    achieved_at timestamptz NOT NULL,
    PRIMARY KEY (player_id, mode, gen_mask)
);
CREATE INDEX best_scores_board_idx ON best_scores (mode, gen_mask, best, achieved_at);

-- The games played before this migration. A score was reached when the round
-- that set it was answered (the winning guess, the last right answer).
INSERT INTO best_scores (player_id, mode, gen_mask, best, game_id, achieved_at)
SELECT DISTINCT ON (g.player_id, g.mode, g.gen_mask)
    g.player_id, g.mode, g.gen_mask, g.score, g.id,
    coalesce((SELECT r.answered_at FROM game_rounds r WHERE r.game_id = g.id AND r.n = g.score), g.updated_at)
FROM games g
WHERE (g.mode = 'wordle' AND g.status = 'won') OR (g.mode <> 'wordle' AND g.score > 0)
ORDER BY g.player_id, g.mode, g.gen_mask,
    CASE WHEN g.mode = 'wordle' THEN g.score ELSE -g.score END,
    g.updated_at;
