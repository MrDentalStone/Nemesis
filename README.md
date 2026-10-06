# Nemesis

An AI player for Magic: The Gathering Commander in Tabletop Simulator.

Nemesis seats an AI (Claude, GPT, Gemini or a local model) at your TTS Commander
table as a real opponent. It plays its own deck, talks in table chat, answers your
spells and attacks, lets you answer its own, and checks your attacks against the
rules. It only ever sees its own hand.

It has two parts:

- **`nemesis-mcp`**, an [MCP](https://modelcontextprotocol.io) server (Go, stdio).
  Your AI client talks to it. It drives TTS through the External Editor API on
  `127.0.0.1:39999`.
- **The Nemesis pad**, a TTS object (`mod/Nemesis.lua`). It owns the AI's seat and is
  the only code that touches hidden information, so the fairness rules live in Lua on
  the table, not in the AI's good intentions. Libraries are counted, never listed, and
  other players' hands are counts.

At the table the AI is called **Kestrel**; `-name` changes that.

## What you need

- Tabletop Simulator and one of the [supported tables](docs/TABLES.md)
- An MCP-capable AI client. Claude Code works best; see [docs/CLIENTS.md](docs/CLIENTS.md)
  for Claude Desktop, Codex, Gemini/Antigravity, VS Code and local models, including
  what works on free tiers. New to AI tools? [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md)
  walks you through it from nothing.
- Go 1.27+ only if you build from source

## Install

**Windows: run `NemesisSetup-<version>.exe`** from the Releases page. It opens a
short setup page in your browser: tick your AI client (Claude Desktop, Claude Code,
Codex or Gemini CLI), give the AI a name, and it installs everything for your user (no
admin rights): the server, the pad in TTS's Saved Objects, your client's settings and
Start menu shortcuts. Re-run it to change your choices; uninstall from Windows Settings > Apps.
The installer is not code-signed yet, so Windows SmartScreen asks once: **More info >
Run anyway**. Then skip to [Play](#play).

By hand, or on macOS and Linux:

1. **Get the binaries.** Download the release zip for your OS from the Releases page and
   unzip it anywhere. Or build from source:

   ```
   git clone https://github.com/MrDentalStone/Nemesis
   cd Nemesis
   go build -o bin/ ./cmd/...
   ```

2. **Install the pad.** Copy `Nemesis.json` (from the zip, or `dist/` in the repo) into
   TTS's Saved Objects folder:
   - Windows: `Documents\My Games\Tabletop Simulator\Saves\Saved Objects\`
   - macOS: `~/Library/Tabletop Simulator/Saves/Saved Objects/`
   - Linux: `~/.local/share/Tabletop Simulator/Saves/Saved Objects/`

   From a checkout, `go run ./cmd/build-mod -install` does it for you.

3. **Add the MCP server to your AI client.** For **Claude Desktop**, download the
   `.mcpb` extension for your OS from the Releases page and double-click it; that
   replaces steps 1 and 3. For Claude Code:

   ```
   claude mcp add nemesis -- C:\path\to\nemesis-mcp.exe
   ```

   Other clients: [docs/CLIENTS.md](docs/CLIENTS.md). Close any TTS script editor
   plugin (Atom, VS Code) first: only one program can listen on port 39998.

## Play

1. Load a [supported table](docs/TABLES.md) in TTS, open your AI client and say
   "let's play". If the table has no Nemesis pad yet, a small tile appears in the
   middle asking to place one: press **Yes** (or type "yes"). You can also spawn it
   yourself from **Objects > Saved Objects > Nemesis**.
2. Use `<` `>` on the pad to pick a free colour for the AI (or let it ask with
   `claim_seat`; press **Yes**).
3. Pick a **play mode** on the pad: Slow Grow (default), Group Hug, Meme / Chaos,
   Full Send or Teaching. Flag any **new players** so the AI goes easy on them and
   explains more.
4. The AI picks a deck that suits the mode from the
   [32 shipped decks](docs/DECKS.md), imports it with the table's deck importer, and
   joins the game.
5. Take turns normally (TTS's End Turn, or keybind 0). When the turn marker passes
   the AI's colour the pad starts its turn, and the AI hands it on when done. **Your
   turn** on the pad is a fallback; with only one human seated TTS sends no turn
   event, so use it then.

**Play against it like a person.** When you cast something, the AI sees it land and,
if it holds an instant or an ability it could use, decides whether to respond ("Hold
on, in response..."). When the AI casts a spell, it leaves a few seconds to respond
without asking: **drop your counterspell or removal on its mat** (or say "wait"), and
it reads the card and answers it or accepts it. Cards you drop on its mat at other
times work the same way. **Attack it by tapping your creatures** on your turn; it
checks them, says its blocks and takes the damage. When it does ask the table
something, answer in chat ("no", "go", "wait", "yes"); silence for 15 seconds counts
as no, and the **Wait** button on the pad changes that. Tables that like buttons can
still press **Pads: off** to put small Yes / No / Respond pads at every seat.

Players move their own life trackers. The AI asks once per game whether it may apply
agreed damage to yours. **Undo** asks it to reverse its last action; **STOP** freezes
it until you press it again.

## Modes and decks are Markdown

Play modes live in `modes/*.md` and decks in `decks/*.md`. Each mode says how the AI
chooses targets, knockouts and removal; each deck carries pilot notes and its list.
Drop your own files into `%APPDATA%\nemesis\` (Windows), `~/.config/nemesis/` (Linux)
or `~/Library/Application Support/nemesis/` (macOS) and they appear on the pad and in `list_decks` without rebuilding
anything. See [docs/CONTENT.md](docs/CONTENT.md).

## Documentation

| | |
|---|---|
| [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md) | New to AI tools: from nothing to your first game |
| [docs/CLIENTS.md](docs/CLIENTS.md) | Setting up Claude Code, Claude Desktop, Codex, Gemini/Antigravity, VS Code, local models; free tiers |
| [docs/TABLES.md](docs/TABLES.md) | Supported TTS tables, with workshop links |
| [docs/TOOLS.md](docs/TOOLS.md) | Every MCP tool and its parameters (generated) |
| [docs/DECKS.md](docs/DECKS.md) | The shipped modes and decks (generated) |
| [docs/CONTENT.md](docs/CONTENT.md) | Writing your own modes and decks |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | Building, testing, the pad protocol, project layout |
| [PLAYBOOK.md](PLAYBOOK.md) | How the AI plays at the table (the AI reads this) |
| [RULES.md](RULES.md) | The Commander rules guide the AI checks claims against |

## Credits and legal

- Decks are built by [rikrassen's MTG importer](https://steamcommunity.com/sharedfiles/filedetails/?id=2265064081)
  server, the one the supported tables ship with. Card data comes from the
  [Scryfall API](https://scryfall.com/docs/api).
- The tables are the work of their authors; see [docs/TABLES.md](docs/TABLES.md).
- Nemesis is unofficial Fan Content permitted under the
  [Wizards of the Coast Fan Content Policy](https://company.wizards.com/en/legal/fancontentpolicy).
  Not approved or endorsed by Wizards. Portions of the materials used are property of
  Wizards of the Coast. Magic: The Gathering is a trademark of Wizards of the Coast LLC.
- Built with substantial AI assistance (Claude).

MIT licensed; see [LICENSE](LICENSE).
