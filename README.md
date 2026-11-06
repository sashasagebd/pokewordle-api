# pokewordle-api

The backend for [PokéWordle](https://github.com/sashasagebd/pokewordle). It
serves Pokémon data from its own Postgres, and will run the games on the
server so scores on the leaderboard can't be faked.

It runs on my home server's deploy platform
([ops-dashboard](https://github.com/sashasagebd/ops-dashboard)), with its own
platform-managed Postgres, and is public through Cloudflare Tunnel.

## Status

- [x] **Phase 1:** Pokémon data in Postgres, `GET /pokemon`, health check, deploy
- [x] **Phase 2:** anonymous players, server-side games (the server holds the answer), Who's That silhouettes
- [x] **Phase 3:** usernames (argon2id), best scores, leaderboards
- [ ] **Phase 4:** rate limits for the whole API, cleanup of old games and players

## API

| Request | Answer |
|---|---|
| `GET /healthz` | `ok` when the database answers, 503 otherwise. The platform's health check. |
| `GET /pokemon?gens=1,2` | Pokémon of those generations (1 to 9) in dex order. All generations without `gens`. |
| `GET /pokemon/{id or name}` | One Pokémon, e.g. `/pokemon/25` or `/pokemon/pikachu`. 404 if there's no such Pokémon. |

A Pokémon looks like this (height in decimetres and weight in hectograms,
PokéAPI's units):

```json
{"id":25,"name":"pikachu","generation":1,"height":4,"weight":60,"types":["electric"]}
```

Errors are `{"error": "..."}` with a 4xx or 5xx status.

### Games

The server holds every answer and shows only what the player has earned, so
a game's score is what was actually played. (It can't stop someone looking
Pokémon up, or a bot: in Higher or Lower the next Pokémon's name is shown, and
`/pokemon` is public.)

Players are anonymous. `POST /players` returns `{"token": "..."}`, sent from
then on as `Authorization: Bearer <token>`. Only its sha256 is stored, so a
lost token can't be recovered: the frontend just gets a new one. A missing or
unknown token is a 401. Game responses are `Cache-Control: no-store`.

| Request | Answer |
|---|---|
| `POST /players` | `{"token": "..."}` (201) |
| `POST /games` `{"mode", "gens": [1, 2]}` | A new game (201). Modes: `wordle`, `higher_lower`, `whos_that`. No `gens` means all. Abandons the player's game of that mode in progress. |
| `GET /games/{id}` | The game as the player may see it, e.g. to pick it up after a reload. |
| `POST /games/{id}/guess` `{"name"}` | Wordle: `{"result": comparison, "game": game}`. |
| `POST /games/{id}/answer` `{"n", "dir", "stat"}` or `{"n", "name"}` | Higher or Lower (`dir` `higher`/`lower`, `stat` `height`/`weight`) or Who's That: `{"correct", "answer": Pokémon, "game": game}`. |
| `POST /games/{id}/give-up` | Ends the game as lost and reveals the answer. |
| `GET /games/{id}/rounds/{n}/image` | Who's That: round `n`'s silhouette (PNG), for the open round and the one after it (to preload). |

A game:

```json
{"id": "…", "mode": "higher_lower", "gens": [1], "status": "active", "score": 3,
 "round": {"n": 4, "current": {"id": 25, "name": "pikachu", "...": "…"},
           "next": {"id": 6, "name": "charizard"}, "upcoming": {"id": 81, "name": "magnemite"}}}
```

- `status` is `active`, `won` (Wordle), `lost` (a wrong answer, or giving up) or `abandoned`.
- `score` is the number of guesses in Wordle and the streak in the others.
- `round` (streak modes, while active): Higher or Lower shows `current` with its
  values and `next` by name only. `upcoming` is the Pokémon after `next`, for
  preloading. Who's That shows only `n`.
- `guesses` (Wordle) lists the comparisons so far, oldest first.
- `answer` is the Pokémon revealed once the game is over.

A Wordle comparison, `{"guess": Pokémon, "height", "weight", "type", "correct"}`:
`height` and `weight` say where the answer is relative to the guess
(`higher`, `lower`, `equal`). `type` compares first types: `same`, `effective`
(the guess's type is super effective against the answer's), `weak` (the
reverse) or `neutral`.

Mistakes that don't count as a move are 400s with a message for the player
(an unknown Pokémon, one outside the game's generations, a repeated guess).
Moves on a finished game are 409. Answers carry the round `n` they answer:
answering a round that's already been answered (a retried request) is a 409
with the game as it is now, `{"error", "game"}`, so the frontend can catch up.
Someone else's game is a 404, the same as a missing one.

### Accounts and leaderboards

A guest can claim a username and password, keeping everything they've played,
and log in with them on another device for a new token to the same player.
Logging in with a guest's token merges the guest into the account: its games
move over, each best score keeps whichever is better, and the guest is
deleted. Passwords are hashed with argon2id (19 MiB, 2 passes, at most 2
hashes at a time to stay inside the container's memory). The two password
endpoints allow 10 tries a minute per IP (`CF-Connecting-IP`), and answer 429
past that.

| Request | Answer |
|---|---|
| `GET /me` | `{"username": null or "...", "best": [{"mode", "gens", "best", "gameId", "achievedAt"}]}` |
| `POST /me/username` `{"username", "password"}` | Claims a name: 3 to 20 letters, digits, `_` or `-`, unique ignoring case. Passwords are 8 to 128 characters. 409 if it's taken or the player already has one. |
| `POST /sessions` `{"username", "password"}` | Logs in: `{"token", "username"}` (201). 401 for a wrong name or password, without saying which. Merges the guest if sent with a guest's token. |
| `DELETE /sessions/current` | Logs out: the token stops working (204). |
| `GET /leaderboard?mode=&gens=1,2&limit=20` | `{"entries": [{"rank", "username", "best", "achievedAt"}], "me": {"rank", "best"}}`. Named players only. `me` comes with a token, when the player has a best on that board; its `rank` is null for a guest. Cached for 30s without a token. |

A best score is per mode and exact set of generations: the fewest guesses in
a won Wordle, the longest streak otherwise. Streaks count as they grow, in the
same transaction as the right answer, and a best only ever improves. Ties go
to whoever got there first.

## Pokémon data

The data lives in [internal/pokedex/pokemon.json](internal/pokedex/pokemon.json),
which is embedded in the binary and loaded into the `pokemon` table on every
start (rows that already match are left alone). The sprites Who's That makes
silhouettes from are in [internal/pokedex/sprites/](internal/pokedex/sprites/)
(PokéAPI's default sprites, about 3 MB), also embedded. The API never calls
PokéAPI itself. To refresh them, then review the diff and commit it:

```sh
go run ./cmd/genseed               # both
go run ./cmd/genseed -only data    # just pokemon.json
go run ./cmd/genseed -only sprites # just the sprites
```

It checks the data before writing it (all 1,025 Pokémon, generations matching
the dex ranges, one or two known types, sprites that decode), and so does the
API at startup.

## Running it locally

You need Go 1.26 and a Postgres to point it at:

```sh
DATABASE_URL='postgres://app:secret@localhost:5432/app?sslmode=disable' go run .
curl localhost:8080/pokemon/pikachu
```

Checks (the same as CI):

```sh
gofmt -l . && go vet ./... && go test ./...
```

The database tests skip without `DATABASE_URL`; CI runs them against a real
Postgres.

## Configuration

| Variable | Default | What |
|---|---|---|
| `DATABASE_URL` | required | Set by the platform once the app has a database. |
| `ALLOWED_ORIGINS` | `https://pokewordle.sashasagebd.com,https://pokewordle.sashasagebd.workers.dev,http://localhost:5173` | Browser origins allowed to call the API (CORS), comma-separated. |
| `PORT` | `8080` | Port to listen on. |

## Deploying on the platform

1. On the dashboard's Apps page, add an app:

   | Field | Value |
   |---|---|
   | Name | `pokewordle-api` |
   | Repo URL | `https://github.com/sashasagebd/pokewordle-api` |
   | Port | `8080` |
   | Health check path | `/healthz` |

2. Press **Add a database** on its page, then deploy. The first start creates
   the tables and loads the Pokémon. Until the database answers, the app
   doesn't listen, so a deploy without one fails its health check instead of
   going live broken.
3. Make it public (**Public access**, name `pokewordle-api`), so it's served
   at `https://pokewordle-api.<PUBLIC_DOMAIN>`. The frontend's API URL
   points there, so renaming it means updating the frontend too.

Pushes to `main` deploy automatically.

**Schema changes must be backward compatible.** A deploy runs the new version
next to the old one against the same database until the switch, and a
rollback runs an older image against the newer schema. So migrations only add
things; a column or table can be dropped only once no deployed version reads
it.
