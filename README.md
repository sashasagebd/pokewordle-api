# pokewordle-api

The backend for [PokéWordle](https://github.com/sashasagebd/pokewordle). It
serves Pokémon data from its own Postgres, and will run the games on the
server so scores on the leaderboard can't be faked.

It runs on my home server's deploy platform
([ops-dashboard](https://github.com/sashasagebd/ops-dashboard)), with its own
platform-managed Postgres, and is public through Cloudflare Tunnel.

## Status

- [x] **Phase 1:** Pokémon data in Postgres, `GET /pokemon`, health check, deploy
- [ ] **Phase 2:** anonymous players and server-side games (the server holds the answer)
- [ ] **Phase 3:** usernames and leaderboards
- [ ] **Phase 4:** silhouette images, rate limits, cleanup of old games

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

## Pokémon data

The data lives in [internal/pokedex/pokemon.json](internal/pokedex/pokemon.json),
which is embedded in the binary and loaded into the `pokemon` table on every
start (rows that already match are left alone). The API never calls PokéAPI
itself. To refresh the file from PokéAPI, then review the diff and commit it:

```sh
go run ./cmd/genseed
```

It checks the data before writing it (all 1,025 Pokémon, generations matching
the dex ranges, one or two known types), and so does the API at startup.

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
