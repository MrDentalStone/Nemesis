package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis"
	"github.com/MrDentalStone/Nemesis/internal/content"
	"github.com/MrDentalStone/Nemesis/internal/mcpserver"
)

// The content tools work without a table, over a real MCP session.
func TestContentTools(t *testing.T) {
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "nemesis", Version: "test"}, nil)
	mcpserver.Register(srv, mcpserver.Deps{Content: content.Load(nemesis.Files), Docs: nemesis.Files, Name: "Nyx"})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		if res.IsError {
			t.Fatalf("%s: tool error: %s", name, b.String())
		}
		return b.String()
	}

	if out := call("playbook", nil); !strings.Contains(out, "Hard rules") || !strings.Contains(out, "**Nyx**") {
		t.Errorf("playbook not personalised: %.200s", out)
	}
	if out := call("mode_rules", map[string]any{"mode": "Group Hug"}); !strings.Contains(out, "New-player flags") || !strings.Contains(out, "kynaios-group-hug") {
		t.Errorf("mode_rules missing shared rules or decks: %s", out)
	}
	if out := call("list_decks", map[string]any{"mode": "teaching"}); !strings.Contains(out, "krenko-goblins") {
		t.Errorf("teaching decks: %s", out)
	}
	if out := call("list_decks", map[string]any{"mode": "all"}); strings.Count(out, "\n- ") < 29 {
		t.Errorf("all decks: %s", out)
	}
	if out := call("deck_info", map[string]any{"deck": "ghalta-stompy"}); !strings.Contains(out, "How to pilot") || !strings.Contains(out, "Ghalta, Primal Hunger") {
		t.Errorf("deck_info: %.300s", out)
	}
}
