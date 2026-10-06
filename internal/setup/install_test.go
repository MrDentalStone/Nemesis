package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

type fakeSys struct {
	runs    []string
	entries map[string]map[string]string
	procs   []Proc
	killed  []uint32
}

func (f *fakeSys) Shortcut(lnk, target, args, wd, desc string) error {
	return os.WriteFile(lnk, []byte(target+" "+args), 0o644)
}
func (f *fakeSys) SetUninstallEntry(key string, s map[string]string, d map[string]uint32) error {
	if f.entries == nil {
		f.entries = map[string]map[string]string{}
	}
	f.entries[key] = s
	return nil
}
func (f *fakeSys) DeleteUninstallEntry(key string) error { delete(f.entries, key); return nil }
func (f *fakeSys) Run(name string, args ...string) (string, error) {
	f.runs = append(f.runs, strings.Join(args, " "))
	return "", nil
}
func (f *fakeSys) Processes() ([]Proc, error) { return f.procs, nil }
func (f *fakeSys) Kill(pid uint32) error {
	f.killed = append(f.killed, pid)
	var left []Proc
	for _, p := range f.procs {
		if p.PID != pid {
			left = append(left, p)
		}
	}
	f.procs = left
	return nil
}

func payload() fstest.MapFS {
	return fstest.MapFS{"nemesis-mcp.exe": {Data: []byte("mcp")}, "nemesis-cli.exe": {Data: []byte("cli")}, "PAYLOAD.txt": {Data: []byte("x")}}
}

func TestInstallUninstall(t *testing.T) {
	root := t.TempDir()
	p := TestPaths(root, "claude")
	sys := &fakeSys{}
	// a user's existing configs that must survive
	os.MkdirAll(p.ClaudeDesktopDirs[0], 0o755)
	os.WriteFile(p.ClaudeDesktopConfigs()[0], []byte(`{"mcpServers":{"fs":{"command":"npx"}},"theme":"dark"}`), 0o644)
	os.MkdirAll(filepath.Dir(p.CodexConfig), 0o755)
	os.WriteFile(p.CodexConfig, []byte("model = \"x\"\n"), 0o644)

	in := &Installer{P: p, Sys: sys, Version: "v9.9.9", Payload: payload(), Pad: []byte(`{"nemesisPad": true}`)}
	m, warns, err := in.Install(Options{ClaudeDesktop: true, ClaudeCode: true, Codex: true, GeminiCLI: true, Name: "Nyx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) > 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if len(m.Shortcuts) != 2 || !m.Codex || !m.GeminiCLI || !m.ClaudeCodeCLI || len(m.ClaudeConfigs) != 1 {
		t.Fatalf("manifest %+v", m)
	}
	cd, _ := os.ReadFile(p.ClaudeDesktopConfigs()[0])
	if !strings.Contains(string(cd), `"fs"`) || !strings.Contains(string(cd), `"-name"`) || !exists(p.ClaudeDesktopConfigs()[0]+".bak") {
		t.Fatalf("claude desktop:\n%s", cd)
	}
	if exists(p.AgentEnv()) {
		t.Fatal("no agent settings should be written")
	}
	if sys.entries[p.UninstallKey] == nil || !strings.Contains(sys.entries[p.UninstallKey]["DisplayIcon"], "nemesis-mcp.exe") ||
		!strings.Contains(sys.runs[1], "mcp add -s user nemesis -- ") {
		t.Fatalf("entries/runs: %v %v", sys.entries, sys.runs)
	}

	// re-install with fewer clients: the unticked ones are removed
	if _, _, err = in.Install(Options{ClaudeDesktop: true, Name: "Kestrel"}); err != nil {
		t.Fatal(err)
	}
	codex, _ := os.ReadFile(p.CodexConfig)
	gem, _ := os.ReadFile(p.GeminiSettings)
	if strings.Contains(string(codex), "nemesis") || HasServer(gem, "mcpServers", "nemesis") {
		t.Fatalf("unticked clients kept:\n%s\n%s", codex, gem)
	}

	in.Uninstall(false)
	cd, _ = os.ReadFile(p.ClaudeDesktopConfigs()[0])
	if strings.Contains(string(cd), "nemesis") || !strings.Contains(string(cd), `"theme"`) {
		t.Fatalf("after uninstall:\n%s", cd)
	}
	if exists(p.PadFile()) || exists(p.StartMenuDir) || sys.entries[p.UninstallKey] != nil {
		t.Fatal("left behind")
	}
}

// An install from before v0.10 had the Gemini agent: its program, shortcuts
// and key file. Upgrading drops the program and shortcuts and keeps the key.
func TestUpgradeFromAgentInstall(t *testing.T) {
	root := t.TempDir()
	p := TestPaths(root, "")
	sys := &fakeSys{}
	os.MkdirAll(p.InstallDir, 0o755)
	os.MkdirAll(p.StartMenuDir, 0o755)
	os.MkdirAll(p.DesktopDir, 0o755)
	os.MkdirAll(p.ConfigDir, 0o755)
	agent := p.Exe("nemesis-agent.exe")
	os.WriteFile(agent, []byte("agent"), 0o755)
	os.WriteFile(agent+".old-1", []byte("older"), 0o755)
	lnk := filepath.Join(p.StartMenuDir, "Nemesis (Gemini).lnk")
	desk := filepath.Join(p.DesktopDir, "Nemesis (Gemini).lnk")
	os.WriteFile(lnk, nil, 0o644)
	os.WriteFile(desk, nil, 0o644)
	os.WriteFile(p.AgentEnv(), []byte("GEMINI_API_KEY=k1\n"), 0o644)
	old, _ := json.Marshal(map[string]any{"version": "v0.9.1", "files": []string{"nemesis-mcp.exe", "nemesis-agent.exe", "nemesis-cli.exe"},
		"shortcuts": []string{lnk, desk}, "options": map[string]any{"agent": true, "key": "", "name": "Kestrel"}})
	os.WriteFile(p.Manifest(), old, 0o644)

	in := &Installer{P: p, Sys: sys, Version: "v0.10.0", Payload: payload(), Pad: []byte(`{"nemesisPad": true}`)}
	if _, warns, err := in.Install(Options{Name: "Kestrel"}); err != nil || len(warns) > 0 {
		t.Fatal(err, warns)
	}
	if exists(agent) || exists(agent+".old-1") || exists(lnk) || exists(desk) {
		t.Fatal("agent leftovers kept")
	}
	if !exists(p.AgentEnv()) {
		t.Fatal("the user's key file must stay on upgrade")
	}
}

func TestClosesRunningPrograms(t *testing.T) {
	root := t.TempDir()
	p := TestPaths(root, "")
	sys := &fakeSys{procs: []Proc{
		{PID: 101, Path: p.Exe("nemesis-mcp.exe")},
		{PID: 102, Path: strings.ToUpper(p.Exe("nemesis-agent.exe"))}, // Windows paths ignore case
		{PID: 103, Path: filepath.Join(root, "elsewhere", "nemesis-mcp.exe")},
	}}
	if d := Detect(p, sys, nil); len(d.Running) != 2 {
		t.Fatalf("running: %+v", d.Running)
	}
	in := &Installer{P: p, Sys: sys, Version: "v0.10.0", Payload: payload(), Pad: []byte(`{}`)}
	m, _, err := in.Install(Options{Name: "Kestrel", CloseRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sys.killed) != 2 || sys.killed[0] != 101 || sys.killed[1] != 102 || len(m.Closed) != 2 {
		t.Fatalf("killed %v, closed %v", sys.killed, m.Closed)
	}
	if raw, _ := os.ReadFile(p.Manifest()); strings.Contains(string(raw), "closeRunning\": true") {
		t.Fatal("CloseRunning is a one-off, not a saved setting")
	}

	// without the go-ahead nothing is closed
	sys2 := &fakeSys{procs: []Proc{{PID: 201, Path: p.Exe("nemesis-mcp.exe")}}}
	in.Sys = sys2
	if _, _, err := in.Install(Options{Name: "Kestrel"}); err != nil || len(sys2.killed) != 0 {
		t.Fatal(err, sys2.killed)
	}
}

func TestReplaceFileInUseFallback(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.exe")
	os.WriteFile(f, []byte("old"), 0o644)
	if _, err := replaceFile(f, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	if string(b) != "new" {
		t.Fatal(string(b))
	}
}

func TestNoPayload(t *testing.T) {
	in := &Installer{P: TestPaths(t.TempDir(), ""), Sys: &fakeSys{}, Payload: fstest.MapFS{"PAYLOAD.txt": {}}}
	if _, _, err := in.Install(Options{}); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatal(err)
	}
}
