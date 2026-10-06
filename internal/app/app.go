// Package app wires a Nemesis server from flags and environment variables:
// the TTS connection, the pad, the importer, Scryfall and the content layers.
// nemesis-mcp serves it over stdio; nemesis-cli runs it per call.
package app

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis"
	"github.com/MrDentalStone/Nemesis/internal/content"
	"github.com/MrDentalStone/Nemesis/internal/importer"
	"github.com/MrDentalStone/Nemesis/internal/mcpserver"
	"github.com/MrDentalStone/Nemesis/internal/pad"
	"github.com/MrDentalStone/Nemesis/internal/scryfall"
	"github.com/MrDentalStone/Nemesis/internal/ttsapi"
)

type Options struct {
	Dev      bool
	TTSAddr  string
	Listen   string
	Name     string
	Importer string
	Content  string
}

// RegisterFlags adds the shared flags to fs (flag.CommandLine for main).
func RegisterFlags(fs *flag.FlagSet) *Options {
	o := &Options{}
	fs.BoolVar(&o.Dev, "dev", false, "expose exec_lua (arbitrary Lua) - never for real games")
	fs.StringVar(&o.TTSAddr, "tts", "127.0.0.1:39999", "TTS External Editor API address")
	fs.StringVar(&o.Listen, "listen", "127.0.0.1:39998", "where TTS pushes messages (fixed by TTS)")
	fs.StringVar(&o.Name, "name", Env("NEMESIS_NAME", "Kestrel"), "the AI's name at the table (env NEMESIS_NAME)")
	fs.StringVar(&o.Importer, "importer", Env("NEMESIS_IMPORTER", importer.DefaultBaseURL), "deck importer server (env NEMESIS_IMPORTER)")
	fs.StringVar(&o.Content, "content", Env("NEMESIS_CONTENT", ""),
		"extra folders with modes/ and decks/, separated by "+string(os.PathListSeparator)+" (env NEMESIS_CONTENT)")
	return o
}

// Instructions reach every MCP client, including ones that read no
// instruction file (Claude Desktop). Keep the first lines self-contained.
const Instructions = `Nemesis seats you as a player at a Tabletop Simulator Magic: The Gathering Commander table, named %s. ` +
	`Before the first game call playbook (how to play here) and mode_rules (the table's play mode), unless your client already loaded PLAYBOOK.md. ` +
	`You only ever see your own hand. Announce with say before acting, never touch other players' objects or life trackers without their yes, ` +
	`and loop on wait_for_event (45 s calls are fine, nothing is lost between them) during other players' turns.`

// Runtime is a running Nemesis server and what it is built from.
type Runtime struct {
	Server  *mcp.Server
	Deps    mcpserver.Deps
	Sources []string
}

func (r *Runtime) Close() { r.Deps.TTS.Close() }

// Summary is the start-up line both binaries print.
func (r *Runtime) Summary(version, mode string) string {
	return fmt.Sprintf("nemesis %s as %q listening for TTS on %s, mode %s, importer %s, %d modes and %d decks from %s",
		version, r.Deps.Name, r.Deps.TTS.ListenAddr(), mode, r.Deps.Importer.BaseURL,
		len(r.Deps.Content.Modes), len(r.Deps.Content.Decks), strings.Join(r.Sources, ", "))
}

// Start listens for TTS and builds the server with every tool registered.
func Start(o *Options, version string, logf func(string, ...any)) (*Runtime, error) {
	lib, sources := LoadContent(o.Content)
	for _, e := range lib.Errors {
		logf("content: %s", e)
	}
	tts, err := ttsapi.Listen(o.TTSAddr, o.Listen)
	if err != nil {
		return nil, err
	}
	tts.SetSpeaker(o.Name)
	deps := mcpserver.Deps{
		TTS:      tts,
		Pad:      pad.New(tts),
		Importer: importer.New(o.Importer, version),
		Scryfall: scryfall.New(version),
		Content:  lib,
		Docs:     nemesis.Files,
		Name:     o.Name,
		Dev:      o.Dev,
	}
	// pick up whatever happened at the table while no server was running
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if n, err := deps.Pad.Drain(ctx); err == nil && n > 0 {
			logf("recovered %d buffered table events", n)
		}
	}()
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "nemesis",
		Title:   "Nemesis - an AI player for Commander in Tabletop Simulator",
		Version: version,
	}, &mcp.ServerOptions{Instructions: fmt.Sprintf(Instructions, o.Name)})
	mcpserver.Register(srv, deps)
	return &Runtime{Server: srv, Deps: deps, Sources: sources}, nil
}

// LoadContent layers the modes and decks: built in, then the user config
// folder (os.UserConfigDir()/nemesis), then the folder above the executable
// and the working directory (a repo checkout), then extra folders. Later
// layers override files with the same id.
func LoadContent(extra string) (*content.Library, []string) {
	layers := []fs.FS{nemesis.Files}
	sources := []string{"built-in"}
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" {
			return
		}
		abs, err := filepath.Abs(dir)
		if err != nil || seen[strings.ToLower(abs)] {
			return
		}
		seen[strings.ToLower(abs)] = true
		if f := content.Dir(abs); f != nil {
			layers = append(layers, f)
			sources = append(sources, abs)
		}
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		add(filepath.Join(cfg, "nemesis"))
	}
	if exe, err := os.Executable(); err == nil {
		add(filepath.Dir(filepath.Dir(exe)))
	}
	if wd, err := os.Getwd(); err == nil {
		add(wd)
	}
	for _, d := range filepath.SplitList(extra) {
		add(d)
	}
	return content.Load(layers...), sources
}

// Env reads a setting; an unfilled "${user_config.x}" placeholder (a Claude
// Desktop extension option the user left empty) counts as unset.
func Env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" && !strings.HasPrefix(v, "${") {
		return v
	}
	return def
}
