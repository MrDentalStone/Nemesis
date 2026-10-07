// mksetup builds NemesisSetup.exe: it cross-builds nemesis-mcp and
// nemesis-cli for windows/amd64 into cmd/nemesis-setup/payload (which the
// installer embeds), copies the docs players may want next to them, then
// builds the installer as a GUI program (no console window).
//
//	go run ./cmd/nemesis-setup/mksetup -version v1.1.0 -out build/NemesisSetup.exe
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	version := flag.String("version", "dev", "version stamped into every binary")
	out := flag.String("out", filepath.Join("build", "NemesisSetup.exe"), "where to write the installer")
	keep := flag.Bool("keep", false, "leave the built programs in the payload folder")
	flag.Parse()

	payload := filepath.Join("cmd", "nemesis-setup", "payload")
	if _, err := os.Stat(payload); err != nil {
		fatal(fmt.Errorf("run from the repository root: %w", err))
	}
	if !*keep {
		cleanup = func() { clean(payload) }
		defer cleanup()
	}
	ld := "-s -w -X main.version=" + *version
	for _, cmd := range []string{"nemesis-mcp", "nemesis-cli"} {
		build(filepath.Join(payload, cmd+".exe"), ld, "./cmd/"+cmd)
	}
	for _, f := range []string{"README.md", "PLAYBOOK.md", "RULES.md", "LICENSE"} {
		data, err := os.ReadFile(f)
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(payload, f), data, 0o644); err != nil {
			fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fatal(err)
	}
	build(*out, ld+" -H windowsgui", "./cmd/nemesis-setup")
	st, _ := os.Stat(*out)
	fmt.Printf("wrote %s (%.1f MB)\n", *out, float64(st.Size())/(1<<20))
}

func build(out, ldflags, pkg string) {
	c := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, pkg)
	c.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	fmt.Println("building", out)
	if err := c.Run(); err != nil {
		fatal(fmt.Errorf("%s: %w", pkg, err))
	}
}

// clean empties the payload folder back to its placeholder.
func clean(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "PAYLOAD.txt" {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// cleanup empties the payload again; fatal runs it too (os.Exit skips defers)
var cleanup = func() {}

func fatal(err error) {
	cleanup()
	fmt.Fprintln(os.Stderr, "mksetup:", err)
	os.Exit(1)
}
