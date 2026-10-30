# AI clients

Nemesis is a local **stdio** MCP server, so any MCP client that can launch a local
program works. A game is long: the AI loops on `wait_for_event` for one to three
hours and makes hundreds of tool calls, which is where clients differ.

Every client gets the same help from the server: its MCP `instructions` tell the AI
to read the `playbook` tool and `mode_rules` before playing, so clients that load no
instruction file still learn the table rules. In a checkout, Claude Code reads
`CLAUDE.md`, Codex and Copilot read `AGENTS.md`, and Gemini/Antigravity read
`GEMINI.md`; all three point at `PLAYBOOK.md`.

Paths below use `C:\path\to\nemesis-mcp.exe`; use your own (on macOS/Linux, the
binary has no `.exe`).

## Which client?

Checked October 2026. Plans and limits change; treat the free-tier notes as a guide,
not a promise.

| Client | Cost | Verdict |
|---|---|---|
| **Claude Code** | Claude Pro/Max or API credits | **Best.** No short tool timeout, reads CLAUDE.md, runs a whole game in one session. |
| **Claude Desktop** | **Free plan works** | **Good, with caveats.** Tool calls stop after about 60 s (Nemesis waits 45 s by default, so that is fine). Users report a "Continue" prompt about every 20 tool calls, and the Free usage limit (reset every 5 hours, size not published) can end a session mid-game. |
| **Codex CLI / app** | ChatGPT Plus or higher | **Good.** Raise `tool_timeout_sec`. Free ChatGPT accounts get only a limited desktop Codex with unpublished limits. |
| **ChatGPT (web/desktop chat)** | | **Does not work.** Custom MCP connectors need a paid plan and a remote HTTP server; Nemesis is local. |
| **Antigravity CLI** (`agy`) | Free Google tier | **Possible, with caveats.** Weekly agent quota, size not published; a long game may use it up. Untested with Nemesis. |
| **Gemini CLI** | Free AI Studio API key | **Uncertain.** The free Google-login tier ended in June 2026; an API key may still give about 250 Flash requests a day, which is roughly one game. Untested. |
| **VS Code + GitHub Copilot** | Copilot Free | **Not practical.** 50 chat requests a month on Free; a game needs hundreds of tool calls. Paid Copilot works. |
| **Local model** (Ollama with Claude Code, Codex `--oss`, Goose or LM Studio) | Free, your GPU | **Works, plays worse.** Only strong tool-calling models (recent Qwen or Gemma) cope with ~38 tools over a long game, and TTS shares the GPU. |

**For the smoothest game, Claude Code on Pro.** Chat apps poll the table during
every other player's turn, and each poll resends the conversation, which is why
Claude Desktop's Free plan lasts only a few turns. New to AI tools? Start with
[GETTING-STARTED.md](GETTING-STARTED.md).

## Experimental: nemesis-agent

`nemesis-agent` plays without a chat app: it runs the model itself through an API key
(Gemini or any OpenAI-compatible API) and wakes it only when something needs a
decision. It is experimental and lives on the
[`agent-experimental`](https://github.com/MrDentalStone/Nemesis/tree/agent-experimental)
branch, where its own docs explain setup. Main releases ship the MCP server only.

## Claude Code

```
claude mcp add nemesis -- C:\path\to\nemesis-mcp.exe
```

Or, in a checkout, the repo's `.mcp.json` already points at `bin/nemesis-mcp.exe`;
build it and start Claude Code in the folder. The path is relative, so a session
started anywhere else does not find the server: use `claude mcp add` with the full
path for that (`nemesis-setup` does it for you). Check with `claude mcp list`.

If Claude Code says enterprise MCP configuration is active, add the server to the
managed config instead (on Windows `C:\Program Files\ClaudeCode\managed-mcp.json`):

```json
{ "mcpServers": { "nemesis": { "type": "stdio", "command": "C:\\path\\to\\nemesis-mcp.exe", "args": [] } } }
```

Claude Code moves a tool call to the background after 120 s, which stalls the
playbook's `wait_for_event` loop, so use `timeout_seconds=100`.

## Claude Desktop (Free plan)

1. **Install the extension.** Download `nemesis-<version>-windows-amd64.mcpb` (or
   `darwin-arm64` for Apple Silicon Macs, `darwin-amd64` for Intel Macs) from the
   Releases page. Double-click it, or drag it onto Claude Desktop, or use
   **Settings > Extensions > Install extension**. The install dialog asks for the AI's
   name, an optional folder of your own modes and decks, and the deck importer (leave
   the default). No JSON, no Go.

   *Or by hand:* **Settings > Developer > Edit Config** opens
   `claude_desktop_config.json` (`%APPDATA%\Claude\` on Windows,
   `~/Library/Application Support/Claude/` on macOS). Add the server and restart
   Claude Desktop fully (tray icon > Quit):

   ```json
   {
     "mcpServers": {
       "nemesis": { "command": "C:\\path\\to\\nemesis-mcp.exe", "args": [] }
     }
   }
   ```

   The **Add custom connector** dialog under Connectors is for remote servers only;
   it cannot add Nemesis.
2. The tools menu in a new chat should list `nemesis`. If it does not, check
   **Settings > Developer** (it should say running) and the log
   `mcp-server-nemesis.log` in Claude's `logs` folder. The usual cause is another
   program on port 39998: a TTS script-editor plugin, or Claude Code with Nemesis
   attached.
3. Make a **Project** called Nemesis and put this in its instructions:

   > You are Kestrel, a player at my Tabletop Simulator Commander table, using the
   > nemesis tools. At the start call playbook, then playbook(part=rules), then
   > mode_rules. During other players' turns keep calling wait_for_event with the
   > default timeout. Keep chat lines short.

4. Start a chat in the project and say "let's play".

Caveats: the 60 s tool limit cannot be changed, so do not ask for long waits. When
Claude stops to ask whether to continue, say "continue". If you hit the Free usage
limit mid-game, the pad keeps every event, so a new chat after the reset can pick up
with `table_state`.

## Codex (CLI, IDE extension, desktop app)

They share `%USERPROFILE%\.codex\config.toml` (`~/.codex/config.toml`):

```toml
[mcp_servers.nemesis]
command = 'C:\path\to\nemesis-mcp.exe'
args = []
startup_timeout_sec = 20
tool_timeout_sec = 300   # the default 60 s is too short for long waits
```

Or `codex mcp add nemesis -- C:\path\to\nemesis-mcp.exe`, then add the
`tool_timeout_sec` line. Run Codex in a checkout so it reads `AGENTS.md`, or tell it
to call `playbook` first.

## Antigravity CLI and Gemini CLI

Antigravity: `~/.gemini/config/mcp_config.json` (global) or
`.agents/mcp_config.json` (workspace), or `agy mcp add`:

```json
{ "mcpServers": { "nemesis": { "command": "C:\\path\\to\\nemesis-mcp.exe", "args": [] } } }
```

Gemini CLI: `~/.gemini/settings.json` or `.gemini/settings.json` in the project.
`trust: true` skips a confirmation prompt on every tool call, which a game needs:

```json
{ "mcpServers": { "nemesis": { "command": "C:\\path\\to\\nemesis-mcp.exe", "args": [], "timeout": 600000, "trust": true } } }
```

Both read `GEMINI.md` in the working directory.

## VS Code (GitHub Copilot agent mode)

`.vscode/mcp.json` in your workspace:

```json
{ "servers": { "nemesis": { "type": "stdio", "command": "C:\\path\\to\\nemesis-mcp.exe", "args": [] } } }
```

Copilot reads `.github/copilot-instructions.md`, and `AGENTS.md` when
`chat.useAgentsMdFile` is on.

## Local models

Any client above can talk to a local model; the MCP side does not change.

- **Claude Code with Ollama**: Ollama documents an Anthropic-compatible API. Set
  `ANTHROPIC_BASE_URL=http://localhost:11434` and `ANTHROPIC_AUTH_TOKEN=ollama`, then
  run `claude` with a tool-calling model.
- **Goose**: add an extension in Goose's `config.yaml`:

  ```yaml
  extensions:
    nemesis:
      name: nemesis
      type: stdio
      cmd: C:\path\to\nemesis-mcp.exe
      args: []
      enabled: true
      timeout: 300
  ```

  Goose reads `AGENTS.md`.
- **LM Studio**: Program tab > Install > Edit `mcp.json`, same shape as Claude Desktop.

Expect weaker rules play than a frontier model. Pick the largest model your GPU fits
next to TTS, and use the Teaching or Slow Grow mode so mistakes cost less.

## Server options

| Flag | Env | Default | |
|---|---|---|---|
| `-name` | `NEMESIS_NAME` | `Kestrel` | The AI's name in chat and on the pads |
| `-importer` | `NEMESIS_IMPORTER` | `https://importer.rikrassen.com` | Deck importer server (same protocol as rikrassen's) |
| `-content` | `NEMESIS_CONTENT` | | Extra folders with `modes/` and `decks/` |
| `-tts` | | `127.0.0.1:39999` | TTS External Editor API |
| `-listen` | | `127.0.0.1:39998` | Where TTS pushes messages (fixed by TTS) |
| `-dev` | | off | Adds `exec_lua`. Never for real games |
| `-version` | | | Print the version |

Pass flags after the command, for example
`claude mcp add nemesis -- C:\path\to\nemesis-mcp.exe -name Nyx`, or `"args": ["-name", "Nyx"]`
in a JSON config.

Only one program can listen on port 39998: close TTS script-editor plugins, and do
not run two AI clients with Nemesis at once.
