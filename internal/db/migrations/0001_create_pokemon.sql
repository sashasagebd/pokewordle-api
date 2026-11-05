-- Reference data, filled from the embedded seed file on every start.
CREATE TABLE pokemon (
    id          integer PRIMARY KEY CHECK (id > 0),       -- national dex number
    name        text NOT NULL UNIQUE CHECK (name <> ''),  -- PokéAPI's name, e.g. 'mr-mime'
    generation  smallint NOT NULL CHECK (generation BETWEEN 1 AND 9),
    height      integer NOT NULL CHECK (height > 0),      -- decimetres, as PokéAPI gives it
    weight      integer NOT NULL CHECK (weight > 0),      -- hectograms, as PokéAPI gives it
    types       text[] NOT NULL CHECK (cardinality(types) BETWEEN 1 AND 2) -- in slot order
);

CREATE INDEX pokemon_generation_idx ON pokemon (generation);
