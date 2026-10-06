// Package ttsapi talks to Tabletop Simulator's External Editor API.
//
// TTS listens on 127.0.0.1:39999 for editor messages and connects back to
// 127.0.0.1:39998 to push its own. Every message, in both directions, is one
// JSON object on a fresh TCP connection that the sender closes afterwards.
//
// The only message we send is "Execute Lua Code" (messageID 3). Its
// returnID field is mandatory: without it TTS runs the script but never
// reports the return value. TTS pushes return values (5), custom messages
// from sendExternalMessage (4), print output (2), script errors (3) and
// "game loaded" (1).
package ttsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Message is the wire shape in both directions; fields are a union.
type Message struct {
	MessageID          int             `json:"messageID"`
	ReturnID           int             `json:"returnID,omitempty"`
	GUID               string          `json:"guid,omitempty"`
	Script             string          `json:"script,omitempty"`
	ReturnValue        json.RawMessage `json:"returnValue,omitempty"`
	CustomMessage      json.RawMessage `json:"customMessage,omitempty"`
	Message            string          `json:"message,omitempty"`
	Error              string          `json:"error,omitempty"`
	ErrorMessagePrefix string          `json:"errorMessagePrefix,omitempty"`
}

const (
	msgLoadingGame = 1
	msgPrint       = 2
	msgError       = 3
	msgCustom      = 4
	msgReturn      = 5
)

// LogEntry is one captured print or error line from TTS.
type LogEntry struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // "print" | "error" | "info"
	GUID string    `json:"guid,omitempty"`
	Text string    `json:"text"`
}

// Client drives one TTS instance.
type Client struct {
	ttsAddr string
	ln      net.Listener
	Timeout time.Duration

	execMu  sync.Mutex // one script in flight at a time, so errors attribute cleanly
	mu      sync.Mutex
	nextID  int
	pending map[int]chan Message
	errCh   chan string // Lua errors seen while a script is pending

	logMu sync.Mutex
	log   []LogEntry

	curMu     sync.Mutex
	cursor    Cursor
	curLoaded bool

	// Events receives every sendExternalMessage payload. The buffer is
	// generous; when it is full the newest event is dropped and logged.
	Events chan json.RawMessage

	reloadMu sync.Mutex
	onReload []func()

	speaker atomic.Pointer[string] // the AI's chat prefix, e.g. "[Kestrel]"

	closed chan struct{}
}

// Listen binds the push listener and returns a client. ttsAddr is where TTS
// listens (127.0.0.1:39999); listenAddr is where TTS pushes to
// (127.0.0.1:39998 — TTS hard-codes it, so tests are the only reason to vary it).
func Listen(ttsAddr, listenAddr string) (*Client, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w (is the Atom/VS Code TTS plugin running? only one editor can listen)", listenAddr, err)
	}
	c := &Client{
		ttsAddr: ttsAddr,
		ln:      ln,
		Timeout: 10 * time.Second,
		pending: map[int]chan Message{},
		errCh:   make(chan string, 8),
		Events:  make(chan json.RawMessage, 256),
		closed:  make(chan struct{}),
	}
	go c.accept()
	return c, nil
}

// ListenAddr is the bound push address (useful when listenAddr had port 0).
func (c *Client) ListenAddr() string { return c.ln.Addr().String() }

// Close stops the listener.
func (c *Client) Close() error {
	select {
	case <-c.closed:
		return nil
	default:
	}
	close(c.closed)
	return c.ln.Close()
}

// SetSpeaker names the AI at the table so its own chat lines ("[Name] ...")
// are not echoed back as table news.
func (c *Client) SetSpeaker(name string) {
	p := "[" + name + "]"
	c.speaker.Store(&p)
}

// OnReload registers a callback for "game loaded" pushes (object GUIDs change).
func (c *Client) OnReload(fn func()) {
	c.reloadMu.Lock()
	c.onReload = append(c.onReload, fn)
	c.reloadMu.Unlock()
}

func (c *Client) accept() {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			select {
			case <-c.closed:
				return
			default:
				c.addLog("info", "", "accept: "+err.Error())
				continue
			}
		}
		go c.handle(conn)
	}
}

func (c *Client) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, err := io.ReadAll(io.LimitReader(conn, 64<<20))
	if err != nil {
		c.addLog("info", "", "read push: "+err.Error())
		return
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		c.addLog("info", "", "bad push json: "+err.Error())
		return
	}
	switch m.MessageID {
	case msgReturn:
		c.mu.Lock()
		ch, ok := c.pending[m.ReturnID]
		c.mu.Unlock()
		if ok {
			ch <- m
		}
	case msgCustom:
		if c.seenBefore(m.CustomMessage) {
			break // already delivered through a drain
		}
		select {
		case c.Events <- m.CustomMessage:
		default:
			c.addLog("info", "", "event buffer full; dropped an event")
		}
	case msgPrint:
		c.addLog("print", "", m.Message)
		c.forwardLog("print", m.Message)
	case msgError:
		c.addLog("error", m.GUID, strings.TrimSpace(m.ErrorMessagePrefix+m.Error))
		c.forwardLog("error", strings.TrimSpace(m.ErrorMessagePrefix+m.Error))
		select {
		case c.errCh <- m.Error:
		default:
		}
	case msgLoadingGame:
		c.addLog("info", "", "game (re)loaded")
		c.reloadMu.Lock()
		fns := append([]func(){}, c.onReload...)
		c.reloadMu.Unlock()
		for _, fn := range fns {
			fn()
		}
	}
}

// forwardLog turns table output into an event so wait_for_event sees what the
// humans see in chat: life-tracker lines ("Blue gained 5 life |49|"), commander
// damage, the table's own announcements, and Lua errors. The AI's own lines
// are left out, they are not news.
func (c *Client) forwardLog(kind, text string) {
	if sp := c.speaker.Load(); sp != nil && strings.HasPrefix(text, *sp) {
		return
	}
	b, err := json.Marshal(map[string]any{"ev": "log", "kind": kind, "text": text, "t": time.Now().Unix()})
	if err != nil {
		return
	}
	select {
	case c.Events <- b:
	default:
	}
}

// Cursor is how far into the pad's event ring this listener has seen:
// the pad's load epoch and the last sequence number. It is persisted so a
// fresh process (the CLI, or the MCP server after a restart) can ask the pad
// for everything that happened in the gap.
type Cursor struct {
	Epoch int64 `json:"epoch"`
	Seq   int64 `json:"seq"`
}

func cursorPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "nemesis", "event-cursor.json")
}

// LoadCursor reads the persisted cursor (zero value if none).
func (c *Client) LoadCursor() Cursor {
	c.curMu.Lock()
	defer c.curMu.Unlock()
	if c.curLoaded {
		return c.cursor
	}
	c.curLoaded = true
	if b, err := os.ReadFile(cursorPath()); err == nil {
		_ = json.Unmarshal(b, &c.cursor)
	}
	return c.cursor
}

// Advance moves the cursor forward (a new epoch always wins) and persists it.
func (c *Client) Advance(epoch, seq int64) {
	c.LoadCursor()
	c.curMu.Lock()
	defer c.curMu.Unlock()
	if epoch == c.cursor.Epoch && seq <= c.cursor.Seq {
		return
	}
	c.cursor = Cursor{Epoch: epoch, Seq: seq}
	b, _ := json.Marshal(c.cursor)
	_ = os.MkdirAll(filepath.Dir(cursorPath()), 0o755)
	_ = os.WriteFile(cursorPath(), b, 0o644)
}

// seenBefore reports whether a pushed event is at or behind the cursor
// (delivered already by a drain); otherwise it advances the cursor.
func (c *Client) seenBefore(raw json.RawMessage) bool {
	var h struct {
		Epoch float64 `json:"epoch"` // Lua numbers arrive as floats
		Seq   float64 `json:"seq"`
	}
	if err := json.Unmarshal(raw, &h); err != nil || h.Seq == 0 {
		return false // unnumbered event (old pad): always deliver
	}
	epoch, seq := int64(h.Epoch), int64(h.Seq)
	cur := c.LoadCursor()
	if epoch == cur.Epoch && seq <= cur.Seq {
		return true
	}
	c.Advance(epoch, seq)
	return false
}

// Inject delivers an event fetched from the pad's ring as if it had been pushed.
func (c *Client) Inject(raw json.RawMessage) {
	select {
	case c.Events <- raw:
	default:
		c.addLog("info", "", "event buffer full; dropped a drained event")
	}
}

func (c *Client) addLog(kind, guid, text string) {
	c.logMu.Lock()
	c.log = append(c.log, LogEntry{Time: time.Now(), Kind: kind, GUID: guid, Text: text})
	if len(c.log) > 500 {
		c.log = c.log[len(c.log)-500:]
	}
	c.logMu.Unlock()
}

// Log returns the most recent n captured lines, oldest first.
func (c *Client) Log(n int) []LogEntry {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	if n <= 0 || n > len(c.log) {
		n = len(c.log)
	}
	out := make([]LogEntry, n)
	copy(out, c.log[len(c.log)-n:])
	return out
}

// ErrLua is returned when TTS reports a script error for the executed code.
var ErrLua = errors.New("lua error")

// Exec runs lua in the Global script context and returns the raw JSON of its
// return value (TTS JSON-encodes strings, numbers, booleans and tables).
// A script that returns nothing yields a timeout, so always `return` something.
func (c *Client) Exec(ctx context.Context, lua string) (json.RawMessage, error) {
	c.execMu.Lock()
	defer c.execMu.Unlock()

	// Drain stale errors so an old one can't be pinned on this script.
	for len(c.errCh) > 0 {
		<-c.errCh
	}

	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	body, err := json.Marshal(Message{MessageID: 3, ReturnID: id, GUID: "-1", Script: lua})
	if err != nil {
		return nil, err
	}
	if err := c.send(ctx, body); err != nil {
		return nil, err
	}

	timeout := c.Timeout
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < timeout {
		timeout = time.Until(dl)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case m := <-ch:
		return m.ReturnValue, nil
	case e := <-c.errCh:
		return nil, fmt.Errorf("%w: %s", ErrLua, e)
	case <-timer.C:
		return nil, fmt.Errorf("tts: no return value within %s (is a table loaded? does the script `return`?)", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ExecJSON runs lua that returns a JSON string (via JSON.encode) and decodes
// it into out. A table return value (already JSON) is accepted too.
func (c *Client) ExecJSON(ctx context.Context, lua string, out any) error {
	raw, err := c.Exec(ctx, lua)
	if err != nil {
		return err
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		raw = json.RawMessage(s)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode lua result %q: %w", truncate(string(raw), 200), err)
	}
	return nil
}

func (c *Client) send(ctx context.Context, body []byte) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.ttsAddr)
	if err != nil {
		return fmt.Errorf("tts not reachable at %s (is Tabletop Simulator running?): %w", c.ttsAddr, err)
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write(body)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
