// Package setup installs Nemesis for one Windows user: the binaries, the pad
// in TTS's Saved Objects, the AI clients' configs, the agent's key file,
// Start menu shortcuts and an Add/Remove Programs entry. Everything it does
// is recorded in a manifest so uninstall undoes exactly that. The config
// edits are plain functions over bytes so they are tested without touching
// a real profile; Paths can point the whole install into a test folder.
package setup

import (
	"os"
	"path/filepath"
)

// Paths is every place the installer reads or writes.
type Paths struct {
	InstallDir        string // %LOCALAPPDATA%\Programs\Nemesis
	ConfigDir         string // %APPDATA%\nemesis: agent.env, user modes and decks
	ClaudeDesktopDirs []string
	ClaudeJSON        string // ~/.claude.json, Claude Code's user config (fallback without the CLI)
	ClaudeCLI         string // claude(.cmd/.exe), "" when not found
	ManagedMCP        string // Claude Code's enterprise config; when present it overrides user servers
	CodexConfig       string // ~/.codex/config.toml (or $CODEX_HOME)
	GeminiSettings    string // ~/.gemini/settings.json
	SavedObjects      string // TTS Saved Objects folder
	StartMenuDir      string // ...\Start Menu\Programs\Nemesis
	DesktopDir        string
	UninstallKey      string // under HKEY_CURRENT_USER
	Temp              string
	Test              bool // a test root: never run the real claude CLI
}

const RealUninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\Nemesis`

// Folders the OS knows (Documents is often redirected to OneDrive, so it
// comes from the known-folder API rather than ~/Documents).
type Folders struct {
	Home, Roaming, Local, Documents, Programs, Desktop string
}

// RealPaths builds the paths for this user from the OS folders.
func RealPaths(f Folders, lookPath func(string) (string, error)) Paths {
	codex := filepath.Join(f.Home, ".codex")
	if h := os.Getenv("CODEX_HOME"); h != "" {
		codex = h
	}
	p := Paths{
		InstallDir:     filepath.Join(f.Local, "Programs", "Nemesis"),
		ConfigDir:      filepath.Join(f.Roaming, "nemesis"),
		ClaudeJSON:     filepath.Join(f.Home, ".claude.json"),
		ManagedMCP:     filepath.Join(os.Getenv("ProgramFiles"), "ClaudeCode", "managed-mcp.json"),
		CodexConfig:    filepath.Join(codex, "config.toml"),
		GeminiSettings: filepath.Join(f.Home, ".gemini", "settings.json"),
		SavedObjects:   filepath.Join(f.Documents, "My Games", "Tabletop Simulator", "Saves", "Saved Objects"),
		StartMenuDir:   filepath.Join(f.Programs, "Nemesis"),
		DesktopDir:     f.Desktop,
		UninstallKey:   RealUninstallKey,
		Temp:           os.TempDir(),
	}
	// Claude Desktop reads %APPDATA%\Claude; the Microsoft Store (MSIX) build
	// sees its own redirected copy under Packages\Claude_*\LocalCache.
	p.ClaudeDesktopDirs = []string{filepath.Join(f.Roaming, "Claude")}
	if m, _ := filepath.Glob(filepath.Join(f.Local, "Packages", "Claude_*", "LocalCache", "Roaming", "Claude")); len(m) > 0 {
		p.ClaudeDesktopDirs = append(p.ClaudeDesktopDirs, m...)
	}
	if lookPath != nil {
		if c, err := lookPath("claude"); err == nil {
			p.ClaudeCLI = c
		}
	}
	return p
}

// TestPaths puts every path under root, so a whole install and uninstall can
// run without touching the real profile. The Add/Remove Programs entry goes
// to a separate key, and the claude CLI is only the one claudeCLI names.
func TestPaths(root, claudeCLI string) Paths {
	j := func(p ...string) string { return filepath.Join(append([]string{root}, p...)...) }
	return Paths{
		InstallDir:        j("Local", "Programs", "Nemesis"),
		ConfigDir:         j("Roaming", "nemesis"),
		ClaudeDesktopDirs: []string{j("Roaming", "Claude")},
		ClaudeJSON:        j("home", ".claude.json"),
		ClaudeCLI:         claudeCLI,
		ManagedMCP:        j("ProgramFiles", "ClaudeCode", "managed-mcp.json"),
		CodexConfig:       j("home", ".codex", "config.toml"),
		GeminiSettings:    j("home", ".gemini", "settings.json"),
		SavedObjects:      j("Documents", "My Games", "Tabletop Simulator", "Saves", "Saved Objects"),
		StartMenuDir:      j("StartMenu", "Nemesis"),
		DesktopDir:        j("Desktop"),
		UninstallKey:      `Software\NemesisSetupTest\Nemesis`,
		Temp:              j("Temp"),
		Test:              true,
	}
}

func (p Paths) ClaudeDesktopConfigs() []string {
	out := make([]string, len(p.ClaudeDesktopDirs))
	for i, d := range p.ClaudeDesktopDirs {
		out[i] = filepath.Join(d, "claude_desktop_config.json")
	}
	return out
}

func (p Paths) AgentEnv() string       { return filepath.Join(p.ConfigDir, "agent.env") }
func (p Paths) Manifest() string       { return filepath.Join(p.InstallDir, "install.json") }
func (p Paths) Exe(name string) string { return filepath.Join(p.InstallDir, name) }
func (p Paths) PadFile() string        { return filepath.Join(p.SavedObjects, "Nemesis.json") }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
