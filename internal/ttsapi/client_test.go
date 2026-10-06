package ttsapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeTTS accepts executeLua messages and replies the way TTS does: by
// dialling the client's push port with a fresh connection per message.
type fakeTTS struct {
	ln     net.Listener
	pushTo string
	// script → reply builder; return nil to send nothing
	reply func(m Message) []Message
}

func newFakeTTS(t *testing.T) *fakeTTS {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTTS{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var m Message
				json.NewDecoder(conn).Decode(&m)
				for _, r := range f.reply(m) {
					f.push(r)
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeTTS) push(m Message) {
	c, err := net.Dial("tcp", f.pushTo)
	if err != nil {
		return
	}
	json.NewEncoder(c).Encode(m)
	c.Close()
}

func setup(t *testing.T, reply func(Message) []Message) (*Client, *fakeTTS) {
	f := newFakeTTS(t)
	f.reply = reply
	c, err := Listen(f.ln.Addr().String(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.Timeout = 2 * time.Second
	f.pushTo = c.ListenAddr()
	t.Cleanup(func() { c.Close() })
	return c, f
}

func TestExecReturnsValueByReturnID(t *testing.T) {
	c, _ := setup(t, func(m Message) []Message {
		if m.MessageID != 3 || m.GUID != "-1" || m.ReturnID == 0 {
			t.Errorf("bad exec message: %+v", m)
		}
		return []Message{
			{MessageID: 2, Message: "noise"},
			{MessageID: 5, ReturnID: m.ReturnID + 99, ReturnValue: json.RawMessage(`"wrong"`)},
			{MessageID: 5, ReturnID: m.ReturnID, ReturnValue: json.RawMessage(`"{\"n\":3}"`)},
		}
	})
	var out struct{ N int }
	if err := c.ExecJSON(context.Background(), "return JSON.encode({n=3})", &out); err != nil {
		t.Fatal(err)
	}
	if out.N != 3 {
		t.Fatalf("got %+v", out)
	}
	if logs := c.Log(0); len(logs) != 1 || logs[0].Text != "noise" || logs[0].Kind != "print" {
		t.Fatalf("log = %+v", logs)
	}
}

func TestExecTableReturnValue(t *testing.T) {
	c, _ := setup(t, func(m Message) []Message {
		return []Message{{MessageID: 5, ReturnID: m.ReturnID, ReturnValue: json.RawMessage(`{"a":true}`)}}
	})
	var out map[string]bool
	if err := c.ExecJSON(context.Background(), "return {a=true}", &out); err != nil {
		t.Fatal(err)
	}
	if !out["a"] {
		t.Fatal("expected a=true")
	}
}

func TestExecLuaErrorSurfaces(t *testing.T) {
	c, _ := setup(t, func(m Message) []Message {
		return []Message{{MessageID: 3, GUID: "-1", ErrorMessagePrefix: "Error in Script (Global): ", Error: "attempt to index a nil value"}}
	})
	_, err := c.Exec(context.Background(), "nil.x = 1")
	if !errors.Is(err, ErrLua) || !strings.Contains(err.Error(), "nil value") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecTimeoutWhenNoReturn(t *testing.T) {
	c, _ := setup(t, func(m Message) []Message { return nil })
	c.Timeout = 150 * time.Millisecond
	_, err := c.Exec(context.Background(), "print('x')")
	if err == nil || !strings.Contains(err.Error(), "no return value") {
		t.Fatalf("err = %v", err)
	}
}

func TestCustomMessagesBecomeEvents(t *testing.T) {
	c, f := setup(t, func(m Message) []Message { return nil })
	f.push(Message{MessageID: 4, CustomMessage: json.RawMessage(`{"ev":"chat","text":"hi"}`)})
	select {
	case ev := <-c.Events:
		if !strings.Contains(string(ev), `"chat"`) {
			t.Fatalf("event = %s", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

func TestReloadCallback(t *testing.T) {
	c, f := setup(t, func(m Message) []Message { return nil })
	done := make(chan struct{})
	c.OnReload(func() { close(done) })
	f.push(Message{MessageID: 1})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload callback not called")
	}
}

func TestUnreachableTTS(t *testing.T) {
	c, err := Listen("127.0.0.1:1", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Exec(context.Background(), "return 1")
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("err = %v", err)
	}
}
