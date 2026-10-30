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
| `internal/mcpserver` | The tools. `server.go` reads and talks, `actions.go` acts, `content.go` serves the playbook, modes and decks, `events.go` decodes and renders events and works out the response window, `setup.go` places or updates the pad, `draft.go` runs drafts. |
| `internal/padobj` | The pad as a TTS object, built from the embedded `mod/Nemesis.lua`. |
| `internal/setup` | What the installer does: client config edits, install manifest, uninstall. |
| `internal/content` | Loads `modes/*.md` and `decks/*.md` in layers. |
| `internal/app` | Shared start-up: flags, TTS listener, content layers, the MCP server. |
| `internal/importer` | Client for the deck importer server (rikrassen's protocol). |
| `internal/scryfall` | A small, rate-limited Scryfall client (tokens, deckcheck). |
| `internal/cards` | Parses TTS card nicknames and descriptions. |
| `mod/Nemesis.lua` | The pad: seat, buttons, fairness filter, event relay, table adaptation, layout, import. |
| `nemesis.go` | Embeds PLAYBOOK.md, RULES.md, DRAFT.md, modes/, decks/ and the pad's Lua into the binaries. |

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

Interrupts (`interrupt.go`): the humans have no stack, so a spell is already on
their mat when the model reads about it. `ttsapi.Client.OnEvent` hands every live
`ev=play` to the server as it arrives, before the model polls. The server works out
the response window there (NextEvents reuses it), and if an affordable answer only
answers a spell (counter or copy) and the spell clears `needsWindow`, it asks the
pad to `hold`: "Hold on", and the card lights up. The pad lifts the hold on
`release` (the `pass` tool, or quietly when `cast(respond_to=...)` announces the
answer) or after the round wait, with an `ev=hold` event. When
`cast(counters=true)` resolves, `countered` puts **Countered / Not countered**
buttons on the human's card. Only its owner or a host can press them, and the
owner dragging the card to their graveyard counts as Countered. The pad moves
the card on Countered and emits `ev=countered`. A countered spell already in the
graveyard gets a chat line asking its owner to undo it.

`ask_table` rounds settle on chat: the first word decides ("no", "go", "ok" pass;
"wait", "hold", "respond" hold the round), and silence for the pad's wait (the
**Wait** button, default 15 s) counts as no responses. The round's countdown is a
1 s ticker on the admin pad (`startRoundClock`).

**Decision pads** (v1.3.1, replacing the old optional player pads): a round opened
with `pads=true` spawns a big locked pad in the middle of each asked seat's mat
(`DECISION_PAD_SCRIPT`) with the question, the countdown and Yes / No / Wait.
Presses come back through `decisionPress`; only the seat (or a host) may answer.
Wait toggles a per-seat pause that freezes the ticker and emits `ev=round_pause`,
which `awaitRound` uses to stretch its deadline (cast holds up to 3 minutes, then
replies HELD). A seat's pad goes when it answers, the rest when the round settles
or is replaced; a pad reload clears any left over. `cast` opens such a round for
heavy hitters (`heavy.go`: HEAVY_HITTERS.md by name, plus wipe patterns and
creatures of mana value or power 8+) with a 30 s window. The admin pad's
"Big pads" button turns them off. In a Winston draft the human looking at a pile
gets the same pad with Take / Pass and no clock (`showWinstonPad`, round
`'winston'`, respawned after a pad reload).

**Attack pad**: a small locked pad beside Kestrel's life tracker
(`ATTACK_PAD_SCRIPT`, spawned on load and on seat changes, left where a player
moves it). A press lights it for the current turn (`state.attackCall`, cleared on
every turn change). The tap watch still batches tapped creatures; the batch's
`ev=attack` carries `onMe` (pressed, or Kestrel is the attacker's only opponent)
and `minorEvent` folds the rest into the Meanwhile line. A press after the batch
re-sends it as `late`; a press with no taps within 6 s sends `noTaps`
(vigilance); a second press sends `takenBack`.

**Placement and the table's trackers** (v1.3.3, measured on MTG EDH 4-player (S)
with `table_layout`, a diagnostic tool that lists everything around the AI's seat
in mat terms): once a seat is picked (4 s after the last `<` `>` click, or at once
on a confirmed claim) `placePad` moves an unlocked pad to the strip beside the mat,
level with the top of the library column, facing across the table. The attack pad
sits beside the life tracker and locks itself. `pollMana` keeps the table's mana
trackers (`White Mana` ... `Colorless Mana`, the nearest of each to the AI's mat)
showing what the AI's untapped permanents could add (`{T}: Add ...` clauses: a
choice counts once for each colour, "any color" for all five), every 3 s, written
through `val` + `updateVal` only on change.

## Drafting (v1.3.0 to v1.3.9; Winston FDN played live 2026-10-20)

`start_draft` runs a draft for any number of drafters: Winston for two, pick-two
for three or four, booster draft for five or more (`format=` overrides). The split
keeps the fairness rule:

- **Go** (`draft.go`) checks the set code, or reads a cube (CubeCobra id or link,
  any plain-text URL, or the list inline). For a set it also looks the set up on
  Scryfall (`set:X is:booster -t:basic`, paged), only to remember each card's mana
  cost and rarity for rendering, since the importer's nicknames carry no cost; a
  Scryfall miss is not an error.
- **The pad** gets a set's boosters from the importer's own `POST /draft`
  (`{set, hands:[{position, forward, right}], useStates}`: three play boosters per
  hand with the set's real collation, one deck per pack nicknamed
  `Player N - Pack R`) and spawns pack R of drafter N face-down on that drafter's
  mat. Basic lands (the land slot; the table keeps a box of basics) and the
  importer's token cards are destroyed (`stripTokens`/`isNoPick`) from whatever
  deck sits on each spot once it has settled: boosters spawned on one spot merge
  as they land, so the spawned objects are gone by then. The land slot's common
  dual lands stay, as real picks. Winston asks for a third hand's worth (nine
  boosters, about 117 cards once basics are out), shuffles them into one stack
  and cuts it to the standard 90; `wTopTo` also throws away any basic or token
  it deals off the stack, in case one slipped through. A cube's packs are
  decided in the pad (`math.random`), built with one `/build` request per drafter
  and split by name (`splitPacks` keeps only the named cards, so the importer's
  token cards go). Go never learns a
  pack that is not in the AI's hand.
- **Booster and pick-two** pass synchronously: every 1.5 s `pollDraft` counts how
  many of each pack's cards (by guid) have left that seat's hand; once every seat
  has taken its 1 or 2 (two polls in a row for a human), all packs move hand to
  hand at once, packs 1 and 3 in seat order and pack 2 the other way.
- **Winston**: the stack and three piles sit in a row 4 units inside the AI's
  mat from the seam between the two drafters' mats (on the seam, cards slid
  off their spots and the pad lost the stack). A human's pile goes to their
  hand, with a Take / Pass decision pad on their mat; they can also answer
  `take`/`pass` in chat or on the admin pad's buttons. The pad snapshots the
  hand before dealing (`dr.before`) and a pass sends back everything new in it,
  not only the guids the deal recorded: a pile deck's last card once stayed
  behind in the human's hand. Humans keep their picks in hand, so without a
  snapshot only the recorded guids go back. The AI reads its pile through the
  event, never by moving it; after its own pass the next pile comes in the
  `draft_pick` reply only (no duplicate `[draft]` event).
- Mana costs and rarities are a per-session cache; after a server restart
  mid-draft, `draft_view` and incoming `[draft]` events look the missing cards
  up on Scryfall again (`recallCosts`).
- The AI's picks go face-down to its pool (its mat's "other" row). `draft_view`
  lists only its pack, its own Winston pile on its turn, and its pool;
  `table_state` hides the AI's own face-down cards during a draft (unopened packs).
- While `state.draft` is set the pad reports no plays, zone moves or attacks: picks
  dropped on a mat would read as casts. It clears on the first game turn after the
  draft (`start_turn` or a marker move) or `cancel_draft`.
- `draft_build` turns chosen pool guids plus basics (built on the importer) into a
  shuffled library, the rest into a sideboard beside it, sets the AI's life to 20,
  and deals 7.

Seen working live (2026-10-20, 1v1 Winston FDN on MTG EDH 4-player (S)): the
importer's boosters, the 90-card stack, take/pass by chat and pad, the blind
stack card, decision pads (Wait pause/resume, Yes, No) and a pad reload
mid-draft. v1.3.8 (deal-time basic filter) and v1.3.9 (hand snapshot on pass)
were built after the last reload and are not yet seen live. Still to check:
pick-two and booster drafts (pack spots, cards moving hand to hand, `group()`
tidying packs), a cube draft, `draft_build`, and the mana trackers with real
lands on the mat.

## Fixed in v1.4.0

From the 2026-10-30 four-player game (pad v1.3.11), not yet seen live:

- `cast(tap=[...])` failed with "pattern too complex" whenever a tapped land had
  a long rules line without a colon (Frostboil Snarl, Fortified Village).
  `sacrificedForMana` split each line with `'^(.-):'`, which backtracks over the
  whole line in TTS's Lua. It now finds the colon with a plain `string.find`.
  `internal/padobj` has a test that fails on any lazy `.-` pattern in the pad
  (opt out with `pattern-ok` on the line, for short text only).
- `tap` takes `mana=true` to sacrifice Treasures and the like, and without it
  warns when it only tapped one.
- A second server on port 39998 now names the process holding it ("held by
  nemesis-mcp.exe PID 2208: another Nemesis session ...") instead of blaming
  an editor plugin.
- Draws, mulligans and moves to hand deal to hand 1 explicitly, then move any
  newly dealt card that still landed in the secondary hand (cards from `look`
  stay there).
- `move` of a token to the graveyard, exile, hand or library destroys it. A card
  another player put on Kestrel's mat goes to its owner's graveyard or exile,
  with a chat line; to hand or library it asks the owner to take it.
- (S) table update: cards no longer land on the table's locked tiles (mana and
  energy counters in the land box, commander damage and radiation tiles at the
  front row); `rowSlot` skips any slot overlapping one (`matFurniture`), and
  `table_layout` lists the blocked slots. The pad now sits on the mat's far
  edge (`PAD_SPOT`) instead of on the next seat's life tracker, and the attack
  pad on the divider beside it (`ATTACK_PAD_SPOT`); each falls back to its old
  spot when the table has something there.
- Phantom `[zone] your Forest entered land` lines: the (S) table's MTG Lotus Helper
  floats locked preview basics (one per colour the deck plays) in front of every
  fetch land and deletes them when it leaves. The pad now ignores locked cards
  (`isTableProp`) in zone and play events; `table_layout` marks them `tableProp`.
- `spawn_token` put creature tokens in the 'other' row: the server sends `row = ''`
  when it is left out, and `''` is truthy in Lua. `optArg()` treats it as not given.
- Docs: Claude Code's `wait_for_event` timeout is 100 s (it backgrounds calls
  past 120 s); the checkout's `.mcp.json` path is relative, so start Claude
  Code in the repo or register the full path; `mode_rules` with no mode reads
  the table live and can be ahead of queued `[mode]` events.

## Changed in v1.2.3

From the 2026-10-13 four-player Group Hug game, not yet seen live:

- `wait_for_event` replayed the pad's whole event ring (200+ events) after every
  attack, and each attack arrived three or four times. A pad script replaced in
  place left its 1.5 s tap-watch timer running in the old instance, which kept
  emitting `ev=attack` with the old epoch; the server followed that epoch, so
  the next drain saw a mismatch and replayed everything. Each load now claims a
  strictly newer epoch in Global (`nemesisPadEpoch`) and a stale instance stops
  its timer and emits nothing. The server drops pushed events from an older
  epoch, and a drain sets the cursor from the live pad's answer.
- `end_turn` failed whenever a table script errored in `onPlayerTurn` (darbs MTG
  Helpers and the Turn Skipper do once a player is out), although the turn had
  passed. Lua errors carrying another object's GUID no longer fail our calls;
  they still reach `wait_for_event` as `[log]` lines.
- A spell a player drops on another player's mat (a Cyclonic Rift on its
  target) was reported as the mat owner's play. The pad notes who held it or
  whose hand it left: `[play] White (...): Cyclonic Rift ..., dropped on Blue's
  board: White cast it`, and Countered buttons go to the caster.
- `search_library("forest")` took Krosan Verge: with nicknames that carry the
  type line, the description is only rules text and is no longer searched
  (`desc=true` still does). Multi-word queries match each word, so
  `"basic land forest"` finds a basic Forest.
- `spawn_token` takes `power`/`toughness` (Phelddagrif's 1/1 Hippo is a 3/3 on
  Scryfall) and `note`, and marks the card once it has landed; an immediate
  `mark` on a fresh token failed with "not one of my objects".
- `cast` counts commander casts from the command zone and says the tax
  ({2} per earlier cast) in its reply; a new `import_deck` starts the count.

## Changed in v1.2.2

From the 2026-10-09 1v1 game (Miirym vs Trostani), not yet seen live:

- `spawn_token("Treasure")` made a 3/3 Dinosaur: a plain name is now searched as
  that exact name (`!"Treasure" -is:dfc`), and non-creature tokens land in the
  other row.
- `attack` waits 45 s for blocks (was 20 s); humans were still working them out.
- `search_library(basic=true)` takes only basic lands; a Forest search for
  Terramorphic Expanse had taken Breeding Pool.
- `cast(tap=[...])` sacrifices a source whose mana ability sacrifices it
  (Treasure, Gold, Lotus Petal): a token is removed, a card goes to the
  graveyard, and a counted token card drops by one. Before, a Treasure was only
  tapped and stayed on the table.

## Changed in v1.2.0 and v1.2.1

Smoke-tested at the table on 2026-10-09 (one human, pad v1.2.0): hold, counter
with Countered button, `pass`, hold expiry, no hold when the counter is not
affordable or the card is face-down, import/deal polling, `draw`, `say=`, event
folding. Found and fixed in the same session (v1.2.1, not yet seen live):

- The live-event hook never fired: it matched the bytes `"ev":"play"`, but TTS
  re-encodes the pad's messages. Events are always decoded now (the `"ev":"pad"`
  reconfigure check had the same flaw).
- Negate held the table for a creature spell: `canTarget` matches a counter's
  restriction ("noncreature spell", "instant or sorcery spell") to the spell.
- A countered card its owner already took back to hand: Kestrel asks for the
  graveyard instead of failing.
- Nemesis's own pads and the set-up tile no longer show up as `[zone]` events; the
  table's chatter `[log]` lines (land counter, importer progress) fold into the
  Meanwhile line, which is capped at 6 items.

Still open: a second card merging into Kestrel's graveyard pile reports
`[zone] your X left graveyard`.

The v1.2.0 changes:

- Interrupts: the hold, `cast(respond_to, counters)`, `pass` and the Countered
  buttons (see above). The button size and placement on a card are guesses until
  someone sees them live.
- Speed: a light pad `window` op (your mat and hand, no Encoder calls) replaces
  Snapshot plus Hand for response windows, and a play's window is worked out
  once, as it arrives. `cast` takes the card text from `play`'s reply. Waiting
  tools (`Await`) drain the pad once and skip the 300 ms settle. Draws poll the
  hand instead of sleeping 1.8 to 2.5 s. `begin_turn` overlaps the untap animation
  with the draw. The pad keeps `Global.getTable('data')` for a second.
- Fewer calls: `say=` on `play`, `tap`, `untap`, `flip` and `move`;
  `move(guids=[...])`; `spawn_token(count=N)` (one card, counter and note); token
  lookups cached for the session.
- Fewer tokens: `wait_for_event` folds land drops and other players' turn changes
  into one "Meanwhile" line with the next real event. `draw` returns the new cards
  plus the rest of the hand by name. `[play]` text you cannot answer is capped at
  300 characters. Shorter tool descriptions.
- Quieter chat (TTS keeps no chat history): the pad explains how to answer a
  window or round only for the first `HINT_TIMES` (3) of each kind, then shows
  just the seconds. Hold, countered and "No responses" lines are shorter. `say`
  flags lines over 100 characters back to the model, and PLAYBOOK has a chat
  budget (no narrating untaps, draws, phases or reasoning).

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
