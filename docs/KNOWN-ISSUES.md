# Known issues

Open failures seen in real games, with evidence and a suspected cause. Fix, then
move the entry to the release notes of the version that fixes it
([DEVELOPMENT.md](DEVELOPMENT.md)).

The 2026-10-30 game's issues 1 to 8 are fixed in v1.4.0; check them at the table.

## Seen at the table, not ours

From the 2026-10-30 four-player game:

- `Turn Skipper - 62e99d: (51,2-60) attempt to index a nil value` in
  `onPlayerTurn` when the puck sat on a seat (the table's object).
- `MTG Lotus Helper v1.4.5: Object reference not set to an instance of an
  object` several times (the table's helper, added in the 2026-10-28 update).
  Reproduced 2026-10-30: it fires when a fetch land (Evolving Wilds) leaves the
  battlefield and the helper removes its preview basics.
- A rejected `ask_table` call in the client still left round #1 open on the
  pad. Clear it with `cancel_round`. Most likely the call reached the server
  before the client showed the rejection.
