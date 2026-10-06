package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sys is what only Windows can do; the command package provides it.
type Sys interface {
	Shortcut(lnk, target, args, workDir, desc string) error
	SetUninstallEntry(key string, str map[string]string, dword map[string]uint32) error
	DeleteUninstallEntry(key string) error
	// Run starts a console program without flashing a window.
	Run(name string, args ...string) (string, error)
	// Processes lists running programs whose name starts with "nemesis",
	// with their full image path; Kill ends one and waits for it to go.
	Processes() ([]Proc, error)
	Kill(pid uint32) error
}

// Proc is a running program.
type Proc struct {
	PID  uint32 `json:"pid"`
	Path string `json:"path"`
}

// Options is what the user picked in the wizard (or on the command line).
type Options struct {
	ClaudeDesktop bool   `json:"claudeDesktop"`
	ClaudeCode    bool   `json:"claudeCode"`
	Codex         bool   `json:"codex"`
	GeminiCLI     bool   `json:"geminiCli"`
	Name          string `json:"name"`
	// CloseRunning ends Nemesis programs still running from the install
	// folder before their files are replaced (the wizard asks first).
	CloseRunning bool `json:"closeRunning"`
}

// Manifest records an install so the next install and the uninstall know
// what is ours.
type Manifest struct {
	Version       string    `json:"version"`
	Installed     time.Time `json:"installed"`
	Options       Options   `json:"options"`
	Files         []string  `json:"files"`
	Shortcuts     []string  `json:"shortcuts"`
	PadFile       string    `json:"padFile,omitempty"`
	ClaudeConfigs []string  `json:"claudeDesktopConfigs,omitempty"`
	ClaudeCodeCLI bool      `json:"claudeCodeCli,omitempty"` // added with `claude mcp add`
	ClaudeJSON    bool      `json:"claudeJson,omitempty"`    // added to ~/.claude.json directly
	Codex         bool      `json:"codex,omitempty"`
	GeminiCLI     bool      `json:"geminiCli,omitempty"`
	// Closed lists the programs this run had to end (not kept between runs).
	Closed []string `json:"closed,omitempty"`
}

// Installer runs the steps; Log gets one line per step for the wizard.
type Installer struct {
	P       Paths
	Sys     Sys
	Payload fs.FS  // nemesis-mcp.exe, nemesis-cli.exe, docs
	Pad     []byte // the Saved Object file
	Version string
	Self    string // this executable, copied as uninstall.exe
	Log     func(string)
}

func (in *Installer) logf(f string, a ...any) {
	if in.Log != nil {
		in.Log(fmt.Sprintf(f, a...))
	}
}

// PayloadOK reports whether this build carries the binaries.
func PayloadOK(p fs.FS) bool {
	_, err := fs.Stat(p, "nemesis-mcp.exe")
	return err == nil
}

// ReadManifest returns the current install, or nil.
func ReadManifest(p Paths) *Manifest {
	raw, err := os.ReadFile(p.Manifest())
	if err != nil {
		return nil
	}
	var m Manifest
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return &m
}

// Detection is what the welcome page shows and how the choices are ticked.
type Detection struct {
	TTS           bool      `json:"tts"`
	SavedObjects  string    `json:"savedObjects"`
	ClaudeDesktop bool      `json:"claudeDesktop"`
	ClaudeCode    bool      `json:"claudeCode"`
	ClaudeCodeCLI bool      `json:"claudeCodeCli"`
	ManagedMCP    bool      `json:"managedMcp"`
	Codex         bool      `json:"codex"`
	GeminiCLI     bool      `json:"geminiCli"`
	AgentEnv      bool      `json:"agentEnv"` // a key file from the old Gemini agent
	SavedName     string    `json:"savedName"`
	Existing      *Manifest `json:"existing"`
	InstallDir    string    `json:"installDir"`
	Running       []Proc    `json:"running"`
}

func Detect(p Paths, sys Sys, lookPath func(string) (string, error)) Detection {
	d := Detection{SavedObjects: p.SavedObjects, InstallDir: p.InstallDir}
	// TTS creates "My Games\Tabletop Simulator" on first start
	d.TTS = exists(filepath.Dir(filepath.Dir(p.SavedObjects)))
	for _, dir := range p.ClaudeDesktopDirs {
		d.ClaudeDesktop = d.ClaudeDesktop || exists(dir)
	}
	d.ClaudeCodeCLI = p.ClaudeCLI != ""
	d.ClaudeCode = d.ClaudeCodeCLI || exists(p.ClaudeJSON)
	d.ManagedMCP = exists(p.ManagedMCP)
	d.Codex = exists(filepath.Dir(p.CodexConfig))
	d.GeminiCLI = exists(filepath.Dir(p.GeminiSettings))
	if !p.Test && lookPath != nil {
		_, e1 := lookPath("codex")
		_, e2 := lookPath("gemini")
		d.Codex = d.Codex || e1 == nil
		d.GeminiCLI = d.GeminiCLI || e2 == nil
	}
	d.AgentEnv = exists(p.AgentEnv())
	d.Running = running(p, sys)
	d.Existing = ReadManifest(p)
	if d.Existing != nil && d.Existing.Options.Name != "" {
		d.SavedName = d.Existing.Options.Name
	}
	return d
}

// server is an MCP server entry, in the key order people expect to read.
type server struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
	Trust   bool              `json:"trust,omitempty"`
}

// serverArgs are the nemesis-mcp arguments for the chosen name.
func serverArgs(name string) []string {
	if name == "" || name == "Kestrel" {
		return []string{}
	}
	return []string{"-name", name}
}

// owner names the program to restart when a binary is in use.
var owner = map[string]string{
	"nemesis-mcp.exe":   "your AI client (Claude Desktop, Claude Code, Codex...)",
	"nemesis-agent.exe": "the Nemesis (Gemini) window",
	"nemesis-cli.exe":   "nemesis-cli",
}

// running lists Nemesis programs started from the install folder. An upgrade
// that only moves their files aside leaves the old version playing, which
// is easy to miss (a whole test game ran on the old agent that way).
func running(p Paths, sys Sys) []Proc {
	if sys == nil {
		return nil
	}
	all, err := sys.Processes()
	if err != nil {
		return nil
	}
	dir := strings.ToLower(filepath.Clean(p.InstallDir)) + string(filepath.Separator)
	self := os.Getpid()
	var out []Proc
	for _, pr := range all {
		if int(pr.PID) != self && strings.HasPrefix(strings.ToLower(filepath.Clean(pr.Path)), dir) {
			out = append(out, pr)
		}
	}
	return out
}

// closeRunning ends the programs running from the install folder and
// returns their names.
func (in *Installer) closeRunning(warn func(string, ...any)) []string {
	var closed []string
	for _, pr := range running(in.P, in.Sys) {
		name := filepath.Base(pr.Path)
		if err := in.Sys.Kill(pr.PID); err != nil {
			warn("could not close %s (PID %d): %v", name, pr.PID, err)
			continue
		}
		closed = append(closed, name)
		in.logf("Closed %s (PID %d) so it can be updated", name, pr.PID)
	}
	return closed
}

// Install runs every step; it stops at the first one that cannot be done
// safely and leaves what was already done in place (re-running repairs).
func (in *Installer) Install(o Options) (*Manifest, []string, error) {
	var warnings []string
	warn := func(f string, a ...any) {
		w := fmt.Sprintf(f, a...)
		warnings = append(warnings, w)
		in.logf("NOTE: %s", w)
	}
	if !PayloadOK(in.Payload) {
		return nil, nil, errors.New("this setup was built without the Nemesis programs inside (no payload); download the release NemesisSetup.exe")
	}
	o.Name = strings.TrimSpace(o.Name)
	if o.Name == "" {
		o.Name = "Kestrel"
	}
	prev := ReadManifest(in.P)
	m := &Manifest{Version: in.Version, Installed: time.Now(), Options: o}
	m.Options.CloseRunning = false

	// 0. programs still running the old version
	if o.CloseRunning {
		m.Closed = in.closeRunning(warn)
	}

	// 1. programs
	in.logf("Installing the programs to %s", in.P.InstallDir)
	if err := os.MkdirAll(in.P.InstallDir, 0o755); err != nil {
		return nil, nil, err
	}
	clearOld(in.P.InstallDir)
	err := fs.WalkDir(in.Payload, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == "PAYLOAD.txt" {
			return err
		}
		data, err := fs.ReadFile(in.Payload, path)
		if err != nil {
			return err
		}
		inUse, err := replaceFile(in.P.Exe(path), data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if inUse {
			warn("IMPORTANT: %s is still running the OLD version: close %s and start it again to use %s", path, owner[path], in.Version)
		}
		m.Files = append(m.Files, path)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	// files an older version installed that this one does not (the Gemini
	// agent before v0.10)
	if prev != nil {
		keep := map[string]bool{"uninstall.exe": true}
		for _, f := range m.Files {
			keep[f] = true
		}
		for _, f := range prev.Files {
			if keep[f] {
				continue
			}
			if err := os.Remove(in.P.Exe(f)); err == nil {
				in.logf("Removed %s (no longer part of Nemesis)", f)
			} else if !errors.Is(err, os.ErrNotExist) {
				warn("IMPORTANT: could not remove %s: %v. If it is running, close it: it is an old version", f, err)
			}
		}
	}
	if in.Self != "" && !samePath(in.Self, in.P.Exe("uninstall.exe")) {
		self, err := os.ReadFile(in.Self)
		if err == nil {
			_, err = replaceFile(in.P.Exe("uninstall.exe"), self)
		}
		if err != nil {
			warn("could not copy the uninstaller: %v", err)
		} else {
			m.Files = append(m.Files, "uninstall.exe")
		}
	}

	// 2. the pad
	if err := os.MkdirAll(in.P.SavedObjects, 0o755); err != nil {
		warn("could not create the TTS Saved Objects folder: %v", err)
	} else if err := os.WriteFile(in.P.PadFile(), in.Pad, 0o644); err != nil {
		warn("could not write the pad to Saved Objects: %v", err)
	} else {
		m.PadFile = in.P.PadFile()
		in.logf("Placed the Nemesis pad in TTS Saved Objects (%s)", in.P.SavedObjects)
	}

	mcp := in.P.Exe("nemesis-mcp.exe")
	args := serverArgs(o.Name)

	// 3. clients; ones ticked last time and not now are removed
	if o.ClaudeDesktop {
		entry := server{Command: mcp, Args: args}
		for _, cfg := range in.P.ClaudeDesktopConfigs() {
			if err := editJSON(cfg, true, func(b []byte) ([]byte, error) { return SetServer(b, "mcpServers", "nemesis", entry) }); err != nil {
				warn("Claude Desktop config %s: %v", cfg, err)
				continue
			}
			m.ClaudeConfigs = append(m.ClaudeConfigs, cfg)
			in.logf("Added Nemesis to Claude Desktop (%s). Quit Claude Desktop from the tray and start it again.", cfg)
		}
	} else if prev != nil {
		in.removeClaudeDesktop(prev.ClaudeConfigs)
	}

	if o.ClaudeCode {
		in.claudeCode(m, mcp, args, warn)
	} else if prev != nil && (prev.ClaudeCodeCLI || prev.ClaudeJSON) {
		in.removeClaudeCode(prev)
	}

	if o.Codex {
		body := []string{
			"command = " + TOMLString(mcp),
			"args = " + TOMLArray(args),
			"startup_timeout_sec = 20",
			"tool_timeout_sec = 300",
		}
		err := editText(in.P.CodexConfig, func(s string) string { return SetTOMLTable(s, "mcp_servers.nemesis", body) })
		if err != nil {
			warn("Codex config: %v", err)
		} else {
			m.Codex = true
			in.logf("Added Nemesis to Codex (%s)", in.P.CodexConfig)
		}
	} else if prev != nil && prev.Codex {
		in.removeCodex()
	}

	if o.GeminiCLI {
		entry := server{Command: mcp, Args: args, Timeout: 600000, Trust: true}
		if err := editJSON(in.P.GeminiSettings, false, func(b []byte) ([]byte, error) { return SetServer(b, "mcpServers", "nemesis", entry) }); err != nil {
			warn("Gemini CLI settings: %v", err)
		} else {
			m.GeminiCLI = true
			in.logf("Added Nemesis to Gemini CLI (%s)", in.P.GeminiSettings)
		}
	} else if prev != nil && prev.GeminiCLI {
		in.removeGemini()
	}

	// 4. shortcuts
	in.shortcuts(m, prev, warn)

	// 5. Add/Remove Programs
	size := uint32(0)
	for _, f := range m.Files {
		if st, err := os.Stat(in.P.Exe(f)); err == nil {
			size += uint32(st.Size() / 1024)
		}
	}
	uninst := in.P.Exe("uninstall.exe")
	err = in.Sys.SetUninstallEntry(in.P.UninstallKey, map[string]string{
		"DisplayName":     "Nemesis (AI player for Tabletop Simulator)",
		"DisplayVersion":  strings.TrimPrefix(in.Version, "v"),
		"Publisher":       "Nemesis",
		"InstallLocation": in.P.InstallDir,
		"UninstallString": `"` + uninst + `" -uninstall`,
		"DisplayIcon":     in.P.Exe("nemesis-mcp.exe"),
		"URLInfoAbout":    "https://github.com/MrDentalStone/Nemesis",
	}, map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": size})
	if err != nil {
		warn("could not register in Add/Remove Programs: %v", err)
	}

	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(in.P.Manifest(), raw, 0o644); err != nil {
		return nil, warnings, err
	}
	in.logf("Done: Nemesis %s is installed.", in.Version)
	return m, warnings, nil
}

func (in *Installer) claudeCode(m *Manifest, mcp string, args []string, warn func(string, ...any)) {
	snippet := func() string {
		e, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"nemesis": server{Type: "stdio", Command: mcp, Args: args}}})
		return string(e)
	}
	if exists(in.P.ManagedMCP) {
		raw, _ := os.ReadFile(in.P.ManagedMCP)
		if !HasServer(raw, "mcpServers", "nemesis") {
			warn("Claude Code on this PC uses an admin-managed server list (%s), which overrides your own. Ask whoever manages this PC to add: %s", in.P.ManagedMCP, snippet())
		}
	}
	if in.P.ClaudeCLI != "" {
		in.Sys.Run(in.P.ClaudeCLI, "mcp", "remove", "-s", "user", "nemesis")
		out, err := in.Sys.Run(in.P.ClaudeCLI, append([]string{"mcp", "add", "-s", "user", "nemesis", "--", mcp}, args...)...)
		low := strings.ToLower(out)
		switch {
		case strings.Contains(low, "enterprise") || strings.Contains(low, "managed"):
			warn("Claude Code says its MCP servers are managed by an administrator. Ask them to add to managed-mcp.json: %s", snippet())
		case err != nil:
			warn("claude mcp add failed: %v %s", err, strings.TrimSpace(out))
		default:
			m.ClaudeCodeCLI = true
			in.logf("Added Nemesis to Claude Code (user scope, all folders)")
		}
		return
	}
	entry := server{Type: "stdio", Command: mcp, Args: args, Env: map[string]string{}}
	if err := editJSON(in.P.ClaudeJSON, false, func(b []byte) ([]byte, error) { return SetServer(b, "mcpServers", "nemesis", entry) }); err != nil {
		warn("Claude Code config %s: %v", in.P.ClaudeJSON, err)
		return
	}
	m.ClaudeJSON = true
	in.logf("Added Nemesis to Claude Code (%s). Restart Claude Code to pick it up.", in.P.ClaudeJSON)
}

func (in *Installer) shortcuts(m *Manifest, prev *Manifest, warn func(string, ...any)) {
	if prev != nil {
		for _, s := range prev.Shortcuts {
			os.Remove(s)
		}
	}
	if err := os.MkdirAll(in.P.StartMenuDir, 0o755); err != nil {
		warn("could not create the Start menu folder: %v", err)
		return
	}
	add := func(lnk, target, args, desc string) {
		os.MkdirAll(filepath.Dir(lnk), 0o755)
		if err := in.Sys.Shortcut(lnk, target, args, in.P.InstallDir, desc); err != nil {
			warn("shortcut %s: %v", filepath.Base(lnk), err)
			return
		}
		m.Shortcuts = append(m.Shortcuts, lnk)
	}
	// shortcuts from older versions that are not in the manifest
	os.Remove(filepath.Join(in.P.StartMenuDir, "Nemesis (Gemini).lnk"))
	if in.P.DesktopDir != "" {
		os.Remove(filepath.Join(in.P.DesktopDir, "Nemesis (Gemini).lnk"))
	}
	add(filepath.Join(in.P.StartMenuDir, "Nemesis folder.lnk"), in.P.InstallDir, "", "Where Nemesis is installed")
	add(filepath.Join(in.P.StartMenuDir, "Uninstall Nemesis.lnk"), in.P.Exe("uninstall.exe"), "-uninstall", "Remove Nemesis")
	in.logf("Added Start menu shortcuts (Nemesis folder)")
}

// Uninstall undoes what the manifest records (or everything Nemesis-named
// when there is no manifest). The install folder itself is removed by the
// caller after this process exits (an exe cannot delete itself).
func (in *Installer) Uninstall(deleteKey bool) []string {
	var warnings []string
	in.closeRunning(func(f string, a ...any) { warnings = append(warnings, fmt.Sprintf(f, a...)) })
	m := ReadManifest(in.P)
	if m == nil {
		in.logf("No install record found; removing whatever Nemesis entries are present")
		m = &Manifest{ClaudeConfigs: in.P.ClaudeDesktopConfigs(), ClaudeCodeCLI: in.P.ClaudeCLI != "", ClaudeJSON: true,
			Codex: true, GeminiCLI: true, PadFile: in.P.PadFile()}
	}
	in.removeClaudeDesktop(m.ClaudeConfigs)
	in.removeClaudeCode(m)
	if m.Codex {
		in.removeCodex()
	}
	if m.GeminiCLI {
		in.removeGemini()
	}
	for _, s := range m.Shortcuts {
		os.Remove(s)
	}
	if in.P.DesktopDir != "" {
		os.Remove(filepath.Join(in.P.DesktopDir, "Nemesis (Gemini).lnk"))
	}
	os.RemoveAll(in.P.StartMenuDir)
	in.logf("Removed the shortcuts")
	if m.PadFile != "" {
		if raw, err := os.ReadFile(m.PadFile); err == nil && strings.Contains(string(raw), "nemesisPad") {
			os.Remove(m.PadFile)
			in.logf("Removed the pad from TTS Saved Objects (pads already on a table stay there)")
		}
	}
	if deleteKey {
		if err := os.Remove(in.P.AgentEnv()); err == nil {
			in.logf("Deleted %s", in.P.AgentEnv())
		}
	}
	if err := in.Sys.DeleteUninstallEntry(in.P.UninstallKey); err != nil {
		warnings = append(warnings, err.Error())
	}
	return warnings
}

func (in *Installer) removeClaudeDesktop(cfgs []string) {
	for _, cfg := range cfgs {
		if removeServerFrom(cfg) {
			in.logf("Removed Nemesis from Claude Desktop (%s)", cfg)
		}
	}
}

func (in *Installer) removeClaudeCode(m *Manifest) {
	if m.ClaudeCodeCLI && in.P.ClaudeCLI != "" {
		if _, err := in.Sys.Run(in.P.ClaudeCLI, "mcp", "remove", "-s", "user", "nemesis"); err == nil {
			in.logf("Removed Nemesis from Claude Code")
		}
	}
	if m.ClaudeJSON && removeServerFrom(in.P.ClaudeJSON) {
		in.logf("Removed Nemesis from Claude Code (%s)", in.P.ClaudeJSON)
	}
}

func (in *Installer) removeCodex() {
	changed := false
	editText(in.P.CodexConfig, func(s string) string {
		out, ok := RemoveTOMLTable(s, "mcp_servers.nemesis")
		changed = ok
		return out
	})
	if changed {
		in.logf("Removed Nemesis from Codex")
	}
}

func (in *Installer) removeGemini() {
	if removeServerFrom(in.P.GeminiSettings) {
		in.logf("Removed Nemesis from Gemini CLI")
	}
}

func removeServerFrom(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	out, changed, err := RemoveServer(raw, "mcpServers", "nemesis")
	if err != nil || !changed {
		return false
	}
	return writeAtomic(path, out) == nil
}

// editJSON rewrites a JSON config through fn. backup keeps the user's
// original next to it once (<file>.bak), before our first edit.
func editJSON(path string, backup bool, fn func([]byte) ([]byte, error)) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := fn(raw)
	if err != nil {
		return fmt.Errorf("%w (the file is not valid JSON; fix or delete it and run setup again)", err)
	}
	if backup && len(raw) > 0 && !exists(path+".bak") {
		os.WriteFile(path+".bak", raw, 0o644)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, out)
}

func editText(path string, fn func(string) string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, []byte(fn(string(raw))))
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".nemesis-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// replaceFile writes data to path. A running exe cannot be overwritten on
// Windows but it can be renamed, so the old one moves aside to *.old-N and
// is deleted now or on the next install. inUse reports that it could not be
// deleted yet (the program is running the old version).
func replaceFile(path string, data []byte) (inUse bool, err error) {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return false, err
	}
	if exists(path) {
		if os.Remove(path) != nil {
			old := ""
			for i := 1; ; i++ {
				old = fmt.Sprintf("%s.old-%d", path, i)
				if !exists(old) {
					break
				}
			}
			if err := os.Rename(path, old); err != nil {
				os.Remove(tmp)
				return false, fmt.Errorf("in use and cannot be moved aside: %w", err)
			}
			inUse = os.Remove(old) != nil
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return inUse, err
	}
	return inUse, nil
}

func clearOld(dir string) {
	m, _ := filepath.Glob(filepath.Join(dir, "*.old-*"))
	for _, f := range m {
		os.Remove(f)
	}
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
