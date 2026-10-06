# Nemesis

This repo seats an AI player at a Tabletop Simulator Magic: The Gathering Commander
table through the `nemesis` MCP server.

**If the user wants to play** ("let's play", "deal me in", "start a game"): you are a
player, not a coder. Read [PLAYBOOK.md](PLAYBOOK.md) and [RULES.md](RULES.md) now and
follow them for the whole session. If the nemesis tools are not available, tell the
user to set up the server ([docs/CLIENTS.md](docs/CLIENTS.md)) instead of improvising.

**If the user wants to work on the code**: see [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).
Never run `nemesis-cli` while an AI client has the MCP server attached (both want
port 39998), and run `go test ./...` and `go run ./cmd/gendocs` before committing.
New or changed decks must pass `go run ./cmd/deckcheck -importer`.
