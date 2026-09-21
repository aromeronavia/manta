# Manta

[![Build Status](https://github.com/dotabuff/manta/actions/workflows/ci.yml/badge.svg)](https://github.com/dotabuff/manta/actions/workflows/ci.yml)

Manta is a fully functional Dota 2 replay parser written in [Go](https://golang.org), targeting the Source 2 (Dota 2 Reborn) game engine.

## Getting Started

Manta is a low-level replay parser, meaning that it will provide you access to the raw data in the replay, but doesn't provide any opinion on how that data should be structured for your use case. You'll need to create callback functions, inspect the raw data, and decide how you're going to use it.

Three commands built on the parser ship with it, see [Tools](#tools):
`manta-explorer` browses the raw streams a replay produces, `manta-map`
replays a match on the minimap with scoreboard, farming, itemization and feed
panels, and `manta-server` serves that viewer by match id.

## Usage

Get the code:

    go get github.com/dotabuff/manta

Use it to parse a replay:

```go
import (
  "log"
  "os"

  "github.com/dotabuff/manta"
  "github.com/dotabuff/manta/dota"
)

func main() {
  // Create a new parser instance from a file. Alternatively see NewParser([]byte)
  f, err := os.Open("my_replay.dem")
  if err != nil {
    log.Fatalf("unable to open file: %s", err)
  }
  defer f.Close()

  p, err := manta.NewStreamParser(f)
  if err != nil {
    log.Fatalf("unable to create parser: %s", err)
  }

  // Register a callback, this time for the OnCUserMessageSayText2 event.
  p.Callbacks.OnCUserMessageSayText2(func(m *dota.CUserMessageSayText2) error {
    log.Printf("%s said: %s\n", m.GetParam1(), m.GetParam2())
    return nil
  })

  // Start parsing the replay!
  p.Start()

  log.Printf("Parse Complete!\n")
}
```

## Tools

Manta ships three commands built on the parser. The two command line tools
take one replay path, which may be a plain `.dem` or bzip2 / Zstandard
compressed; the container is detected from the file header, not the
extension. The third is a web service that fetches replays by match id.

    go run ./cmd/manta-explorer path/to/replay.dem
    go run ./cmd/manta-map path/to/replay.dem

or install them:

    go install github.com/dotabuff/manta/cmd/manta-explorer@latest
    go install github.com/dotabuff/manta/cmd/manta-map@latest

The Makefile has shortcuts: `make explorer REPLAY=path/to/replay.dem` and
`make map REPLAY=path/to/replay.dem`.

### manta-explorer: browse what the parser produces

A local web UI over the raw streams manta decodes from a replay, meant for
debugging the parser and finding the fields you need:

- **Entity ops**: every create, update, enter, leave and delete with the class,
  index and serial. Selecting an operation can show the entity's complete
  state at that tick, or one tick earlier, to see what changed.
- **Game events**: `CMsgSource1LegacyGameEvent` entries decoded by name.
- **Combat log**: `CMsgDOTACombatLogEntry` rows with names resolved through the
  `CombatLogNames` string table, and the full message on request.
- **String tables**: every create/update with the table and the number of
  entries it changed, plus the final keys of each table, searchable.

Each stream has a type or class picker with counts, a tick range, and paging;
entity ops can also be filtered by entity index. Rows deep-link as
`#entities/<row>/inspect`.

    manta-explorer [flags] replay.dem        # then open http://127.0.0.1:8080

| Flag | Default | Meaning |
|------|---------|---------|
| `-addr` | `127.0.0.1:8080` | address to listen on |

The whole replay is indexed at startup, about a second for a full match, and
held in memory. Inspecting state at a tick re-parses the replay up to that
tick on demand, well under a second. The explorer uses only manta's public
API, so it cannot show which fields an update changed or string table values;
compare an entity's state at consecutive ticks instead.

### manta-map: replay a match on the minimap

Plays a match back on the minimap with a scrubbable timeline and analysis
panels, and can write the whole viewer to one self-contained HTML file.

- **Map**: hero icons with movement trails and health and mana bars, kills, towers,
  barracks and ancients (greyed once destroyed), observer and sentry wards,
  couriers and Roshan. Kill banners and item purchase alerts pop up as the
  playhead crosses them, like the in-game notices. Layers toggle individually.
- **Camera**: scroll to zoom from 1× to 8× around the cursor, drag to pan, and
  double-click a hero (or press F with one selected) to lock the camera on it
  as it moves. Buttons in the map's corner zoom, follow and reset; `0` resets.
- **Hero card**: click a hero on the map, in the top bar or in the scoreboard
  for its loadout as the in-game HUD shows it: abilities with level pips and
  cooldown sweeps counting down, and the inventory, backpack, teleport scroll
  and neutral item with their cooldowns and charge counts. Escape or × closes
  it.
- **Scoreboard**: level, K/D/A, net worth and items per player, with net worth
  and experience advantage charts you can click to seek.
- **Farming**: one hero at the playhead: GPM, XPM, last hits (with the
  10-minute mark), denies, neutral kills, stacks, farm uptime and time dead,
  each ranked against the other nine; a heatmap of where the hero farmed with a
  lane/jungle split inferred from position; creep score per minute; GPM and XPM
  curves; gold by source; item timings; and a comparison table.
- **Itemization**: the same hero against [OpenDota]: the player's per-minute
  rates as percentiles of the hero's recent matches (`/benchmarks`), the hero's
  popular items per game phase in professional matches against what the player
  bought (`/heroes/{id}/itemPopularity`, using OpenDota's phase and cost
  filters), and games and win rates by purchase time for the core items bought
  (`/scenarios/itemTimings`).
- **Feed**: kills, chat and notable events (first blood, towers, Roshan,
  buybacks) up to the playhead.

A Dota-style top bar sits over the map: both teams' hero portraits with colour
strip, health, mana, level and respawn countdown flank the kill counts, the
game clock and the day/night dial (driven by the replay's own time-of-day
counter). Under each portrait: last hits/denies, net worth with its rank among
the ten players, and K/D/A. Clicking a portrait selects that hero. The » button
in the tab row collapses the side panel so the map can have the whole window;
the choice is remembered in the browser.

    manta-map [flags] replay.dem             # then open http://127.0.0.1:8081
    manta-map -out match.html replay.dem     # write one self-contained HTML file

| Flag | Default | Meaning |
|------|---------|---------|
| `-addr` | `127.0.0.1:8081` | address to listen on |
| `-out` | | write a self-contained HTML file to this path instead of serving |
| `-interval` | `30` | ticks between samples (30 is one per second) |
| `-assets` | | directory holding `minimap/<version>.webp`; a [ReDota] checkout beside the repository is found automatically |
| `-map` | | force a map edition (`7.23`, `7.29`, `7.33`, `7.38`, `7.40`) instead of picking by match date |
| `-opendota` | `true` | fetch item popularity, benchmarks and item timings from OpenDota |
| `-timings` | `true` | with `-opendota`, also fetch timing scenarios for the core items each hero bought (one request per hero and item) |
| `-opendota-cache` | user cache dir | where OpenDota responses are cached for a week |

Sampling the replay takes about a second. Without a minimap image the viewer
draws a schematic map; hero and item icons load from Valve's CDN. OpenDota
data loads in the background when serving and the Farming tab fills in when it
arrives; exports embed it. The first fetch for a match is paced to about one
request per second (roughly a minute and a half for ten heroes with timings),
later runs are served from the cache.

The viewer accepts URL parameters to open at a moment:
`#t=12:34&panel=farm&hero=5&select=3&follow=1&speed=8&play=1` (`panel` is
`score`, `farm`, `item` or `feed`; `hero` and `select` are player ids;
`follow=1` locks the camera on the selected hero).

### manta-server: the viewer as a web service

`cmd/manta-server` wraps the same viewer in a deployable service: enter a
match id on the landing page and it locates the replay through OpenDota
(`/api/matches/{id}`, asking OpenDota to fetch the match first when it has not
seen it), downloads it from Valve's replay servers, parses it and opens the viewer
at `/match/<id>/`. Processed matches persist in the data directory and are
listed on the landing page; OpenDota reference data is fetched in the
background after a match is ready.

    docker build -t manta-server .
    docker run -p 8080:8080 -v manta-data:/data manta-server
    # or: docker compose up -d

The image is a static binary on a distroless base. At build time it fetches
the community minimap images ReDota uses (build with `--build-arg MINIMAPS=0`
to skip them). Without Docker: `go run ./cmd/manta-server` serves on
http://127.0.0.1:8080 with data under `./data`.

| Flag | Default | Meaning |
|------|---------|---------|
| `-addr` | `127.0.0.1:8080` | address to listen on (the image uses `0.0.0.0:8080`) |
| `-data` | `data` | processed matches, downloaded replays and the OpenDota cache |
| `-assets` | | comma-separated directories holding `minimap/<version>.webp` |
| `-opendota` | `true` | fetch reference data for each match |
| `-timings` | `true` | with `-opendota`, fetch item timing scenarios too |
| `-keep-replays` | `false` | keep the downloaded `.dem.bz2` files after parsing |
| `-max-jobs` | `2` | matches processed concurrently |
| `-max-replay-mb` | `600` | largest replay download accepted |
| `-request-wait` | `5m` | how long to wait for OpenDota to locate an unseen match |
| `-interval` | `30` | ticks between samples |

Valve keeps replays for a few weeks after a match; older matches fail with a
clear message. The service has no authentication or rate limiting of its own,
so put it behind whatever your deployment normally uses.

[OpenDota]: https://docs.opendota.com
[ReDota]: https://github.com/timkurvers/redota

## Developing

You can run `make update` to re-generate protobufs and callbacks based on the [SteamDatabase/Protobufs](https://github.com/SteamDatabase/Protobufs) project.

## License

Manta is distributed under the [MIT license](https://github.com/dotabuff/manta/blob/master/LICENSE).

## Code of Conduct

Manta has adopted the [Contributor Covenant Code of Conduct](https://github.com/dotabuff/manta/blob/master/CONDUCT.md).

## Getting Help

The best place to ask questions about Dota 2 replay parsing is the #dota2replay channel on QuakeNet, where we're happy to answer any questions you may have. Please only open Github issues for actual bugs in manta, not questions about usage.

Looking to parse Source 1 (original Dota 2) replays? Take a look at [Yasha](https://github.com/dotabuff/yasha).

## Authors and Acknowledgements

Manta is maintained and development is sponsored by [Dotabuff](http://www.dotabuff.com), a leading Dota 2 community website with an emphasis on statistics. Manta wouldn't exist without the efforts of a number of people:

- [Jason Coene](https://github.com/jcoene) is the long-time primary maintainer of the project
- [Michael Fellinger](https://github.com/manveru) built Dotabuff's Source 1 parser [yasha](https://github.com/dotabuff/yasha).
- [Robin Dietrich](https://github.com/invokr) built the C++ parser [Alice](https://github.com/AliceStats/Alice).
- [Martin Schrodt](https://github.com/spheenik) built the Java parser [clarity](https://github.com/skadistats/clarity).
- [Drew Schleck](https://github.com/dschleck), built an original C++ parser [edith](https://github.com/dschleck/edith).
