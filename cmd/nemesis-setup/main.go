// nemesis-setup is NemesisSetup.exe: a one-file Windows installer with the
// programs inside. It runs a small wizard in the browser (no admin rights,
// everything per user): close Nemesis programs still running the old
// version, copy the programs, put the pad in TTS's Saved Objects, add
// Nemesis to the AI clients the user picks, and add Start menu shortcuts and
// an Add/Remove Programs entry. Installed as uninstall.exe, -uninstall undoes
// it.
//
// Build it with the programs inside: go run ./cmd/nemesis-setup/mksetup
package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MrDentalStone/Nemesis/internal/padobj"
	"github.com/MrDentalStone/Nemesis/internal/setup"
)

// set by mksetup (-ldflags "-X main.version=...")
var version = "dev"

// payload holds nemesis-mcp.exe and nemesis-cli.exe when built by mksetup;
// a plain build carries only PAYLOAD.txt.
//
//go:embed payload
var payloadFS embed.FS

func main() {
	uninstall := flag.Bool("uninstall", false, "remove Nemesis")
	silent := flag.Bool("silent", false, "no wizard: install (or with -uninstall, remove) with the flags below")
	o := setup.Options{}
	flag.BoolVar(&o.ClaudeDesktop, "claude-desktop", false, "silent: add to Claude Desktop")
	flag.BoolVar(&o.ClaudeCode, "claude-code", false, "silent: add to Claude Code")
	flag.BoolVar(&o.Codex, "codex", false, "silent: add to Codex")
	flag.BoolVar(&o.GeminiCLI, "gemini-cli", false, "silent: add to Gemini CLI")
	flag.StringVar(&o.Name, "name", "Kestrel", "silent: the AI's name at the table")
	deleteKey := flag.Bool("delete-key", false, "silent uninstall: also delete agent.env (the old Gemini agent's key file)")
	root := flag.String("root", os.Getenv("NEMESIS_SETUP_ROOT"), "test: install everything under this folder instead of the real profile")
	claudeCLI := flag.String("claude-cli", os.Getenv("NEMESIS_SETUP_CLAUDE"), "test: the claude command to use with -root")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser (the wizard address goes to the log)")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *showVersion {
		fmt.Println("nemesis-setup", version)
		return
	}

	var p setup.Paths
	if *root != "" {
		abs, _ := filepath.Abs(*root)
		p = setup.TestPaths(abs, *claudeCLI)
	} else {
		f, err := folders()
		if err != nil {
			messageBox("Nemesis setup", "Cannot find your Windows folders: "+err.Error(), true)
			os.Exit(1)
		}
		p = setup.RealPaths(f, exec.LookPath)
	}
	os.MkdirAll(p.Temp, 0o755)
	logPath := filepath.Join(p.Temp, "nemesis-setup.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		defer lf.Close()
		log.SetOutput(io.MultiWriter(lf, os.Stderr))
	}
	log.SetFlags(log.LstdFlags)
	log.Printf("nemesis-setup %s (uninstall=%v silent=%v root=%q)", version, *uninstall, *silent, *root)

	payload, _ := fs.Sub(payloadFS, "payload")
	pad, err := padobj.SaveFile(padobj.Lua())
	if err != nil {
		log.Fatal(err)
	}
	self, _ := os.Executable()
	in := &setup.Installer{P: p, Sys: winSys{}, Payload: payload, Pad: pad, Version: version, Self: self,
		Log: func(s string) { log.Print(s) }}

	if *silent {
		// no one to ask: programs still running the old version are closed
		o.CloseRunning = true
		code := 0
		if *uninstall {
			for _, w := range in.Uninstall(*deleteKey) {
				log.Print("NOTE: ", w)
			}
			finishUninstall(in, self)
		} else if _, _, err := in.Install(o); err != nil {
			log.Print("FAILED: ", err)
			code = 1
		}
		os.Exit(code)
	}

	mode := "install"
	if *uninstall {
		mode = "uninstall"
	}
	w := newWizard(mode, in)
	url, err := w.start()
	if err != nil {
		messageBox("Nemesis setup", "Could not start the setup page: "+err.Error(), true)
		os.Exit(1)
	}
	log.Printf("wizard at %s", url)
	if !*noBrowser {
		if err := openBrowser(url); err != nil {
			go messageBox("Nemesis setup", "Open this address in your browser to continue:\n\n"+url, false)
		} else {
			// opened, but nobody came: show the address in case the browser did not
			go func() {
				time.Sleep(45 * time.Second)
				if !w.contacted() {
					messageBox("Nemesis setup", "If the setup page did not open, paste this into your browser:\n\n"+url, false)
				}
			}()
		}
	}
	w.wait()
	if w.uninstalled() {
		finishUninstall(in, self)
	}
	log.Print("setup finished")
}

// finishUninstall removes the install folder once this process has exited
// (uninstall.exe lives in it).
func finishUninstall(in *setup.Installer, self string) {
	dir := in.P.InstallDir
	if !strings.HasPrefix(strings.ToLower(filepath.Clean(self)), strings.ToLower(filepath.Clean(dir))+string(filepath.Separator)) {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("could not remove %s: %v", dir, err)
		} else {
			log.Printf("removed %s", dir)
		}
		return
	}
	if err := removeLater(dir, in.P.Temp); err != nil {
		log.Printf("could not schedule removing %s: %v", dir, err)
		return
	}
	log.Printf("%s will be removed in a few seconds", dir)
}
