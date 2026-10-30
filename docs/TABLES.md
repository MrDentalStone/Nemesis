# Supported tables

Nemesis works on scripted MTG Commander tables whose Global script keeps a `data`
table of per-seat zones (library, graveyard, playmat, ...). The pad reads that
table, fills in anything a table leaves out (see `adaptSeat` in `mod/Nemesis.lua`),
and lays the AI's cards out along the printed mat.

| Table | Seats | Workshop | Status |
|---|---|---|---|
| MTG EDH 4-player (χ) | 4 | [3745501663](https://steamcommunity.com/sharedfiles/filedetails/?id=3745501663) | Supported, tested live |
| MTG EDH 4-player (S) | 4 | [3805851163](https://steamcommunity.com/sharedfiles/filedetails/?id=3805851163) | Supported from pad v0.6.0; tested live (import, mulligan, turns, rounds, library search) |
| MTG 6 player table - scripted | 6 | [2293586471](https://steamcommunity.com/sharedfiles/filedetails/?id=2293586471) | Supported, tested live |

All three ship [rikrassen's deck importer](https://steamcommunity.com/sharedfiles/filedetails/?id=2265064081),
and Nemesis builds its decks through the same importer server.

## MTG EDH 4-player (χ)

klrmngr's table ([source on GitHub](https://github.com/klrmngr/mtg-edh-4player)).
Global `data` links every zone a seat has: library, graveyard, exile, command zone,
playmat, land zone, life tracker and per-opponent commander damage trackers. The pad
uses them as they are.

- **Commander damage**: `commander_damage` goes through the table's own trackers
  (`commanderDamageDealt`), and the table takes the life off itself.
- **Reset**: `reset_board` calls the table's `resetLifeAndCommanderDamage` and
  `resetMulliganCount` for the AI's seat.
- **On-card counters**: the table's Encoder (right-click menu) is read for every card
  and written for the AI's cards with `mark`.
- **Layout**: two land rows of 14 inside the printed land box, creatures in the two
  rows nearest the centre, other permanents behind them.

## MTG EDH 4-player (S)

A fork of χ with poison, energy and experience counters, a card preview, artwork
changes and a private token zone per seat. Its Global script is χ's, so everything
above applies. A 4-turn game on 2026-10-06 covered deck import,
mulligans, land drops, casting, library search, ask_table rounds and turn handoff. A
second game the same day (pad v0.9.1, Claude Code over MCP) covered the pad's
self-update, tap-to-attack detection, quick casts, Fact or Fiction piles in chat, a
counterspell against a card dropped on the AI's mat, and tokens handed across.
The token-zone and player-counter handling below is written to the table's layout
but not yet exercised in a game. Pad v0.6.0 adds:

- **Token zones.** Each seat has a third hand zone for tokens, marked by a
  `TokenZone` object. The pad tells hand zones apart by what sits on them, not by
  index: the AI's `my_hand` labels cards there `[token zone]`, and `table_state`
  reports other players' token zones as "N in token zone" instead of counting them
  as cards in hand.
- **Player counters.** The poison, energy and experience tiles beside each mat show up
  in `table_state` ("poison 3") whenever they are not zero.
- **MTG Lotus Helper** (2026-10 update): mana and energy counters inside the land
  box, and locked preview basics floating in front of each fetch land. The pad
  keeps cards off the counters and ignores the previews (locked cards are never
  reported as plays).
- **Card preview.** The table's card sniffer is harmless to Nemesis; remove the
  sniffer figure if you do not want the preview.

## MTG 6 player table - scripted

Oops-I-baked-a-pie's six-seat table (the in-game save calls itself
"MTG EDH 6-player (π)"). Global `data` links only library, graveyard and playmat,
so the pad adapts each seat:

- **Life trackers and Commander Zone mats** are found by their description (the seat
  colour), with a position fallback for the seat's side panel.
- **Exile** is the spot the table's own exile key uses (library plus 3.17 forward,
  turned -90); the pad puts an invisible scripting zone there.
- **Command zone and land box** get invisible zones too, named
  `Nemesis <Colour> exile|command|lands`.
- **Commander damage**: the table has no per-victim trackers, so the pad keeps the
  tally (shown under each seat in `table_state`) and takes the damage off the
  victim's life tracker. `reset_board` resets the AI's life and tally itself.
- **Layout**: two land rows of nine inside the printed land box.
- **Turns**: keybind 0 ends the turn with `Turns.getNextTurnColor()`, which skips
  empty colours; the pad notices the marker passing its seat and starts the AI's
  turn.

## Other tables

A table probably works if its Global script exposes `data[<colour>]` with at least
`libraryZone`, `graveyard` and `playmat` scripting zones. Try `table_state` with the
pad on the table: if every seat shows a library count and a battlefield, the basics
work. Anything else (life trackers named differently, no exile spot) means an
`adaptSeat` change; see [DEVELOPMENT.md](DEVELOPMENT.md) for how to inspect a table
live, and open an issue with the table's workshop link.

Known limits on every table:

- With only one human seated, TTS sends no turn event when they end their turn; use
  the pad's **Your turn** button.
- The table has no "attach": equipment and auras on the AI's cards are tracked as
  notes and P/T changes with `mark`.
