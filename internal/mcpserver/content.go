package mcpserver

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis/internal/content"
)

// registerContent adds the tools that read the playbook, the play modes and
// the decks. They work without a table, so an AI can prepare before TTS is up.
func registerContent(s *mcp.Server, d Deps) {
	type playbookIn struct {
		Part string `json:"part,omitempty" jsonschema:"playbook (default: how to play at this table) | rules (the Commander rules guide and its opponent-turn checklists) | draft (drafting and limited games) | all"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "playbook",
		Description: "How to play at this table: hard rules, game flow, your turn, other players' turns, tool habits. " +
			"Read it before the first game if your client did not already load PLAYBOOK.md; part=rules gives the Commander rules guide, part=draft how to draft.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in playbookIn) (*mcp.CallToolResult, any, error) {
		var parts []string
		switch strings.ToLower(in.Part) {
		case "", "playbook":
			parts = []string{"PLAYBOOK.md"}
		case "rules":
			parts = []string{"RULES.md"}
		case "draft":
			parts = []string{"DRAFT.md"}
		case "all":
			parts = []string{"PLAYBOOK.md", "RULES.md"}
		default:
			return nil, nil, fmt.Errorf("part must be playbook, rules, draft or all")
		}
		var b strings.Builder
		for _, p := range parts {
			raw, err := fs.ReadFile(d.Docs, p)
			if err != nil {
				return nil, nil, err
			}
			b.WriteString(personalize(string(raw), d.Name))
			b.WriteString("\n\n")
		}
		return text(b.String()), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_modes",
		Description: "The play modes this table offers (modes/*.md), with the current one marked. The admin pad cycles through them; mode_rules gives one mode's rules.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		cur := currentMode(ctx, d)
		var b strings.Builder
		for _, m := range d.Content.SortedModes() {
			mark := "  "
			if m.ID == cur {
				mark = "* "
			}
			fmt.Fprintf(&b, "%s%s (%s): %s. %d decks\n", mark, m.Name, m.ID, m.Blurb, len(d.Content.DecksFor(m.ID)))
		}
		if cur == "" {
			b.WriteString("(no pad reachable, current mode unknown)\n")
		}
		return text(b.String() + contentErrors(d.Content)), nil, nil
	})

	type modeIn struct {
		Mode string `json:"mode,omitempty" jsonschema:"mode id or name (default: the mode set on the pad)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mode_rules",
		Description: "The rules for a play mode: who to target, when knockouts are allowed, what to remove, what deck to bring, plus the new-player rules that apply in every mode. Read it before choosing a deck and whenever ev=mode reports a change. With no mode it reads the table live, so it can be ahead of [mode] events still queued when the mode changes fast.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in modeIn) (*mcp.CallToolResult, any, error) {
		id := in.Mode
		if id == "" {
			id = currentMode(ctx, d)
		}
		m := findMode(d.Content, id)
		if m == nil {
			return nil, nil, fmt.Errorf("no mode %q; list_modes shows the ids", id)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# %s (%s)\n\n%s\n", m.Name, m.ID, personalize(m.Body, d.Name))
		if d.Content.Shared != "" {
			b.WriteString("\n" + personalize(d.Content.Shared, d.Name) + "\n")
		}
		decks := d.Content.DecksFor(m.ID)
		if len(decks) > 0 {
			b.WriteString("\n## Decks for this mode\n\n")
			for _, dk := range decks {
				fmt.Fprintf(&b, "- %s (`%s`): %s, bracket %s. %s\n", dk.Name, dk.ID, dk.Commander, dk.Bracket, dk.Summary)
			}
		}
		return text(b.String()), nil, nil
	})

	type listIn struct {
		Mode string `json:"mode,omitempty" jsonschema:"only decks for this mode id; 'all' for every deck (default: the mode set on the pad)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_decks",
		Description: "Tested 100-card decks (decks/*.md) with commander, colours, bracket and a one-line summary. Import one with import_deck(deck=<id>).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, any, error) {
		mode := in.Mode
		if mode == "" {
			mode = currentMode(ctx, d)
		}
		if strings.EqualFold(mode, "all") {
			mode = ""
		} else if m := findMode(d.Content, mode); m != nil {
			mode = m.ID
		}
		decks := d.Content.DecksFor(mode)
		var b strings.Builder
		if mode != "" {
			fmt.Fprintf(&b, "Decks for %s (list_decks mode=all for every deck):\n", mode)
		}
		for _, dk := range decks {
			fmt.Fprintf(&b, "- %s: %s (%s), %s, bracket %s, modes %s. %s\n", dk.ID, dk.Name, dk.Commander,
				or(dk.Colors, "?"), dk.Bracket, strings.Join(d.Content.ModesOf(dk), ", "), dk.Summary)
		}
		if len(decks) == 0 {
			b.WriteString("no decks for this mode; try mode=all\n")
		}
		return text(b.String() + contentErrors(d.Content)), nil, nil
	})

	type deckIn struct {
		Deck string `json:"deck" jsonschema:"deck id from list_decks"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "deck_info",
		Description: "A deck's pilot notes (mulligans, game plan, key synergies, what to hold back per mode) and its full list. Read it after importing so you know how to play the deck.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deckIn) (*mcp.CallToolResult, any, error) {
		dk := d.Content.Decks[in.Deck]
		if dk == nil {
			return nil, nil, fmt.Errorf("no deck %q; list_decks shows the ids", in.Deck)
		}
		return text(fmt.Sprintf("# %s\nCommander: %s. Colours %s, bracket %s, modes %s.\n%s\n\n%s\n",
			dk.Name, dk.Commander, or(dk.Colors, "?"), dk.Bracket, strings.Join(d.Content.ModesOf(dk), ", "),
			dk.Summary, personalize(dk.Body, d.Name))), nil, nil
	})
}

func findMode(l *content.Library, id string) *content.Mode {
	if m := l.Modes[id]; m != nil {
		return m
	}
	for _, m := range l.Modes {
		if strings.EqualFold(m.Name, id) || strings.EqualFold(m.ID, id) {
			return m
		}
	}
	return nil
}

// currentMode asks the pad; "" when no pad is reachable.
func currentMode(ctx context.Context, d Deps) string {
	if d.Pad == nil {
		return ""
	}
	if err := configure(ctx, d); err != nil {
		return ""
	}
	var out struct {
		Mode string `json:"mode"`
	}
	if err := d.Pad.Call(ctx, "configure", nil, &out); err != nil {
		return ""
	}
	return out.Mode
}

func contentErrors(l *content.Library) string {
	if len(l.Errors) == 0 {
		return ""
	}
	return "\nFiles that did not load:\n- " + strings.Join(l.Errors, "\n- ") + "\n"
}

// personalize puts the configured table name into text written for "Kestrel".
func personalize(s, name string) string {
	if name == "" || name == "Kestrel" {
		return s
	}
	return strings.ReplaceAll(s, "Kestrel", name)
}

// configure sends the AI's name and the mode list to the pad, once per pad
// instance (a reloaded pad gets it again).
var configured struct {
	sync.Mutex
	guid string
}

func configure(ctx context.Context, d Deps) error {
	guid, err := d.Pad.GUID(ctx)
	if err != nil {
		return err
	}
	configured.Lock()
	defer configured.Unlock()
	if configured.guid == guid {
		return nil
	}
	var modes []map[string]string
	def := ""
	for _, m := range d.Content.SortedModes() {
		modes = append(modes, map[string]string{"id": m.ID, "name": m.Name, "blurb": m.Blurb})
		if m.Default && def == "" {
			def = m.ID
		}
	}
	args := map[string]any{"name": d.Name}
	if len(modes) > 0 {
		args["modes"] = modes
		if def != "" {
			args["defaultMode"] = def
		}
	}
	if err := d.Pad.Call(ctx, "configure", args, nil); err != nil {
		return err
	}
	configured.guid = guid
	return nil
}

// reconfigure forgets the pad so the next call sends the config again (the
// pad reloaded: ev=pad what=loaded).
func reconfigure() {
	configured.Lock()
	configured.guid = ""
	configured.Unlock()
}
