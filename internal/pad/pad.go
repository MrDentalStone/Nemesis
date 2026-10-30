// Package pad calls named operations on the in-game Nemesis pad. The pad's
// api() is the fairness boundary; this package only serialises requests and
// decodes replies.
package pad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/MrDentalStone/Nemesis/internal/cards"
	"github.com/MrDentalStone/Nemesis/internal/ttsapi"
)

// Pad is a handle to the pad object; the GUID is discovered lazily and
// forgotten when the game reloads.
type Pad struct {
	tts     *ttsapi.Client
	mu      sync.Mutex
	guid    string
	drainMu sync.Mutex // one catch-up at a time, or two callers inject the same events
}

func New(c *ttsapi.Client) *Pad {
	p := &Pad{tts: c}
	c.OnReload(p.Forget)
	return p
}

// Forget drops the cached GUID so the next call rescans the table.
func (p *Pad) Forget() {
	p.mu.Lock()
	p.guid = ""
	p.mu.Unlock()
}

// ErrNoPad means no object with nemesisPad=true exists on the table.
var ErrNoPad = errors.New("no Nemesis pad on the table: call setup_pad (it asks the table, then places one), or spawn it from Objects > Saved Objects")

// GUID of the pad on the table (cached until a reload).
func (p *Pad) GUID(ctx context.Context) (string, error) { return p.find(ctx) }

func (p *Pad) find(ctx context.Context) (string, error) {
	p.mu.Lock()
	g := p.guid
	p.mu.Unlock()
	if g != "" {
		return g, nil
	}
	var out struct {
		GUID string `json:"guid"`
	}
	err := p.tts.ExecJSON(ctx, `
for _, o in ipairs(getAllObjects()) do
  if o.getVar('nemesisPad') == true then return JSON.encode({guid=o.getGUID()}) end
end
return JSON.encode({guid=''})`, &out)
	if err != nil {
		return "", err
	}
	if out.GUID == "" {
		return "", ErrNoPad
	}
	p.mu.Lock()
	p.guid = out.GUID
	p.mu.Unlock()
	return out.GUID, nil
}

// Call runs one pad op and decodes the reply into out. The request travels
// as a JSON string inside a Lua long bracket so no Lua quoting is needed.
func (p *Pad) Call(ctx context.Context, op string, params map[string]any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	params["op"] = op
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if strings.Contains(string(body), "]==]") {
		return errors.New("request contains the ]==] delimiter")
	}
	err = p.call(ctx, op, body, out)
	if errors.Is(err, errPadGone) {
		// the cached pad was deleted or respawned: rescan once
		p.Forget()
		err = p.call(ctx, op, body, out)
	}
	return err
}

var errPadGone = errors.New("pad gone")

func (p *Pad) call(ctx context.Context, op string, body []byte, out any) error {
	guid, err := p.find(ctx)
	if err != nil {
		return err
	}
	lua := fmt.Sprintf(`
local pad = getObjectFromGUID('%s')
if pad == nil then return JSON.encode({error='pad gone'}) end
return pad.call('api', {json=[==[%s]==]})`, guid, body)

	var probe struct {
		Error string `json:"error"`
	}
	raw, err := p.tts.Exec(ctx, lua)
	if err != nil {
		return err
	}
	// pad.call returns a Lua string, which TTS JSON-encodes once more.
	var s string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
	} else {
		s = string(raw)
	}
	if err := json.Unmarshal([]byte(s), &probe); err == nil && probe.Error != "" {
		if probe.Error == "pad gone" {
			p.Forget()
			return fmt.Errorf("pad %s: %w", op, errPadGone)
		}
		return fmt.Errorf("pad %s: %s", op, probe.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(s), out); err != nil {
		return fmt.Errorf("decode %s reply: %w (%s)", op, err, trunc(s, 200))
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- typed replies -------------------------------------------------------

type RawCard struct {
	GUID     string `json:"guid"`
	Nick     string `json:"nick"`
	Desc     string `json:"desc"`
	FaceDown bool   `json:"faceDown"`
	InPile   string `json:"inPile,omitempty"`
	Hand     int    `json:"hand,omitempty"`
	Zone     string `json:"zone,omitempty"` // hand | secondary | tokens
}

// Card is a parsed card with its table identity.
type Card struct {
	GUID string `json:"guid"`
	cards.Card
	FaceDown bool `json:"faceDown,omitempty"`
	Hand     int  `json:"hand,omitempty"`
}

func (r RawCard) Parse() Card {
	return Card{GUID: r.GUID, Card: cards.Parse(r.Nick, r.Desc), FaceDown: r.FaceDown, Hand: r.Hand}
}

type Pos struct{ X, Y, Z float64 }

type Permanent struct {
	GUID     string `json:"guid"`
	Type     string `json:"type"`
	Nick     string `json:"nick"`
	Desc     string `json:"desc"`
	Pos      Pos    `json:"pos"`
	Tapped   bool   `json:"tapped"`
	FaceDown bool   `json:"faceDown"`
	Row      string `json:"row"`
	Count    int    `json:"count"`
	// Counters are the tiles/tokens/dice physically resting on this card:
	// +X/+Y tiles (Name is the label), keyword tokens, dice (Value is the face).
	Counters []Counter `json:"counters"`
	// Enc is the on-card state kept by the table's Encoder modules (what the
	// right-click buttons edit): +1/+1 counters, loyalty, keywords, notes...
	Enc *EncInfo `json:"enc,omitempty"`
}

// EncInfo mirrors the pad's encoderInfo(): only non-default fields are set.
type EncInfo struct {
	PlusOne       int      `json:"plusOne,omitempty"`
	Counters      int      `json:"counters,omitempty"`
	Loyalty       *int     `json:"loyalty,omitempty"`
	Power         int      `json:"power,omitempty"`
	Toughness     int      `json:"toughness,omitempty"`
	BasePower     *int     `json:"basePower,omitempty"`
	BaseToughness *int     `json:"baseToughness,omitempty"`
	PiCounter     int      `json:"piCounter,omitempty"`
	Keywords      []string `json:"keywords,omitempty"`
	Note          string   `json:"note,omitempty"`
	Phased        bool     `json:"phased,omitempty"`
	Token         bool     `json:"token,omitempty"`
}

type Counter struct {
	GUID  string `json:"guid"`
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

type CommanderDamage struct {
	From   string `json:"from"`
	Amount *int   `json:"amount"`
}

type Seat struct {
	Color           string            `json:"color"`
	Seated          bool              `json:"seated"`
	Name            string            `json:"name"`
	IsClaude        bool              `json:"isClaude"`
	Life            *int              `json:"life"`
	HandCount       int               `json:"handCount"`
	Hand2Count      int               `json:"hand2Count"`
	TokenZoneCount  int               `json:"tokenZoneCount"`
	PlayerCounters  map[string]int    `json:"playerCounters"` // poison, energy, experience (tables with those tiles)
	LibraryCount    int               `json:"libraryCount"`
	Graveyard       []RawCard         `json:"graveyard"`
	Exile           []RawCard         `json:"exile"`
	Command         []RawCard         `json:"command"`
	Battlefield     []Permanent       `json:"battlefield"`
	CommanderDamage []CommanderDamage `json:"commanderDamage"`
}

type Snapshot struct {
	Seats []Seat `json:"seats"`
	Turns struct {
		Enabled    bool   `json:"enabled"`
		Active     string `json:"active"`
		Next       string `json:"next"`
		ClaudeTurn bool   `json:"claudeTurn"`
	} `json:"turns"`
	Pad struct {
		Seat         string          `json:"seat"`
		Stopped      bool            `json:"stopped"`
		Pending      json.RawMessage `json:"pending"`
		Version      string          `json:"version"`
		Mode         string          `json:"mode"`
		ModeName     string          `json:"modeName"`
		NewPlayers   []string        `json:"newPlayers"`
		Round        *Round          `json:"round"`
		DecisionPads bool            `json:"decisionPads"`
	} `json:"pad"`
}

// Round is an open question to the table (ops.ask).
type Round struct {
	ID        int             `json:"id"`
	Kind      string          `json:"kind"`
	Text      string          `json:"text"`
	Asked     []string        `json:"asked"`
	WaitingOn []string        `json:"waitingOn"`
	Answers   json.RawMessage `json:"answers"` // Lua encodes an empty table as []
}

// Drain fetches the events the pad buffered past our cursor and feeds them
// into the TTS event channel, so wait_for_event sees chat and button presses
// that happened while nothing was listening. Returns how many were injected.
func (p *Pad) Drain(ctx context.Context) (int, error) {
	p.drainMu.Lock()
	defer p.drainMu.Unlock()
	cur := p.tts.LoadCursor()
	var out struct {
		Epoch  float64           `json:"epoch"` // Lua numbers arrive as floats
		Seq    float64           `json:"seq"`
		Events []json.RawMessage `json:"events"`
	}
	if err := p.Call(ctx, "events", map[string]any{"epoch": cur.Epoch, "since": cur.Seq}, &out); err != nil {
		return 0, err
	}
	epoch := int64(out.Epoch)
	n := 0
	for _, ev := range out.Events {
		var h struct {
			Seq float64 `json:"seq"`
		}
		_ = json.Unmarshal(ev, &h)
		seq := int64(h.Seq)
		if epoch == cur.Epoch && seq <= cur.Seq {
			continue
		}
		p.tts.Inject(ev)
		n++
	}
	// The live pad is the authority: set the cursor to its epoch even if a
	// stale emitter (an older load's timer) had pushed it elsewhere, keeping
	// any live event that arrived during the drain.
	last := int64(out.Seq)
	if now := p.tts.LoadCursor(); now.Epoch == epoch && now.Seq > last {
		last = now.Seq
	}
	p.tts.SetCursor(epoch, last)
	return n, nil
}

func (p *Pad) Snapshot(ctx context.Context) (*Snapshot, error) {
	var s Snapshot
	if err := p.Call(ctx, "snapshot", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

type HandReply struct {
	Seat  string    `json:"seat"`
	Cards []RawCard `json:"cards"`
}

// WindowReply is your own battlefield (no on-card state) and hand: what a
// response window needs, in one light call.
type WindowReply struct {
	Seat        string      `json:"seat"`
	Battlefield []Permanent `json:"battlefield"`
	Cards       []RawCard   `json:"cards"`
}

// Window reads your battlefield and hand in one call. A pad older than
// v1.2.0 has no window op: the caller falls back to Snapshot and Hand.
func (p *Pad) Window(ctx context.Context) (*WindowReply, error) {
	var w WindowReply
	if err := p.Call(ctx, "window", nil, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

func (p *Pad) Hand(ctx context.Context) (*HandReply, error) {
	var h HandReply
	if err := p.Call(ctx, "hand", nil, &h); err != nil {
		return nil, err
	}
	return &h, nil
}
