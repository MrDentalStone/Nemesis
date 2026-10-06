// Package nemesis holds the files built into the binaries: the playbook and
// rules guide the AI reads, the shipped play modes and decks, and the pad's
// Lua. Folders given at run time add to or override the modes and decks.
package nemesis

import "embed"

//go:embed PLAYBOOK.md RULES.md all:modes decks mod/Nemesis.lua
var Files embed.FS
