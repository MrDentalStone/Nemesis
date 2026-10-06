# Getting started (new to AI tools)

This guide takes you from nothing to your first game against Nemesis. If you already
use an AI app that supports MCP, the [README](../README.md) is all you need.

## What you are setting up

Nemesis is not an AI by itself. It is a bridge between two programs you run:

- **Tabletop Simulator** (TTS), with a Magic: The Gathering Commander table loaded.
- **An AI app**, the chat program you type to: Claude Desktop, Claude Code, Codex and
  others. The AI in that app is the player. Nemesis gives it hands at the table: it
  can see its own cards, play them, talk in table chat and react to yours.

The bridge works through **MCP** (Model Context Protocol), a standard way for AI apps
to use outside tools. You do not need to know how it works; the installer sets it up.

## 1. Tabletop Simulator and a table

1. Install Tabletop Simulator from Steam.
2. Subscribe to one of the [supported tables](TABLES.md) on the Steam Workshop.
3. Start TTS once, create a game and load the table from your Workshop items, so you
   know it works. Close TTS again for now.

## 2. An AI app

Pick one. If you are not sure, take **Claude Desktop**.

| App | What it costs | Good to know |
|---|---|---|
| **Claude Desktop** | Free plan works for a taste; Claude Pro for full games | Easiest start. On the Free plan a long game can hit the usage limit. |
| **Claude Code** | Claude Pro or Max | The smoothest games. Runs in a terminal window. |
| Codex, Gemini CLI, local models | Varies | See [CLIENTS.md](CLIENTS.md). |

For Claude Desktop: download it from [claude.ai/download](https://claude.ai/download),
install it, and sign in (or create a free account). Send it one message to check it
works, then quit it from the tray icon (right-click > Quit).

## 3. Install Nemesis

**Windows:**

1. Download `NemesisSetup-<version>.exe` from the
   [Releases page](https://github.com/MrDentalStone/Nemesis/releases).
2. Run it. Windows may warn that the app is unrecognised, because the installer is
   not code-signed yet: click **More info**, then **Run anyway**.
3. A setup page opens in your browser. Tick your AI app (it is usually ticked for
   you), keep the name **Kestrel** or choose another, and press **Install**.
4. When it says done, start your AI app again (Claude Desktop needs a full restart to
   see Nemesis).

**macOS:** download the `.mcpb` file for your Mac from the Releases page (Apple
Silicon: `darwin-arm64`, Intel: `darwin-amd64`) and double-click it with Claude
Desktop open. Then install the pad by hand: README, step 2.

To check it worked in Claude Desktop: open a new chat and look in the tools /
connectors menu of the message box. It should list **nemesis**.

## 4. Your first game

1. Start TTS and load the table. Sit down in a colour.
2. In your AI app, open a new chat and type: **let's play**.
3. The AI looks at the table. If there is no Nemesis pad yet, a small tile appears in
   the middle of the table asking to place one: press **Yes** (or type "yes" in TTS
   chat).
4. On the pad, use `<` `>` to pick a free colour for the AI, and choose a play mode.
   **Slow Grow** is a friendly start; **Teaching** explains its plays as it goes.
5. The AI picks a deck, deals itself a hand and tells you in chat when it is ready.

Then play as you would with a person:

- **Talk to it in TTS chat.** It answers there, by name if you use it.
- **Take your turns normally.** Pass the turn with TTS's End Turn (or the 0 key);
  the AI notices when the turn reaches it, plays, and hands it back.
- **Attack it by tapping your creatures** on your turn. It checks the attack, says
  its blocks and takes the damage.
- **Respond to its spells by dropping a card on its side of the table** (a
  counterspell, removal), or by saying "wait" in chat. It reads your card and reacts.
- **You keep your own life total.** The AI says the damage and you move your tracker.
- The pad's **STOP** button freezes the AI; **Undo** asks it to take back its last
  action.

Keep the AI app open during the game: it is the player. In Claude Desktop, if it
stops and asks whether to continue, say "continue".

## When something goes wrong

| What you see | What to do |
|---|---|
| The AI says TTS is "not reachable" | TTS is not running or no table is loaded. Load the table and say "try again". |
| No **nemesis** in the tools menu | Restart the AI app fully (Claude Desktop: tray icon > Quit, then start it). Run the installer again if it still is missing. |
| "address already in use" or port 39998 | Another program holds Nemesis's port: a TTS script editor plugin (Atom, VS Code), or a second AI app with Nemesis. Close it. |
| The AI went quiet | Your app may have hit its usage limit or a timeout. Say "continue" or start a new chat and say "we're mid-game, check the table". Nothing at the table is lost. |
| It did something wrong | Tell it in chat, plainly. It accepts corrections, and the humans are the judges. |

More detail on each AI app, including free options and their limits:
[CLIENTS.md](CLIENTS.md).
