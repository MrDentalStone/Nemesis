// Package nemesis holds the files built into the binaries: the playbook,
// rules and drafting guides the AI reads, the heavy-hitter list, the
// shipped play modes and decks, and the pad's Lua. Folders given at run
// time add to or override the modes and decks.
package nemesis

import "embed"

//go:embed PLAYBOOK.md RULES.md DRAFT.md HEAVY_HITTERS.md all:modes decks mod/Nemesis.lua
var Files embed.FS
