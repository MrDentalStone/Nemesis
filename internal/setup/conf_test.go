package setup

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSetServerPreservesOthers(t *testing.T) {
	in := `{
  "globalShortcut": "Ctrl+Space",
  "mcpServers": {
    "filesystem": {"command": "npx", "args": ["-y", "fs"]}
  },
  "zeta": 1
}`
	out, err := SetServer([]byte(in), "mcpServers", "nemesis", map[string]any{"command": `C:\N\nemesis-mcp.exe`, "args": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, `"filesystem"`) || !strings.Contains(s, `"zeta": 1`) || !strings.Contains(s, `"globalShortcut"`) {
		t.Fatalf("lost a key:\n%s", s)
	}
	// order kept: globalShortcut before mcpServers before zeta
	if !(strings.Index(s, "globalShortcut") < strings.Index(s, "mcpServers") && strings.Index(s, "mcpServers") < strings.Index(s, "zeta")) {
		t.Fatalf("order changed:\n%s", s)
	}
	if !strings.Contains(s, `C:\\N\\nemesis-mcp.exe`) {
		t.Fatalf("entry missing:\n%s", s)
	}

	// replace, not duplicate
	out2, err := SetServer(out, "mcpServers", "nemesis", map[string]any{"command": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out2), `"nemesis"`) != 1 || !strings.Contains(string(out2), `"command": "x"`) {
		t.Fatalf("replace failed:\n%s", out2)
	}

	back, changed, err := RemoveServer(out2, "mcpServers", "nemesis")
	if err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	if strings.Contains(string(back), "nemesis") || !strings.Contains(string(back), "filesystem") {
		t.Fatalf("remove result:\n%s", back)
	}
	if _, changed, _ := RemoveServer(back, "mcpServers", "nemesis"); changed {
		t.Fatal("second remove reported a change")
	}
}

func TestSetServerEmptyAndNull(t *testing.T) {
	for _, in := range []string{"", "  ", `{}`, `{"mcpServers": null}`} {
		out, err := SetServer([]byte(in), "mcpServers", "nemesis", map[string]any{"command": "c"})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if !HasServer(out, "mcpServers", "nemesis") {
			t.Fatalf("%q: not set:\n%s", in, out)
		}
	}
	if _, err := SetServer([]byte(`[1]`), "mcpServers", "n", 1); err == nil {
		t.Fatal("array accepted")
	}
}

const codex = `# my codex config
model = "gpt-5"

[mcp_servers.other]
command = "other"

[mcp_servers.nemesis]
command = 'old'
args = []

[mcp_servers.nemesis.env]
X = "1"

[profiles.fast]
model = "mini"
`

func TestTOMLReplace(t *testing.T) {
	out := SetTOMLTable(codex, "mcp_servers.nemesis", []string{"command = 'new'", "args = []"})
	if strings.Contains(out, "'old'") || strings.Contains(out, "nemesis.env") {
		t.Fatalf("old block left:\n%s", out)
	}
	for _, want := range []string{"# my codex config", "[mcp_servers.other]", "command = 'new'", "[profiles.fast]", `model = "mini"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "command = 'new'") > strings.Index(out, "[profiles.fast]") {
		t.Fatalf("block moved:\n%s", out)
	}
	back, ok := RemoveTOMLTable(out, "mcp_servers.nemesis")
	if !ok || strings.Contains(back, "nemesis") || !strings.Contains(back, "[profiles.fast]") || !strings.Contains(back, "[mcp_servers.other]") {
		t.Fatalf("remove:\n%s", back)
	}
	if strings.Contains(back, "\n\n\n") {
		t.Fatalf("blank lines piled up:\n%q", back)
	}
}

func TestTOMLAppendAndQuoted(t *testing.T) {
	out := SetTOMLTable("", "mcp_servers.nemesis", []string{"command = 'c'"})
	if out != "[mcp_servers.nemesis]\ncommand = 'c'\n" {
		t.Fatalf("%q", out)
	}
	in := "a = 1\n[mcp_servers.\"nemesis\"]\ncommand = 'x'\n"
	out = SetTOMLTable(in, "mcp_servers.nemesis", []string{"command = 'y'"})
	if strings.Contains(out, "'x'") || strings.Count(out, "nemesis") != 1 {
		t.Fatalf("%q", out)
	}
	if got := TOMLString(`C:\Users\a b\x.exe`); got != `'C:\Users\a b\x.exe'` {
		t.Fatal(got)
	}
	if got := TOMLString(`it's`); got != `"it's"` {
		t.Fatal(got)
	}
}

func TestEnv(t *testing.T) {
	in := "# key file\nOTHER=1\nGEMINI_API_KEY=old\n"
	out := SetEnv(in, map[string]string{"GEMINI_API_KEY": "new", "NEMESIS_NAME": "Nyx"})
	if out != "# key file\nOTHER=1\nGEMINI_API_KEY=new\nNEMESIS_NAME=Nyx\n" {
		t.Fatalf("%q", out)
	}
	if GetEnv(out, "GEMINI_API_KEY") != "new" {
		t.Fatal("get")
	}
	out = SetEnv(out, map[string]string{"NEMESIS_NAME": ""})
	if strings.Contains(out, "NEMESIS_NAME") || !strings.Contains(out, "OTHER=1") {
		t.Fatalf("%q", out)
	}
	if SetEnv("", map[string]string{"A": "b"}) != "A=b\n" {
		t.Fatal("empty")
	}
}
