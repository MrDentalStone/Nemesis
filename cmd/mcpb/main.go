// mcpb writes the manifest.json for a Claude Desktop extension (.mcpb) that
// bundles nemesis-mcp, with the tool list taken from the server itself.
// Pack the folder with Anthropic's CLI afterwards:
//
//	go run ./cmd/mcpb -version v1.1.0 -os windows -dir build/mcpb-windows
//	npx @anthropic-ai/mcpb pack build/mcpb-windows nemesis.mcpb
//
// The folder must hold server/nemesis-mcp(.exe); -build compiles it there.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis"
	"github.com/MrDentalStone/Nemesis/internal/content"
	"github.com/MrDentalStone/Nemesis/internal/importer"
	"github.com/MrDentalStone/Nemesis/internal/mcpserver"
)

func main() {
	version := flag.String("version", "v1.1.0", "release version (a leading v is dropped)")
	goos := flag.String("os", "windows", "windows or darwin")
	arch := flag.String("arch", "amd64", "GOARCH for -build")
	dir := flag.String("dir", "build/mcpb", "bundle folder")
	build := flag.Bool("build", false, "also compile server/nemesis-mcp into the folder")
	flag.Parse()

	platform := map[string]string{"windows": "win32", "darwin": "darwin"}[*goos]
	if platform == "" {
		log.Fatalf("-os must be windows or darwin (Claude Desktop does not run on %s)", *goos)
	}
	exe := "nemesis-mcp"
	if *goos == "windows" {
		exe += ".exe"
	}
	if err := os.MkdirAll(filepath.Join(*dir, "server"), 0o755); err != nil {
		log.Fatal(err)
	}
	if *build {
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+*version,
			"-o", filepath.Join(*dir, "server", exe), "./cmd/nemesis-mcp")
		cmd.Env = append(os.Environ(), "GOOS="+*goos, "GOARCH="+*arch, "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			log.Fatal(err)
		}
	}

	manifest := map[string]any{
		"manifest_version": "0.3",
		"name":             "nemesis",
		"display_name":     "Nemesis: AI Commander player for Tabletop Simulator",
		"version":          strings.TrimPrefix(*version, "v"),
		"description":      "Seats Claude at your Tabletop Simulator Magic: The Gathering Commander table as a fair opponent that only sees its own hand.",
		"long_description": "Nemesis lets Claude play Commander at a scripted MTG table in Tabletop Simulator. " +
			"Spawn the Nemesis pad on the table (Nemesis.json from the release goes in TTS's Saved Objects), pick a seat for the AI, then say \"let's play\". " +
			"The pad holds the fairness rules: libraries are counted, never listed, and other players' hands are counts. " +
			"Ships with 5 play modes and 32 tested decks. Tabletop Simulator must be running with a table loaded; " +
			"close any TTS script-editor plugin first, since only one program can listen on port 39998.",
		"author":        map[string]string{"name": "MrDentalStone", "url": "https://github.com/MrDentalStone"},
		"repository":    map[string]string{"type": "git", "url": "https://github.com/MrDentalStone/Nemesis"},
		"homepage":      "https://github.com/MrDentalStone/Nemesis",
		"documentation": "https://github.com/MrDentalStone/Nemesis/blob/main/docs/CLIENTS.md#claude-desktop-free-plan",
		"support":       "https://github.com/MrDentalStone/Nemesis/issues",
		"license":       "MIT",
		"keywords":      []string{"magic the gathering", "mtg", "commander", "edh", "tabletop simulator", "game"},
		"server": map[string]any{
			"type":        "binary",
			"entry_point": "server/" + exe,
			"mcp_config": map[string]any{
				"command": "${__dirname}/server/" + exe,
				"args":    []string{},
				"env": map[string]string{
					"NEMESIS_NAME":     "${user_config.name}",
					"NEMESIS_CONTENT":  "${user_config.content_dir}",
					"NEMESIS_IMPORTER": "${user_config.importer}",
				},
			},
		},
		"user_config": map[string]any{
			"name": map[string]any{
				"type": "string", "title": "AI name at the table",
				"description": "What the humans see in chat and on the pads.",
				"default":     "Kestrel", "required": false,
			},
			"content_dir": map[string]any{
				"type": "directory", "title": "Your modes and decks (optional)",
				"description": "A folder with modes/ and decks/ subfolders of .md files that add to or override the built-in ones. See docs/CONTENT.md.",
				"required":    false,
			},
			"importer": map[string]any{
				"type": "string", "title": "Deck importer server",
				"description": "Builds decks and tokens. The default is rikrassen's importer, the one the supported tables ship with.",
				"default":     importer.DefaultBaseURL, "required": false,
			},
		},
		"tools":         tools(),
		"compatibility": map[string]any{"platforms": []string{platform}},
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	out := filepath.Join(*dir, "manifest.json")
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", out)
}

// tools lists what the server registers (minus the dev-only exec_lua).
func tools() []map[string]string {
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "nemesis", Version: "mcpb"}, nil)
	mcpserver.Register(srv, mcpserver.Deps{Content: content.Load(nemesis.Files), Docs: nemesis.Files, Name: "Kestrel"})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		log.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpb"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		log.Fatal(err)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	var out []map[string]string
	for _, t := range res.Tools {
		desc, _, _ := strings.Cut(t.Description, ". ")
		out = append(out, map[string]string{"name": t.Name, "description": strings.TrimSuffix(desc, ".") + "."})
	}
	return out
}
