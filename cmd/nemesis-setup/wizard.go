package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/MrDentalStone/Nemesis/internal/setup"
)

//go:embed wizard.html
var page string

// wizard serves the setup page on 127.0.0.1 with a random token, so only
// the browser this process opened can drive it. It ends when the page says
// so, or when the page has stopped checking in for a while.
type wizard struct {
	mode  string // install | uninstall
	in    *setup.Installer
	token string
	addr  string
	quit  chan struct{}

	mu       sync.Mutex
	started  time.Time
	lastSeen time.Time
	busy     bool
	done     bool
	lines    []string
	result   map[string]any
	removed  bool
	quitOnce sync.Once
}

func newWizard(mode string, in *setup.Installer) *wizard {
	b := make([]byte, 16)
	rand.Read(b)
	return &wizard{mode: mode, in: in, token: hex.EncodeToString(b), quit: make(chan struct{}), started: time.Now()}
}

func (w *wizard) start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	w.addr = ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", w.index)
	mux.HandleFunc("GET /api/state", w.api(w.state))
	mux.HandleFunc("POST /api/ping", w.api(func(*http.Request) (any, error) { return map[string]any{"ok": true}, nil }))
	mux.HandleFunc("POST /api/install", w.api(w.install))
	mux.HandleFunc("POST /api/uninstall", w.api(w.uninstall))
	mux.HandleFunc("GET /api/progress", w.api(w.progress))
	mux.HandleFunc("POST /api/quit", w.api(func(*http.Request) (any, error) {
		go func() { time.Sleep(300 * time.Millisecond); w.stop() }()
		return map[string]any{"ok": true}, nil
	}))
	go http.Serve(ln, mux)
	// the in-process log also feeds the page
	prev := w.in.Log
	w.in.Log = func(s string) {
		if prev != nil {
			prev(s)
		}
		w.mu.Lock()
		w.lines = append(w.lines, s)
		w.mu.Unlock()
	}
	return fmt.Sprintf("http://%s/?t=%s", w.addr, w.token), nil
}

func (w *wizard) stop() { w.quitOnce.Do(func() { close(w.quit) }) }

// wait blocks until the page quits, or nobody has checked in for 90 s (the
// tab was closed), or nobody ever came within 15 minutes.
func (w *wizard) wait() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-w.quit:
			return
		case <-t.C:
			w.mu.Lock()
			idle := !w.lastSeen.IsZero() && !w.busy && time.Since(w.lastSeen) > 90*time.Second
			never := w.lastSeen.IsZero() && time.Since(w.started) > 15*time.Minute
			w.mu.Unlock()
			if idle || never {
				return
			}
		}
	}
}

func (w *wizard) contacted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.lastSeen.IsZero()
}

func (w *wizard) uninstalled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.removed
}

// guard: the Host must be ours (no DNS rebinding) and the token must match.
func (w *wizard) ok(r *http.Request, tok string) bool {
	return r.Host == w.addr && tok == w.token
}

func (w *wizard) index(rw http.ResponseWriter, r *http.Request) {
	if !w.ok(r, r.URL.Query().Get("t")) {
		http.Error(rw, "This setup page only works from the link the installer opened.", http.StatusForbidden)
		return
	}
	w.touch()
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Write([]byte(strings.Replace(page, "{{TOKEN}}", w.token, 1)))
}

func (w *wizard) touch() {
	w.mu.Lock()
	w.lastSeen = time.Now()
	w.mu.Unlock()
}

func (w *wizard) api(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !w.ok(r, r.Header.Get("X-Nemesis-Token")) {
			http.Error(rw, "forbidden", http.StatusForbidden)
			return
		}
		w.touch()
		out, err := fn(r)
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "no-store")
		if err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			out = map[string]any{"error": err.Error()}
		}
		json.NewEncoder(rw).Encode(out)
	}
}

func (w *wizard) state(*http.Request) (any, error) {
	return map[string]any{
		"mode":      w.mode,
		"version":   w.in.Version,
		"payloadOk": setup.PayloadOK(w.in.Payload),
		"detect":    setup.Detect(w.in.P, w.in.Sys, exec.LookPath),
		"test":      w.in.P.Test,
	}, nil
}

// begin marks the wizard busy; only one install or uninstall at a time.
func (w *wizard) begin() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.busy {
		return fmt.Errorf("already running")
	}
	w.busy, w.done, w.lines, w.result = true, false, nil, nil
	return nil
}

func (w *wizard) finish(res map[string]any) {
	w.mu.Lock()
	w.busy, w.done, w.result = false, true, res
	w.mu.Unlock()
}

func (w *wizard) install(r *http.Request) (any, error) {
	var o setup.Options
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		return nil, err
	}
	if err := w.begin(); err != nil {
		return nil, err
	}
	go func() {
		m, warns, err := w.in.Install(o)
		res := map[string]any{"ok": err == nil, "warnings": warns}
		if err != nil {
			res["error"] = err.Error()
		}
		if m != nil {
			res["manifest"] = m
		}
		w.finish(res)
	}()
	return map[string]any{"ok": true}, nil
}

func (w *wizard) uninstall(r *http.Request) (any, error) {
	var in struct{ DeleteKey bool }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return nil, err
	}
	if err := w.begin(); err != nil {
		return nil, err
	}
	go func() {
		warns := w.in.Uninstall(in.DeleteKey)
		w.in.Log("Nemesis is removed. The program folder goes when you close this page.")
		w.mu.Lock()
		w.removed = true
		w.mu.Unlock()
		w.finish(map[string]any{"ok": true, "warnings": warns})
	}()
	return map[string]any{"ok": true}, nil
}

func (w *wizard) progress(r *http.Request) (any, error) {
	since := 0
	fmt.Sscan(r.URL.Query().Get("since"), &since)
	w.mu.Lock()
	defer w.mu.Unlock()
	var lines []string
	if since < len(w.lines) {
		lines = append(lines, w.lines[since:]...)
	}
	return map[string]any{"lines": lines, "next": len(w.lines), "done": w.done, "result": w.result}, nil
}
