# Versus: real-time multiplayer rooms

The plan for Phase 5: friends race each other at the same game (any of the
three modes) in a room they join with a code, see each other's progress live, and the server decides
who won. It builds on what's already here: Wordle games run server-side by
`games.Engine`, kept in Postgres, each move a transaction with the game's row
locked, retries made harmless by the round number `n`.

Code paths are in this repo; phase 4 is in the frontend,
[pokewordle](https://github.com/sashasagebd/pokewordle).

## Status

- [x] **1. Rules:** `internal/rooms`, the pure rules of a room (no database, no clock), with tests
- [x] **2. Store:** migration, `rooms.Store`, socket tickets, solo games ignoring room games, cleanup
- [x] **3. Sockets:** `/rooms` endpoints, `/ws`, a hub per instance (`internal/live`), `LISTEN`/`NOTIFY` between instances, closing with 1012 on shutdown
- [x] **4. Frontend:** create or join by code, lobby, countdown, race, results, rematch (in `pokewordle`: `/versus`, `useRoom`)
- [x] **5. Every mode:** Higher or Lower and Who's That in rooms too, as survival races over the same Pokémon ([below](#every-mode))
- [ ] **6. Resilience:** prove it end to end: an instance stopped mid-round, its clients carrying on through the other; then a real deploy. (Grace, forfeits, expiry, 1012 on shutdown and reconnecting for the latest room are built.)
- [ ] **7. Load and observability:** a bot load generator, per-room logs, latency numbers in the README
- [ ] **Later:** a quick-match queue (it just pairs two players into a room), ping-compensated times, ratings

Phases 1 to 4 make it playable, 5 makes it every game, and 6 and 7 make it
solid.

## How it plays

1. A player creates a room, picks the generations, and gets a code like
   `K7QX2M` and a link, `/versus/K7QX2M`. Codes are 6 characters from an
   alphabet without look-alikes (no 0/O, 1/I/L): 31⁶, about 900 million.
2. Friends join with the code, up to 4 in a room. The lobby shows who's in,
   who's ready, and the host's settings. Changing the settings unreadies
   everyone, so nobody starts a round they didn't agree to.
3. Once everyone else is ready, the host starts the round: a 3 second
   countdown on the server's clock, then everyone gets the same answer.
4. The first to solve it wins. Everyone else is ranked by when they solved it
   (then by guesses), then those who didn't, then those who forfeited.
   - A round lasts at most 5 minutes, and at most 1 more minute after the
     first solve, so the winner isn't kept waiting.
   - It ends as soon as at most one player is still playing: their place
     can't change any more. With two players, the first solve ends it.
   - If everyone else forfeited, the last one standing wins. Nobody solving
     it is a draw.
5. While playing you see each opponent's guess count and result tiles
   (height, weight, type), never which Pokémon they guessed: that would give
   their deductions away. Everyone's guesses are shown once the round ends.
6. After a round the room is back in its lobby: **rematch** is just starting
   again. Someone who joins during a round waits in the room and plays the
   next one.

Room games don't count toward solo best scores or leaderboards.

## Every mode

The host picks the mode in the lobby, with the generations (changing either
unreadies everyone). PokéWordle plays as above. Higher or Lower and Who's
That are streak games, so in a room they're a **survival race**:

1. **Everyone gets the same Pokémon in the same order**, however fast each
   goes: in Higher or Lower the same pairs, in Who's That the same
   silhouettes. Each player still chooses height or weight per pair, as in
   solo.
2. **A wrong answer ends your run.** Your streak is your score. Giving up,
   or your grace running out, ends it too, keeping the streak.
3. **The longest streak wins**, ties going to whoever reached it first.
   A streak of 0 all round is a draw.
4. **The round ends** when everyone's run is over, at the time limit
   (5 minutes, as now), or as soon as the only one still going is ahead of
   everyone else: they've won, and nothing they do now changes a place. One
   still going but behind the leader plays on, to catch them or not. No
   finish window: that was so Wordle's winner isn't kept waiting.
5. **You see each opponent's streak and whether they're still going**,
   never their answers: in Who's That, someone's right answer *is* the
   answer to the same silhouette you're looking at. Once the round ends,
   the results show each run's streak and the Pokémon it ended on.

### What changes

- **Rules** (`internal/rooms`): a room and each round have a mode. A
  player's race gains a score, when they reached it, and when their run
  ended. `Answer(player, correct, now)` is the streak mode's move, beside
  Wordle's `Guess`. Ranking, the winner and the round's end are per mode;
  Wordle's stay as they are.
- **The same sequence for everyone:** a round stores a random seed, and
  round *k* of each player's game draws its Pokémon from the seed and *k*
  alone (still never repeating the one before), so players at different
  speeds meet the same ones. `games.Engine` gets a seeded way to add rounds.
- **Schema:** [0006_versus_modes.sql](internal/db/migrations/0006_versus_modes.sql)
  (0005 is deployed, so it stays as it is): `rooms.mode` (default `wordle`,
  so existing rooms keep working), `room_rounds.mode` and `seed`,
  `room_round_players.score`, `scored_at` and `out_at`, and `games.seed`.
- **Who's That silhouettes** come from the existing
  `GET /games/{id}/rounds/{n}/image`, which already only serves your own
  game's open round and the one after it.
- **Socket:** `settings` gains `mode`; streak moves are
  `{"type": "answer", "n", "dir", "stat"}` (Higher or Lower) or
  `{"type": "answer", "n", "name"}` (Who's That), with `n` the round
  answered, as in solo.
- **Views:** each player shows `score`, `racing` and, once it's over,
  `endedOn`. `game` is your own game as its solo mode shows it.
- **Frontend:** a mode picker in the lobby; the race screens reuse the solo
  modes' pieces (the pair and its buttons, the silhouette and its search)
  beside the standings.
- **Tests:** rules for survival races (ties on time, the last one standing
  ahead or behind, the time limit), the seeded sequence being the same for
  every player whatever their pace, and a two-instance socket test per mode.

## Architecture

**Rules are pure.** `internal/rooms` holds a room's state and every change
to it, each taking the time it happens at. No timers inside: `Advance(now)`
applies whatever deadlines have passed (the round's end, a disconnected
player's grace running out, an idle room closing) and every change calls it
first, so the state is right however late a timer fires. `Deadline()` says
when the next one is due, for the hub's timer.

**Each player's race is an ordinary Wordle game.** Starting a round makes
one `games` row per player with the shared answer, so guesses go through
the existing `Engine.Guess` and `Store.move`. In the same transaction the
store tells the room (`Room.Guess`); if the room says no (the round is over),
the whole move rolls back.

**Postgres is the source of truth; memory only holds sockets.** A deploy
runs the new version next to the old one against the same database, and
then stops the old one, so two players in one room can be on different
instances, and every deploy disconnects everyone. So:

- Each instance has a hub: room ID → its own sockets there.
- A change commits, then `NOTIFY room_events, '<roomId>:<seq>'`.
- Each instance keeps one dedicated connection `LISTEN`ing, and on a notify
  reloads the room and pushes to its local sockets.
- On shutdown, sockets are closed with code 1012 (service restart):
  `srv.Shutdown` doesn't close hijacked connections by itself. Clients
  reconnect straight away, to whichever version is live.

**The clock is the database's.** Times come from `now()`, never an
instance's clock. Ending a round is idempotent
(`UPDATE … WHERE … AND finished_at IS NULL`), so whichever instance gets
there first does it. Each instance with sockets in a room sets a timer for
its `Deadline()`, and a once-a-second sweeper covers rooms nobody is
connected to.

**Sockets** use `github.com/coder/websocket`.

- Auth: browsers can't send `Authorization` on a WebSocket, and a token in
  the URL would end up in logs. So `POST /rooms/{code}/ticket` (bearer auth
  as usual) returns a single-use ticket good for 30 seconds, and the socket
  is `GET /ws?ticket=…`.
- CORS doesn't cover WebSockets, so `Origin` is checked against
  `ALLOWED_ORIGINS` by hand.
- The server's `WriteTimeout` would kill a socket after 30 seconds:
  clear it per connection with `http.ResponseController`. The middleware
  mustn't hide `http.Hijacker`.
- Pings every 25 seconds: Cloudflare Tunnel drops idle connections at
  about 100.
- Several tabs can share a player: the hub counts a player's sockets and
  only tells the room they disconnected when the last one closes.

**Disconnects.** A member counts as disconnected from when their socket
closes (or from joining, until it opens). Disconnecting unreadies them.
After 30 seconds of grace they forfeit any round they're playing, and are
removed from the lobby. Reconnecting in time changes nothing; their clock
kept running.

**Fairness.** For now, a synced countdown: the server sends
`{startsAt, serverNow}`, so each client knows its clock offset and starts
on the same moment. Times are credited when the server receives the guess.
Ping-compensated credit (receive time minus half the measured round trip,
capped at 150 ms so faking lag doesn't pay) can come later.

## Schema

[internal/db/migrations/0005_rooms.sql](internal/db/migrations/0005_rooms.sql)
adds `rooms`, `room_members`, `room_rounds`, `room_round_players` (each
player's race: their game, guesses, when they solved it or forfeited) and
`ws_tickets`, and `games.room_round_id`, marking a game as a Versus race.

- A room game is only played through its room: the solo endpoints answer
  409 for one, `POST /games` doesn't abandon one, and none counts toward
  best scores. When a round ends, the games of those who didn't solve it
  end as lost, which reveals the answer.
- During a deploy the old version doesn't know `room_round_id`, so starting
  a solo Wordle there would abandon a room game in progress. Short-lived and
  harmless enough, but worth knowing.
- Every change to a room is one transaction with its row locked, timed by
  `clock_timestamp()` once the lock is held (not `now()`, the transaction's
  start: a guess that waited for the lock would be timed before the change
  it waited for). A move the rules refuse is undone with a savepoint, but
  deadlines that had passed are still saved.
- Cleanup deletes rooms idle for a day (with their rounds and games) and
  expired tickets.
- Logging a guest in to an account moves their games, but their place in a
  room is lost with the guest: they join again.

## API

| Request | What |
|---|---|
| `POST /rooms` `{gens}` | Makes a room with you as host: `{code}` (201). |
| `POST /rooms/{code}/join` | Joins (again, harmlessly, if you're in): `{code}`. 404 for an unknown or closed room, 409 if it's full. |
| `POST /rooms/{code}/ticket` | `{ticket}` (201) for the socket, good for 30 seconds, once. 403 if you're not in the room. |
| `GET /ws?ticket=` | The socket. 401 for a bad ticket, 403 from an origin not in `ALLOWED_ORIGINS` (no `Origin`, not a browser, is fine). |

Joining and making rooms share the 2-a-second `POST` bucket, which makes
guessing codes hopeless.

### The socket

Messages are JSON objects with a `type`. There's no resuming: a socket
opening gets the room as it is, and every change after sends the whole room
again (a room is small). A client keeps the one with the highest `seq`.

| Client → server | |
|---|---|
| `{"type": "ready", "ready": true}` | Lobby |
| `{"type": "settings", "mode": "whos_that", "gens": [1, 2]}` | Host, lobby; unreadies everyone. No `mode` keeps it |
| `{"type": "start"}` | Host, everyone else ready |
| `{"type": "guess", "n": 1, "name": "pikachu"}` | Wordle. `n` counts your guesses from 1, so a resend is refused, not played twice |
| `{"type": "answer", "n", "dir", "stat"}` or `{"type": "answer", "n", "name"}` | Higher or Lower, or Who's That: `n` is the round answered, as in solo |
| `{"type": "forfeit"}`, `{"type": "leave"}` | |

| Server → client | |
|---|---|
| `{"type": "room", "seq", "serverNow", "room"}` | On opening and every change: the room as you may see it (below) |
| `{"type": "guess", "n", "result", "game"}` | Your guess's comparison and your game, as solo Wordle answers |
| `{"type": "answer", "n", "correct", "answer", "game"}` | Your answer's result, the Pokémon revealed and your game, as solo answers |
| `{"type": "error", "for", "n", "error"}` | A message that couldn't be acted on; `error` is meant for the player |

The room: `code`, `status` (`lobby`, `playing`, `closed`), `mode`, `gens`, `you`
and `host` (public IDs: stable in the room, never a player ID), `members`
(`id`, `name`, `ready`, `connected`, `graceEndsAt`), and `round`, the one
being played or the last: `n`, `mode`, `startsAt` (the end of the
countdown), `endsAt`, `finishedAt`, `players` in their places (`id`,
`name`, `rank`, `outcome`, `racing`, `timeMs`; Wordle: `guesses`, `tiles`
without the Pokémon, and once it's over `comparisons` in full; the streak
modes: `score`, and once it's over `endedOn`), `winner`, Wordle's `answer`
once it's over, and `game`, your own race as its solo mode shows it. Times are the server's;
`serverNow` gives the client its clock's offset.

Close codes: 1012 the server is restarting (reconnect), 4000 the room has
closed (don't), 4001 you're not in the room (join again, then reconnect).
At most 5 messages a second (bursts of 10), 1 KB each; a client more than
32 messages behind is disconnected, to reconnect for the latest.

## Testing

- **Rules:** table tests with a fake clock: races, ties, the finish
  window, timeouts, forfeits, grace, host handover, rematches, expiry.
- **Store:** against Postgres, as CI already does.
- **Sockets:** `httptest` with real `coder/websocket` clients playing whole
  rounds, **with the clients split across two API instances** on one
  database, so `LISTEN`/`NOTIFY` is what carries the game.
- **Resilience:** stop an instance mid-round; its clients reconnect to the
  other and the round finishes right. Then for real, during a deploy.
- **Load:** `cmd/bots` plays many rooms at once against a deployed API and
  reports guess-to-broadcast latency (p50, p99), for a table in the README.
