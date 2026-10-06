package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis/internal/pad"
	"github.com/MrDentalStone/Nemesis/internal/padobj"
)

// Getting the pad onto a table without the Saved Objects step: a table with
// no pad gets a small question tile (Yes / No, or "yes" in chat) that places
// the pad itself, so the humans decide; an outdated pad gets the script this
// binary carries, keeping its seat and settings (onSave survives reload()).

var reVersion = regexp.MustCompile(`NEMESIS_VERSION = '([^']+)'`)

// PadVersion is the pad version built into this binary.
func PadVersion() string {
	if m := reVersion.FindStringSubmatch(padobj.Lua()); m != nil {
		return m[1]
	}
	return ""
}

// olderThan compares "v0.6.0"-style versions; anything unparsable is old.
func olderThan(have, want string) bool {
	parse := func(v string) []int {
		var out []int
		for _, p := range strings.Split(strings.TrimPrefix(v, "v"), ".") {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil
			}
			out = append(out, n)
		}
		return out
	}
	h, w := parse(have), parse(want)
	if h == nil {
		return w != nil
	}
	for i := 0; i < len(w); i++ {
		if i >= len(h) {
			return true
		}
		if h[i] != w[i] {
			return h[i] < w[i]
		}
	}
	return false
}

const askTileLua = `nemesisSetupTile = true
local NAME = %q
local PAD = [=====[%s]=====]
local done = false

function onLoad()
  local s = self.getScale()
  local function b(lbl, fn, x, z, w, col, fs)
    self.createButton({ click_function = fn, function_owner = self, label = lbl,
      position = { x / s.x, 0.6, z / s.z }, scale = { 1 / s.x, 1, 1 / s.z },
      width = w, height = w > 0 and 260 or 0, font_size = fs or 110,
      color = col or { 0, 0, 0, 0 }, font_color = { 1, 1, 1 } })
  end
  b('Place ' .. NAME .. "'s pad here?", 'noop', 0, -0.55, 0, nil, 120)
  b('Yes', 'onYes', -0.9, 0.35, 700, { 0.15, 0.5, 0.2 })
  b('No', 'onNo', 0.9, 0.35, 700, { 0.6, 0.15, 0.15 })
  broadcastToAll('[' .. NAME .. '] Hi! I am ' .. NAME .. ', an AI player. May I put my pad on the table? ' ..
    'Press Yes on the tile in the middle, or type "yes" in chat.', { 1, 0.85, 0.4 })
end

function noop() end

function place(by)
  if done then return end
  done = true
  local p = self.getPosition()
  spawnObjectJSON({ json = PAD, position = { p.x, p.y + 0.3, p.z }, rotation = { 0, self.getRotation().y, 0 } })
  broadcastToAll('[' .. NAME .. '] Thanks' .. (by and (', ' .. by) or '') .. '! Pick a free colour for me with < > on the pad.', { 1, 0.85, 0.4 })
  self.destruct()
end

function decline()
  if done then return end
  done = true
  broadcastToAll('[' .. NAME .. '] OK, no pad for now.', { 0.8, 0.8, 0.8 })
  self.destruct()
end

function onYes(_, color) place(Player[color] and Player[color].steam_name) end
function onNo() decline() end

function onChat(message, player)
  local w = (message or ''):lower():match('^%%s*(%%a+)')
  if w == 'yes' or w == 'yeah' or w == 'yep' or w == 'sure' or w == 'ok' or w == 'okay' then place(player.steam_name)
  elseif w == 'no' or w == 'nope' or w == 'nah' then decline() end
end
`

// askTile is the Lua that spawns the question tile at the table centre,
// unless one is already out.
func askTile(name string) (string, error) {
	padJSON, err := json.Marshal(padobj.Object(padobj.Lua(), 0, 1, 0, 0))
	if err != nil {
		return "", err
	}
	tile := fmt.Sprintf(askTileLua, name, padJSON)
	if strings.Contains(string(padJSON), "]=====]") || strings.Contains(tile, "]======]") {
		return "", errors.New("pad script contains a Lua long-bracket delimiter")
	}
	return `
for _, o in ipairs(getAllObjects()) do
  if o.getVar('nemesisSetupTile') == true then return JSON.encode({asked = 'already'}) end
end
spawnObjectData({ data = {
  Name = 'BlockSquare', Nickname = 'Nemesis: place the AI pad?', Locked = true,
  Transform = { posX = 0, posY = 1.6, posZ = 0, rotX = 0, rotY = 0, rotZ = 0, scaleX = 4, scaleY = 0.2, scaleZ = 2 },
  ColorDiffuse = { r = 0.12, g = 0.10, b = 0.16 },
  LuaScript = [======[` + tile + `]======],
} })
return JSON.encode({asked = 'yes'})`, nil
}

// updateLua swaps the pad's script for this binary's and reloads it.
func updateLua(guid string) (string, error) {
	src := padobj.Lua()
	if strings.Contains(src, "]======]") {
		return "", errors.New("pad script contains a Lua long-bracket delimiter")
	}
	return fmt.Sprintf(`
local pad = getObjectFromGUID('%s')
if pad == nil then return JSON.encode({error = 'pad gone'}) end
pad.setLuaScript([======[%s]======])
pad.reload()
return JSON.encode({updated = true})`, guid, src), nil
}

var autoUpdated = struct {
	sync.Mutex
	tried map[string]bool
}{tried: map[string]bool{}}

// autoUpdate brings an outdated pad up to this server's version the first
// time the server meets it, with one chat line, instead of leaving it to the
// model to notice and ask: the pad is the AI's own seat, and an old one
// silently lacks features. Tried once per pad per run.
func autoUpdate(ctx context.Context, d Deps) string {
	guid, err := d.Pad.GUID(ctx)
	if err != nil {
		return ""
	}
	autoUpdated.Lock()
	done := autoUpdated.tried[guid]
	autoUpdated.tried[guid] = true
	autoUpdated.Unlock()
	if done {
		return ""
	}
	var ping struct {
		Version string `json:"version"`
	}
	_ = d.Pad.Call(ctx, "ping", nil, &ping)
	want := PadVersion()
	if !olderThan(ping.Version, want) {
		return ""
	}
	lua, err := updateLua(guid)
	if err != nil {
		return ""
	}
	if _, err := d.TTS.Exec(ctx, lua); err != nil {
		return fmt.Sprintf("NOTE: the pad is %s and could not be updated to %s (%v).\n", or(ping.Version, "old"), want, err)
	}
	d.Pad.Forget()
	reconfigure()
	// reload() respawns the object; give it a moment before the next call
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	_ = d.Pad.Call(ctx, "say", map[string]any{"text": "Updated my pad to " + want + " (seat and settings kept)."}, nil)
	return fmt.Sprintf("(updated the pad from %s to %s)\n", or(ping.Version, "an old version"), want)
}

func registerSetup(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "setup_pad",
		Description: "Get your pad onto the table. No pad: puts a small Yes/No tile in the middle and asks the table in chat; the pad appears when someone says yes ([pad] event). " +
			"An outdated pad: updates its script in place, keeping seat and settings. A current pad: reports its version.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		want := PadVersion()
		guid, err := d.Pad.GUID(ctx)
		if errors.Is(err, pad.ErrNoPad) {
			lua, err := askTile(d.Name)
			if err != nil {
				return nil, nil, err
			}
			var out struct {
				Asked string `json:"asked"`
			}
			if err := d.TTS.ExecJSON(ctx, lua, &out); err != nil {
				return nil, nil, err
			}
			if out.Asked == "already" {
				return text("the question tile is already on the table; wait for someone to answer ([pad] event)"), nil, nil
			}
			return text("asked the table; the pad appears when someone says yes ([pad] event), then the table picks your colour on it"), nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		var ping struct {
			Version string `json:"version"`
		}
		_ = d.Pad.Call(ctx, "ping", nil, &ping) // a very old pad may not answer
		if !olderThan(ping.Version, want) {
			return text("pad " + ping.Version + " is on the table and up to date"), nil, nil
		}
		lua, err := updateLua(guid)
		if err != nil {
			return nil, nil, err
		}
		if _, err := d.TTS.Exec(ctx, lua); err != nil {
			return nil, nil, err
		}
		d.Pad.Forget()
		reconfigure()
		return text(fmt.Sprintf("updated the pad from %s to %s; seat and settings kept", or(ping.Version, "an old version"), want)), nil, nil
	})
}
