# Development

## Build and test

```
go build -o bin/ ./cmd/...        # nemesis-mcp, nemesis-cli, deckcheck, build-mod, gendocs, mcpb, nemesis-setup (no payload)
go test ./...
go run ./cmd/build-mod -install   # bake mod/Nemesis.lua into dist/Nemesis.json and TTS's Saved Objects
go run ./cmd/gendocs              # regenerate docs/TOOLS.md and docs/DECKS.md
go run ./cmd/deckcheck -importer  # validate every deck against Scryfall and the importer
go run ./cmd/nemesis-setup/mksetup -version v0.8.0 -out build/NemesisSetup.exe  # the Windows installer
```

The experimental API-key player (`nemesis-agent`) lives on the `agent-experimental`
branch.

The pad's Lua has no test harness; check syntax with any Lua 5.2 parser (for
example `npx luaparse`) and test it live in TTS.

## Layout

| Path | |
|---|---|
| `cmd/nemesis-mcp` | The stdio MCP server: flags, content layers, server instructions. |
| `cmd/nemesis-cli` | Terminal harness: starts the server per call. |
| `cmd/build-mod` | Bakes `mod/Nemesis.lua` into a TTS Saved Object. |
| `cmd/deckcheck` | Validates decks (Scryfall, brackets, importer build). |
| `cmd/gendocs` | Writes `docs/TOOLS.md` and `docs/DECKS.md`. |
| `cmd/mcpb` | Writes the Claude Desktop extension manifest (and builds the server into it). |
| `cmd/nemesis-setup` | The Windows installer: a browser wizard over the embedded programs (`mksetup` fills the payload). |
| `internal/ttsapi` | External Editor API client: `Exec` with `returnID`, push listener, events, log capture, the event cursor. |
| `internal/pad` | Typed calls to the pad's `api()`. |
| `internal/mcpserver` | The tools. `server.go` reads and talks, `actions.go` acts, `content.go` serves the playbook, modes and decks, `events.go` decodes and renders events and works out the response window, `setup.go` places or updates the pad. |
| `internal/padobj` | The pad as a TTS object, built from the embedded `mod/Nemesis.lua`. |
| `internal/setup` | What the installer does: client config edits, install manifest, uninstall. |
| `internal/content` | Loads `modes/*.md` and `decks/*.md` in layers. |
| `internal/app` | Shared start-up: flags, TTS listener, content layers, the MCP server. |
| `internal/importer` | Client for the deck importer server (rikrassen's protocol). |
| `internal/scryfall` | A small, rate-limited Scryfall client (tokens, deckcheck). |
| `internal/cards` | Parses TTS card nicknames and descriptions. |
| `mod/Nemesis.lua` | The pad: seat, buttons, fairness filter, event relay, table adaptation, layout, import. |
| `nemesis.go` | Embeds PLAYBOOK.md, RULES.md, modes/, decks/ and the pad's Lua into the binaries. |

## How a tool call reaches the table

1. The MCP client calls a tool on `nemesis-mcp` over stdio.
2. The server sends Lua to TTS's External Editor API (`127.0.0.1:39999`):
   `pad.call('api', {json='{"op":"draw","n":1}'})`. The request travels as a JSON
   string inside a Lua long bracket, so nothing needs Lua quoting.
3. `api()` in `mod/Nemesis.lua` dispatches to `ops.<op>`, which refuses anything
   outside the AI's own seat or while STOP is on, and returns JSON.
4. TTS sends the return value back to `127.0.0.1:39998`, where the server listens.

Events go the other way: the pad calls `sendExternalMessage` for chat, buttons, turns,
zone changes and other players' plays, and the server queues them for `wait_for_event`
(rendered as one short line each). The pad numbers
every event (epoch and sequence) and keeps the last 300, so a server that was not
listening drains what it missed on start-up and on every `wait_for_event`. The
cursor lives in `%LOCALAPPDATA%\nemesis\event-cursor.json` (the user cache dir
elsewhere).

## Fairness lives in the pad

`mod/Nemesis.lua` is the only code that can see hidden information:

- hands: only the AI's own seat is listed; other seats are counts (main hand,
  secondary hand and token zone counted separately);
- libraries: never listed, only counted; `look` and `search_library` only touch the
  AI's own library;
- face-down cards on other battlefields are reported as face-down;
- `api()` never takes a GUID it did not hand out itself, and nothing in it is
  reachable from chat.

Keep it that way: new features that need hidden information belong in the pad,
behind a check, never in Go.

## Other players' plays and response rounds

The pad watches every seat's battlefield, graveyard and exile zones. A card that
lands there from anywhere but that seat's battlefield (a hand, the command zone, the
library, a graveyard) is reported as `ev=play` once nobody holds it and it is at rest,
so dragging a card over the mat and back to hand is not a play, and moving a
permanent around the mat is not one either. A card is public by then; face-down cards
go out as face-down.

On the Go side `NextEvents` reads your hand and battlefield once per batch of plays
(`ResponseWindow`): instants and flash cards you can afford, non-mana activated
abilities on untapped permanents, and the mana you have up (one per untapped source).
Each play then carries `window` and `canRespond`, and `wait_for_event` prints them.
The window errs towards listing (a card it cannot cost is listed).

A card a human drops on the AI's own mat (seen by `held_by_color` as it enters) is
reported as `to=your_board`, and it answers an open response window. Attacks are
seen by polling: every 1.5 s the pad reads the active human's creatures and reports
the ones that went from untapped to tapped as one `ev=attack` (mana creatures and
creatures that arrive tapped are left out). Permanents are reported once per trip
to the battlefield (`fieldOf`), so tapping or nudging one is not a new play.

The AI's own spells go through `cast`. It holds a quiet window (8 s) only for spells
that could matter to someone else (`needsWindow` in `minor.go`): Game Changers
(Scryfall's `game_changer` flag, cached per name), mana value 4 or more, oracle text
that targets or affects other players, and the commander. Everything else resolves at
once, and a card dropped on the AI's board within 20 s is flagged as a possible answer
to it (`lateFor`).

`ask_table` rounds settle on chat as well as on the optional player pads: the first
word decides ("no", "go", "ok" pass; "wait", "hold", "respond" hold the round), and
silence for the pad's wait (the **Wait** button, default 15 s) counts as no responses.

## Fixed in v1.1.0 (not yet checked at the table)

From the 2026-10-07 four-player game (pad v1.0.0.1):

- `search_library` and `reveal_until` failed with "pattern too complex": TTS's Lua
  gives up on `'^(.-)\n\n'` over long oracle text. `descHeader()` cuts the
  description with a plain `string.find` instead.
- A turn the table took back (the pad passed the marker to Kestrel during set-up,
  then the humans moved it on) left `claudeTurn` set, so every later pass over
  Kestrel's seat was ignored. A marker move during Kestrel's turn now ends it
  (`[turn] ... the table moved the marker off you`), and the first turn after
  `reset_board` is flagged so Kestrel checks who goes first.
- `begin_turn` drew before untap-step and upkeep effects (Mesmeric Orb, Maralen of
  the Mornsong). It now lists such cards (`beforeDraw` in `minor.go`) and holds
  the draw for `draw()`. It also waits for the untap animation, so the table it
  returns no longer shows lands still tapped.
- `[play]` lines cut oracle text at 220 characters; now 700.
  `parse_card(guid=...)` reads any face-up public card in full.
- `say(to=<colour>)` whispers to one seat, for secret numbers and hidden votes.
- `table_state` marks a seat as OUT OF THE GAME when the table's Turn Skipper puck
  sits on its mat (the tables' own way to skip a dead or scooped player) or its life
  is 0 or less (`seatOut` in `minor.go`), and PLAYBOOK/RULES say that a player who
  leaves takes everything they own with them (CR 800.4a).

## Fixed in v1.0.0

The five issues from the 2026-10-06 live game (pad v0.9.1), each checked at the
table before release:

- Transit noise: zone events wait until the card settles and report only the net
  move (`[zone] your Rat moved from playmat to graveyard`). Cards dealt off the
  library are stamped as Kestrel's own and stay quiet. A card that merges into a
  pile is reported by the last zone it entered, because TTS reports it leaving
  that zone as it is destroyed.
- `move(to=library_top|library_bottom)`: the card is lifted clear of any hand or
  pile, held until it has spawned, then put in. The deck is checked afterwards; a
  card on the wrong end is put back once more, and a `[log]` line says so if that
  fails too.
- Missed drops: a card released above Kestrel's mat counts as a drop from the
  player whose hand or zone it last left. A card that still slips through turns
  into a drop when it settles.
- `needsWindow` ignores reminder text in parentheses and wording like "can't be
  the target of" and "becomes the target of".
- End step: the `[turn] YOUR turn` event lists the instants Kestrel could still
  play in the previous player's end step, before `begin_turn` untaps. Spells that
  only answer a spell (Counterspell, Twincast) are left out of that list.

## Battlefield layout

The AI's cards follow the printed mat: lands only inside the land box along the
player's edge (two rows, nine columns on the six-seat mat and fourteen on the 4-seat,
then overlapping), creatures in the two rows nearest the table centre, other
permanents in the row behind them, the stack between that row and the land box. Rows
and columns come from the mat and land zone sizes, so a table with the same kind of
zones needs no new numbers. Slots handed out in the last 3 s count as taken, so quick
plays never land on one spot (two cards dropped on one spot merge into a deck).

## Inspecting a table live

- The CLI with `-dev` adds `exec_lua`:
  `go run ./cmd/nemesis-cli -dev exec_lua '{"script":"return #getAllObjects()"}'`.
  Never while an AI client has the server attached: both want port 39998.
- With a server attached, you can still send an Execute message (`messageID` 3)
  straight to `127.0.0.1:39999` and `print()` the result; it shows up in `tts_log`.
- Hot-swap the pad without respawning it: run Lua that calls
  `setLuaScript(SRC)` then `reload()` on the object with `nemesisPad == true` (wrap
  the file in a long bracket like `[=====[ ... ]=====]`). Run `build-mod -install`
  as well so the Saved Object matches.
- Table saves in the Workshop folder are JSON, or BSON for some uploads; the
  `Global.getTable('data')` layout is what `seatData()` and `adaptSeat()` read.

## The CLI fallback

Without an MCP client, an AI with shell access can still play through
`bin/nemesis-cli <tool> '<json>' [-- <tool> '<json>' ...]` (`@file` reads the JSON
from a file, `-dev` adds `exec_lua`, `-list` prints the tools). Each call starts a
server for its duration, so nothing listens on port 39998 between calls; the event
buffer means nothing is lost.

## Smoke tests

```
go run ./cmd/nemesis-cli -list
go run ./cmd/nemesis-cli table_state
go run ./cmd/nemesis-cli list_decks '{"mode":"all"}'
go run ./cmd/nemesis-cli say '{"text":"hello table"}'
go run ./cmd/nemesis-cli wait_for_event '{"timeout_seconds":30}'
```

## Releasing

Push a tag like `v0.7.0`. The release workflow builds `nemesis-mcp`,
`nemesis-cli` for Windows, macOS and Linux, bakes `Nemesis.json`, and attaches one
zip per platform to the GitHub release, plus a Claude Desktop extension (`.mcpb`) for
Windows and both Mac architectures. `cmd/mcpb` writes the extension's
`manifest.json` from the server's own tool list; Anthropic's `mcpb` CLI validates
and packs it. To build one locally:

```
go run ./cmd/mcpb -os windows -dir build/mcpb -build
npx @anthropic-ai/mcpb pack build/mcpb build/nemesis.mcpb
```

Bump `NEMESIS_VERSION` in `mod/Nemesis.lua` when the pad changes; the server's
version comes from the tag.
