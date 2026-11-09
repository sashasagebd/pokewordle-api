-- Phase 5: Versus in every mode. Higher or Lower and Who's That rooms are
-- survival races: everyone meets the same Pokémon in the same order (drawn
-- from the round's seed), one wrong answer ends a run, the longest streak
-- wins. Everything defaults to what a Wordle room was, so rooms made before
-- this, and the version before this one during a deploy, carry on as they
-- were.

ALTER TABLE rooms ADD COLUMN mode text NOT NULL DEFAULT 'wordle'
    CHECK (mode IN ('wordle', 'higher_lower', 'whos_that'));

ALTER TABLE room_rounds
    ADD COLUMN mode text NOT NULL DEFAULT 'wordle'
        CHECK (mode IN ('wordle', 'higher_lower', 'whos_that')),
    ADD COLUMN seed bigint;  -- every player's Pokémon are drawn from it

ALTER TABLE room_round_players
    ADD COLUMN score     integer NOT NULL DEFAULT 0 CHECK (score >= 0),  -- streak modes: the streak
    ADD COLUMN scored_at timestamptz,  -- when they reached it
    ADD COLUMN out_at    timestamptz;  -- when a wrong answer ended their run

-- A game's seed (NULL: drawn at random, as every solo game is).
ALTER TABLE games ADD COLUMN seed bigint;
