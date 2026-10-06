# Writing modes and decks

Play modes and decks are Markdown files. The server reads them at start-up; the
modes appear on the pad's mode picker, and the decks in `list_decks`.

## Where files go

Files load in layers. A later layer replaces a file with the same name from an
earlier one, so you can override a shipped mode or deck as well as add new ones.

1. Built into the binary: this repo's `modes/` and `decks/`.
2. Your content folder: `%APPDATA%\nemesis\` on Windows, `~/.config/nemesis/` on
   Linux, `~/Library/Application Support/nemesis/` on macOS. Make `modes/` and
   `decks/` inside it.
3. The folder above the executable (a repo checkout with `bin/`), then the working
   directory, if they have `modes/` or `decks/`.
4. Anything passed with `-content` or `NEMESIS_CONTENT` (several folders separated
   by `;` on Windows, `:` elsewhere).

Restart your AI client after adding files. `list_modes` and `list_decks` print any
file that failed to load and why.

## A play mode: `modes/<id>.md`

```markdown
---
name: Commander Night
blurb: Precons only, no tutors, everyone gets one mulligan for free
order: 6
decks: [krenko-goblins, edgar-vampires]
---

How the AI should play in this mode, in plain Markdown. Cover:

- **Deck:** what kind of deck to bring (bracket, win condition timing).
- **Targets:** who to attack.
- **Knockouts:** when eliminating a player is fine.
- **Removal:** what deserves removal.
```

| Field | Meaning |
|---|---|
| `name` | Shown on the pad. Required. |
| `blurb` | One line, broadcast when someone switches to this mode. |
| `order` | Position on the pad's picker (low first). |
| `default` | `true` on exactly one mode: the mode a fresh pad starts in. |
| `decks` | Deck ids this mode brings along, on top of decks that list the mode themselves. |
| `id` | Optional; the file name is the id (`commander_night.md` is `commander_night`). |

`modes/_shared.md` is appended to every mode's rules: the new-player rules and how to
pick a deck live there. The text says "Kestrel" for the AI; if you run with another
`-name`, the server swaps the name in.

## A deck: `decks/<id>.md`

````markdown
---
name: Krenko Goblins
commander: Krenko, Mob Boss
colors: R
bracket: 2
modes: [slow_grow]
summary: Goblin tokens every turn, finished with a late overrun.
---

## How to pilot

- Keep 7 with 3 or more lands and a 2-drop.
- ...

## Decklist

```
Commander
1 Krenko, Mob Boss
Deck
1 Sol Ring
...
33 Mountain
```
````

| Field | Meaning |
|---|---|
| `name`, `commander` | Required. |
| `colors` | WUBRG letters, for the listings. |
| `bracket` | Commander Bracket 1 to 5, or a range like `2-3`. |
| `modes` | Mode ids this deck suits. A mode's `decks:` line can add it to more. |
| `summary` | One line for `list_decks`. |

The list is the first fenced code block in the file: a `Commander` header line, the
commander(s), a `Deck` header, then the 99. `1 Name` per line, basics as
`30 Forest`. Set pins like `1 Sol Ring (C21) 263` work. The pilot notes above the
list are what the AI reads with `deck_info`: mulligans, the game plan, key lines, and
what to hold back in each mode.

## Check a deck

```
go run ./cmd/deckcheck decks/my-deck.md            # Scryfall checks
go run ./cmd/deckcheck -importer decks/my-deck.md  # and build it on the importer
```

deckcheck needs a checkout and Go. It fails on anything that would trip you up at
the table: not 100 cards, duplicates, an unknown or banned card, a card outside the
commander's colour identity, more Game Changers than the bracket allows, an unknown
mode, or a list the importer cannot build. It does not catch combos, mass land
destruction or extra turns; check those against Wizards' Commander Brackets rules
yourself.
