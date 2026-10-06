//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"

	"github.com/MrDentalStone/Nemesis/internal/setup"
)

// The installer is for Windows; elsewhere it builds (so `go build ./...`
// and CI pass) and can run against a -root test folder.

var errWindows = errors.New("only on Windows")

type winSys struct{}

func folders() (setup.Folders, error) { return setup.Folders{}, errWindows }

func (winSys) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
func (winSys) Shortcut(lnk, target, args, workDir, desc string) error {
	return os.WriteFile(lnk, []byte(target+" "+args+"\n"), 0o644)
}
func (winSys) SetUninstallEntry(string, map[string]string, map[string]uint32) error { return nil }
func (winSys) DeleteUninstallEntry(string) error                                    { return nil }

func openBrowser(url string) error              { return exec.Command("xdg-open", url).Start() }
func messageBox(title, text string, _ bool)     { os.Stderr.WriteString(title + ": " + text + "\n") }
func (winSys) Processes() ([]setup.Proc, error) { return nil, nil }
func (winSys) Kill(uint32) error                { return errWindows }
func removeLater(dir, _ string) error           { return os.RemoveAll(dir) }
