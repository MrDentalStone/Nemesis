package ttsapi

import (
	"context"
	"encoding/csv"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// bindHint explains a failed bind on addr: which process holds the port, when
// the OS will say, and what to do about it.
func bindHint(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "another program is listening there"
	}
	pid, name := portOwner(port)
	switch {
	case strings.HasPrefix(strings.ToLower(name), "nemesis"):
		return fmt.Sprintf("held by %s PID %s: another Nemesis session (an older AI client window, or nemesis-cli) is still running; close it or end that process", name, pid)
	case name != "":
		return fmt.Sprintf("held by %s PID %s; only one program can take TTS's pushes (an Atom/VS Code TTS plugin, or another Nemesis)", name, pid)
	case pid != "":
		return fmt.Sprintf("held by PID %s; only one program can take TTS's pushes", pid)
	}
	return "another Nemesis session or a TTS editor plugin is listening there; only one can"
}

// portOwner finds the process listening on a local TCP port. Best effort:
// empty strings when the OS tools are missing or say nothing useful.
func portOwner(port string) (pid, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if runtime.GOOS == "windows" {
		out, err := exec.CommandContext(ctx, "netstat", "-ano", "-p", "TCP").Output()
		if err != nil {
			return "", ""
		}
		pid = netstatPID(string(out), port)
		if pid == "" {
			return "", ""
		}
		out, err = exec.CommandContext(ctx, "tasklist", "/FI", "PID eq "+pid, "/FO", "CSV", "/NH").Output()
		if err == nil {
			if rec, err := csv.NewReader(strings.NewReader(string(out))).Read(); err == nil && len(rec) > 1 {
				name = rec[0]
			}
		}
		return pid, name
	}
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-Fpc").Output()
	if err != nil {
		return "", ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(l, "p") && pid == "":
			pid = l[1:]
		case strings.HasPrefix(l, "c") && name == "":
			name = l[1:]
		}
	}
	return pid, name
}

// netstatPID picks the PID of the LISTENING row for port out of `netstat -ano`.
func netstatPID(out, port string) string {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 5 && f[3] == "LISTENING" && strings.HasSuffix(f[1], ":"+port) {
			return f[4]
		}
	}
	return ""
}
