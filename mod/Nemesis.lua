-- Nemesis: the pad that lets an AI player sit at this table.
-- MIT licensed, https://github.com/MrDentalStone/Nemesis
--
-- The Go side (nemesis-mcp) runs Lua through the External Editor API and
-- every call lands in api() below. This file is the ONLY code that can see
-- hidden information, so the fairness rules live here:
--   * hands: only the AI's own seat is ever listed; others are counts
--   * libraries: never listed, only counted (look{} is the one exception
--     and it only ever touches the AI's own library)
--   * face-down cards on other battlefields are reported as face-down
-- Nothing in api() takes a GUID of an object it hasn't itself handed out,
-- and nothing here is reachable from chat.

nemesisPad = true          -- marker the Go side scans for
NEMESIS_VERSION = 'v1.0.0'

-- the deck importer the MTG tables ship with; nemesis-mcp passes its own URL
local IMPORTER = 'https://importer.rikrassen.com'
local COLORS = { 'White', 'Red', 'Yellow', 'Blue' }
-- every TTS seat colour in turn order; the table's own list is a subset
local CANON = { 'White', 'Brown', 'Red', 'Orange', 'Yellow', 'Green', 'Teal', 'Blue', 'Purple', 'Pink' }
local ZONE_KEYS = { 'libraryZone', 'graveyard', 'exileZone', 'commandZone', 'playmat', 'landZone' }

local state = { seat = nil, stopped = false, pending = nil }
-- The AI's table name: what the humans see in chat and on the pads.
-- nemesis-mcp sets it (and the play modes) with the configure op.
local NAME = 'Kestrel'
local statusText = 'No seat. Use < > to pick one for ' .. NAME .. '.'

---------------------------------------------------------------------------
-- lifecycle
---------------------------------------------------------------------------

function onLoad(saved)
  if saved ~= nil and saved ~= '' then
    local ok, s = pcall(function() return JSON.decode(saved) end)
    if ok and type(s) == 'table' then
      state.seat = s.seat
      state.stopped = s.stopped == true
      state.claudeTurn = s.claudeTurn == true
      if type(s.cmdr) == 'table' then state.cmdr = s.cmdr end
      if type(s.mode) == 'string' then state.mode = s.mode end
      if type(s.newPlayers) == 'table' then state.newPlayers = s.newPlayers end
      if type(s.name) == 'string' and s.name ~= '' then NAME = s.name end
      if type(s.modes) == 'table' and #s.modes > 0 then MODES = s.modes end
      if type(s.roundWait) == 'number' then state.roundWait = s.roundWait end
    end
  end
  pcall(refreshColors)
  if state.seat then statusText = NAME .. ' is ' .. state.seat end
  if state.claudeTurn then Turns.enable = false; statusText = NAME .. "'s turn" end
  buildControls()
  emit({ ev = 'pad', what = 'loaded', seat = state.seat })
  Wait.time(function() pcall(seedField) end, 1)
  Wait.time(function() pcall(pollTaps) end, 1.5, -1)
end

function onSave()
  return JSON.encode({ seat = state.seat, stopped = state.stopped, claudeTurn = state.claudeTurn == true, cmdr = state.cmdr,
    mode = state.mode, newPlayers = state.newPlayers, name = NAME, modes = MODES, roundWait = state.roundWait })
end

---------------------------------------------------------------------------
-- outbound events (-> nemesis-mcp on :39998)
---------------------------------------------------------------------------

-- Every event gets an epoch (this pad load) and a sequence number and is kept
-- in a ring, so a listener that was not running at the time (the MCP server
-- between sessions, the CLI between calls) can fetch what it missed with the
-- events op instead of losing chat and button presses.
ops = ops or {}
state.evEpoch = math.floor(os.time())
state.evSeq = 0
state.evBuf = {}
local EV_RING = 300

function emit(ev)
  ev.t = Time.time
  state.evSeq = state.evSeq + 1
  ev.seq = state.evSeq
  ev.epoch = state.evEpoch
  state.evBuf[#state.evBuf + 1] = ev
  if #state.evBuf > EV_RING then table.remove(state.evBuf, 1) end
  sendExternalMessage(ev)
end

-- {epoch=, since=}: events after `since` from this epoch; everything buffered
-- if the epoch differs (the pad was reloaded, the listener is stale)
function ops.events(req)
  local sameEpoch = tonumber(req.epoch) == state.evEpoch
  local since = sameEpoch and (tonumber(req.since) or 0) or 0
  local out = {}
  for _, ev in ipairs(state.evBuf) do
    if ev.seq > since then out[#out + 1] = ev end
  end
  return { epoch = state.evEpoch, seq = state.evSeq, events = out }
end

function onChat(message, player)
  emit({ ev = 'chat', color = player.color, name = player.steam_name, text = message })
  chatAnswer(player.color, message)
end

-- Turns. TTS's turn system bounces the marker straight off a colour with no
-- seated player, so Claude's turn is virtual: the pad freezes the marker
-- (Turns.enable=false) while Claude plays and hands it to the next seated
-- player on end_turn. Seat order follows the table's colour order.
local function seatIndex(color)
  for i, c in ipairs(COLORS) do if c == color then return i end end
  return 1
end

-- next/previous SEATED colour after/before Claude's slot
function neighbourSeat(dir)
  local i = seatIndex(state.seat)
  for step = 1, #COLORS do
    local c = COLORS[((i - 1 + dir * step) % #COLORS) + 1]
    if Player[c].seated then return c end
  end
  return nil
end

function startClaudeTurn(prev)
  if state.seat == nil then return end
  state.claudeTurn = true
  Turns.enable = false
  broadcastToAll("[Nemesis] " .. NAME .. "'s turn (" .. state.seat .. ")", Color.fromString(state.seat))
  setStatus(NAME .. "'s turn")
  emit({ ev = 'turn', color = state.seat, prev = prev, claude = true })
end

-- Did a turn passing from `from` to `to` (seat order) go over Claude's slot?
-- from == to (one human at the table) goes all the way round, so yes.
function passesClaude(from, to)
  if state.seat == nil or from == nil then return false end
  local i = seatIndex(from)
  for step = 1, #COLORS do
    local c = COLORS[((i - 1 + step) % #COLORS) + 1]
    if c == state.seat then return true end
    if c == to then return false end
  end
  return false
end

function onPlayerTurn(player, previous)
  -- TTS calls this with player=nil when the turn marker is switched off
  -- (which startClaudeTurn does), so there is nothing to intercept then
  if player == nil then return end
  pcall(refreshColors)
  local prev = previous and previous.color or nil
  if state.justEnded and Time.time - state.justEnded < 3 then
    -- our own end_turn re-enabled the marker. TTS first lands it on a stale
    -- colour and only then on the seat we passed to, so stay quiet until it
    -- reaches that seat; the stale hop would otherwise look like a pass over
    -- Claude's slot and restart Claude's turn.
    if player.color == state.passedTo then state.justEnded = nil end
    emit({ ev = 'turn', color = player.color, prev = prev })
    return
  end
  state.justEnded = nil
  if state.seat and not state.claudeTurn and passesClaude(prev, player.color) then
    -- TTS skipped over Claude's slot: this is really Claude's turn
    startClaudeTurn(prev)
    return
  end
  emit({ ev = 'turn', color = player.color, prev = prev })
end

-- The table's own end-turn key (scripting button 0 = index 10) moves the
-- marker with Turns.getNextTurnColor(). With one human seated that hands the
-- turn straight back to them, and TTS fires no onPlayerTurn for a marker that
-- does not change colour, so catch the key itself as well.
function onScriptingButtonDown(index, color)
  if index ~= 10 or state.seat == nil or state.claudeTurn or not Turns.enable then return end
  if color ~= Turns.turn_color then return end
  local from = color
  Wait.frames(function()
    if state.claudeTurn or not Turns.enable then return end
    if Turns.turn_color == from or passesClaude(from, Turns.turn_color) then
      startClaudeTurn(from)
    end
  end, 3)
end

-- Zone traffic caused by Claude's own ops is noise; myObject() stamps every
-- object it hands out so those settle quietly, and cards dealt off the
-- library are stamped as they leave it (dealingUntil). What's left is humans
-- moving Claude's things (killing a creature, bouncing a permanent...). A
-- card in flight crosses several zones (library, mat, graveyard, land box),
-- so nothing is reported until it settles, and then only the net move.
state.touched = {}
state.zoneMoves = {} -- guid -> move in flight: { left = {key=true}, lastIn = key, entered = bool }
state.fromHuman = {} -- guid -> { seat, key, t }: last left a human's hand or zone

local function recentlyTouched(obj)
  local t = state.touched[obj.getGUID()]
  return t ~= nil and (Time.time - t) < 6
end

-- most specific first: a land in the land box is on the mat as well
local ZONE_RANK = { 'landZone', 'graveyard', 'exileZone', 'commandZone', 'libraryZone', 'playmat' }
local function bestKey(set)
  for _, k in ipairs(ZONE_RANK) do if set[k] then return k end end
  return nil
end

local function stampDealt(object, key)
  if key == 'libraryZone' and object.type == 'Card' and Time.time < (state.dealingUntil or 0) then
    state.touched[object.getGUID()] = Time.time
  end
end

local function noteZone(object, dir, key)
  local g = object.getGUID()
  local m = state.zoneMoves[g]
  if m == nil then
    m = { left = {}, name = object.getName():match('^[^\n]*'), type = object.type }
    state.zoneMoves[g] = m
    local function settle()
      state.zoneMoves[g] = nil
      if recentlyTouched(object) then return end
      local final
      if object == nil or object.isDestroyed() then
        -- merged into a pile (a graveyard deck): TTS reports it leaving that
        -- zone as it is destroyed, so go by the last zone it entered
        final = m.lastIn
      else
        local now = {}
        for _, z in ipairs(object.getZones()) do
          local k = myZoneKey(z)
          if k then now[k] = true end
        end
        final = bestKey(now)
        if final == 'playmat' and bestKey(m.left) == nil and object.type == 'Card' and not ownCard(object) then
          local h = state.fromHuman[g]
          if h and Time.time - h.t < 30 then
            dropSettled(h.seat, object) -- a human's card that slipped past watchDrop
            return
          end
        end
      end
      local from = bestKey(m.left)
      if from == final then return end
      local e = { ev = 'zone', guid = g, name = m.name, type = m.type }
      if from and final then
        e.dir, e.from, e.zone = 'move', from, final
      elseif final then
        e.dir, e.zone = 'enter', final
      else
        e.dir, e.zone = 'leave', from
      end
      emit(e)
    end
    Wait.condition(function() Wait.time(settle, 0.4) end, function()
      return object == nil or object.isDestroyed() or (object.held_by_color == nil and object.resting)
    end, 30, function() state.zoneMoves[g] = nil end)
  end
  if dir == 'enter' then
    m.entered = true
    m.lastIn = key
  else
    if not m.entered then m.left[key] = true end -- where it started
  end
end

function onObjectEnterZone(zone, object)
  if object == self then return end -- the pad itself resting on a mat
  if zone and zone.type == 'Hand' then state.fieldOf[object.getGUID()] = nil end
  local seat, key = zoneOwner(zone)
  if seat == nil then return end
  if key == 'graveyard' or key == 'exileZone' or key == 'libraryZone' or key == 'commandZone' then
    state.fieldOf[object.getGUID()] = nil
  end
  if seat == state.seat then
    stampDealt(object, key)
    local by = object.held_by_color
    if by == nil or by == '' then
      -- released above the mat and fell in: who let go of it?
      local h = state.fromHuman[object.getGUID()]
      if h and Time.time - h.t < 10 then by = h.seat end
    end
    if key == 'playmat' and object.type == 'Card' and by and by ~= '' and by ~= state.seat and not ownCard(object) then
      watchDrop(by, object) -- a human put their card on my board: a response or a play aimed at me
      return
    end
    if not recentlyTouched(object) and object.getName() ~= '' then noteZone(object, 'enter', key) end
    return
  end
  watchPlay(seat, key, object)
end

-- one of Claude's own cards: handed out by an op at some point, or seen
-- leaving one of Claude's zones (a human moving it around), and not just
-- taken from a human's hand or zone
function ownCard(obj)
  local g = obj.getGUID()
  if state.touched[g] ~= nil then return true end
  local h = state.fromHuman[g]
  if h and Time.time - h.t < 30 then return false end
  local l = state.lastLeft[g]
  return l ~= nil and l.seat == state.seat
end

function onObjectLeaveZone(zone, object)
  if object == self then return end -- the pad itself resting on a mat
  local seat, key = zoneOwner(zone)
  if seat == nil then
    -- a card leaving a human's hand: remember it, it may be on its way to
    -- the battlefield (hand zones are not in the seat data, so ask Player)
    if zone and zone.type == 'Hand' and object.type == 'Card' then
      local hc, kind = handOwner(zone)
      if hc then
        state.lastLeft[object.getGUID()] = { seat = hc, key = kind, t = Time.time }
        if hc ~= state.seat then state.fromHuman[object.getGUID()] = { seat = hc, key = kind, t = Time.time } end
        pruneLastLeft()
      end
    end
    return
  end
  state.lastLeft[object.getGUID()] = { seat = seat, key = key, t = Time.time }
  if seat == state.seat then
    stampDealt(object, key)
    if not recentlyTouched(object) and object.getName() ~= '' then noteZone(object, 'leave', key) end
    return
  end
  if object.type == 'Card' then state.fromHuman[object.getGUID()] = { seat = seat, key = key, t = Time.time } end
  pruneLastLeft()
end

function pruneLastLeft()
  state.lastLeftN = (state.lastLeftN or 0) + 1
  if state.lastLeftN < 200 then return end
  state.lastLeftN = 0
  for g, e in pairs(state.lastLeft) do if Time.time - e.t > 60 then state.lastLeft[g] = nil end end
  for g, t in pairs(state.playSeen) do if Time.time - t > 60 then state.playSeen[g] = nil end end
  for g, e in pairs(state.fromHuman) do if Time.time - e.t > 60 then state.fromHuman[g] = nil end end
end

-- Which seat's zone is this, and which of its zones (ZONE_KEYS)? Built for
-- every seat and cached: zone events fire for every die and token in play.
local zoneIdx = { t = -10, map = {} }
function zoneOwner(zone)
  if zone == nil then return nil end
  if Time.time - zoneIdx.t > 5 then
    zoneIdx = { t = Time.time, map = {} }
    for _, c in ipairs(COLORS) do
      local ok, d = pcall(seatData, c)
      if ok and d then
        for _, k in ipairs(ZONE_KEYS) do
          if d[k] ~= nil then zoneIdx.map[d[k].getGUID()] = { seat = c, key = k } end
        end
      end
    end
  end
  local e = zoneIdx.map[zone.getGUID()]
  if e == nil then return nil end
  return e.seat, e.key
end

-- which of Claude's zones is this, if any
function myZoneKey(zone)
  local seat, key = zoneOwner(zone)
  if seat ~= nil and seat == state.seat then return key end
  return nil
end

function handOwner(zone)
  for _, c in ipairs(COLORS) do
    for _, h in ipairs(handZones(c)) do
      local ok, tr = pcall(function() return Player[c].getHandTransform(h.idx) end)
      if ok and tr and tr.position then
        local p = zone.getPosition()
        if math.abs(p.x - tr.position.x) < 0.5 and math.abs(p.z - tr.position.z) < 0.5 then
          return c, h.kind == 'tokens' and 'tokens' or 'hand'
        end
      end
    end
  end
  return nil
end

-- once a card dropped on my board by `by` settles: ev=play to='your_board',
-- and it answers an open "any responses?" round
function watchDrop(by, obj)
  Wait.condition(function()
    if obj == nil or obj.isDestroyed() then return end
    for _, z in ipairs(obj.getZones()) do
      if myZoneKey(z) == 'playmat' then dropSettled(by, obj) return end
    end
  end, function()
    return obj == nil or obj.isDestroyed() or (obj.held_by_color == nil and obj.resting)
  end, 30)
end

function dropSettled(by, obj)
  local guid = obj.getGUID()
  local origin = 'hand'
  local h = state.fromHuman[guid]
  if h and h.seat == by and Time.time - h.t < 30 then
    origin = ({ playmat = 'battlefield', landZone = 'battlefield', libraryZone = 'library', commandZone = 'command',
      exileZone = 'exile', graveyard = 'graveyard', tokens = 'hand' })[h.key] or h.key
  end
  local seen = state.playSeen[guid]
  if seen and Time.time - seen < 20 then return end
  state.playSeen[guid] = Time.time
  local face = isFaceDown(obj)
  local pl = Player[by]
  local name = face and '(face-down card)' or obj.getName()
  local desc = face and '' or obj.getDescription()
  emit({ ev = 'play', color = by, player = pl and pl.steam_name or '', guid = guid, from = origin, to = 'your_board',
    faceDown = face, name = name, desc = desc, claudeTurn = state.claudeTurn == true })
  local r = state.round
  if r and r.kind == 'responses' then
    r.answers[by] = 'respond'
    r.pending[by] = nil
    finishRound('respond', by, false, { name = name, desc = desc, guid = guid })
  end
end

---------------------------------------------------------------------------
-- other players' plays (interrupts)
---------------------------------------------------------------------------
-- A card that lands on a human's battlefield, graveyard or exile from
-- anywhere but that battlefield is a play the AI may want to respond to: a
-- spell cast from hand, a commander from the command zone, a land, a
-- reanimation. The pad reports it once the card has settled (dragging a card
-- over the mat and back to hand is not a play) as ev=play. It is public by
-- then: the card is face up on the table, or reported as face-down.
state.lastLeft = {}   -- guid -> { seat, key, t }: where a card was last seen leaving
state.playSeen = {}   -- guid -> time it was last reported
state.fieldOf = {}    -- guid -> seat whose battlefield it was last reported on
local PLAY_TO = { playmat = 'battlefield', landZone = 'battlefield', graveyard = 'graveyard', exileZone = 'exile' }

function watchPlay(seat, key, obj)
  if PLAY_TO[key] == nil or obj.type ~= 'Card' then return end
  local guid = obj.getGUID()
  -- tapping, nudging or lifting a permanent can bounce it out of the zone and
  -- back in; it is still the same permanent, not a new play
  if PLAY_TO[key] == 'battlefield' and state.fieldOf[guid] == seat then return end
  local from = state.lastLeft[guid]
  if from and from.seat == seat and Time.time - from.t < 15 then
    if PLAY_TO[from.key] == 'battlefield' and PLAY_TO[key] == 'battlefield' then return end -- moved on the mat
    if PLAY_TO[from.key] == 'battlefield' then return end -- died, sacrificed, exiled: not a play
    if from.key == key then return end
  else
    from = nil -- from a hand (or spawned): the usual cast
  end
  local seen = state.playSeen[guid]
  if seen and Time.time - seen < 20 then return end
  local origin = from and from.key or 'hand'
  if origin == 'libraryZone' then origin = 'library' elseif origin == 'commandZone' then origin = 'command' elseif origin == 'exileZone' then origin = 'exile' end
  -- wait until nobody holds it and it is at rest, still in the zone
  Wait.condition(function()
    if obj == nil or obj.isDestroyed() then return end
    local still = false
    for _, z in ipairs(obj.getZones()) do
      local s2, k2 = zoneOwner(z)
      if s2 == seat and PLAY_TO[k2] == PLAY_TO[key] then still = true end
    end
    if not still then return end
    local s = state.playSeen[guid]
    if s and Time.time - s < 20 then return end
    state.playSeen[guid] = Time.time
    if PLAY_TO[key] == 'battlefield' then state.fieldOf[guid] = seat end
    local face = isFaceDown(obj)
    local land = false
    local d = seatData(seat)
    if d and d.landZone then
      for _, o in ipairs(d.landZone.getObjects()) do if o.getGUID() == guid then land = true end end
    end
    local pl = Player[seat]
    emit({ ev = 'play', color = seat, player = pl and pl.steam_name or '', guid = guid, from = origin, to = PLAY_TO[key],
      landRow = land, faceDown = face,
      name = face and '(face-down card)' or obj.getName(), desc = face and '' or obj.getDescription(),
      claudeTurn = state.claudeTurn == true })
  end, function()
    return obj == nil or obj.isDestroyed() or (obj.held_by_color == nil and obj.resting)
  end, 30)
end

---------------------------------------------------------------------------
-- pad UI
---------------------------------------------------------------------------

function buildControls()
  self.clearButtons()
  -- Element positions and sizes get multiplied by the pad's scale (5 x 0.3 x
  -- 3.2), so divide them out and lay out in world units: 5 wide, 3.2 deep.
  local s = self.getScale()
  local inv = { x = 1 / s.x, y = 1 / s.y, z = 1 / s.z }
  local elemScale = { inv.x, inv.y, inv.z }
  local function at(x, y, z) return { x * inv.x, y * inv.y, z * inv.z } end
  local function btn(label, fn, x, z, w, color, tip)
    self.createButton({
      click_function = fn, function_owner = self, label = label, tooltip = tip or '',
      position = at(x, 0.25, z), scale = elemScale,
      width = w, height = 230, font_size = 110,
      color = color or { 0.25, 0.25, 0.28 }, font_color = { 1, 1, 1 },
    })
  end

  local function label(text, x, z, size, color)
    self.createButton({
      click_function = 'noop', function_owner = self, label = text,
      position = at(x, 0.25, z), scale = elemScale,
      width = 0, height = 0, font_size = size, font_color = color or { 0.9, 0.9, 0.9 },
    })
  end

  -- The admin pad: seat, play mode, new-player flags, response wait, safety.
  -- Players answer Claude in chat, or on optional small pads (Yes / No / Respond).

  -- row 0: title + seat picker
  label('NEMESIS  ·  ' .. NAME .. '\'s seat: ' .. (state.seat or '-'), 0, -1.3, 110, { 1, 0.85, 0.4 })
  btn('<', 'onSeatPrev', -2.15, -1.3, 220, { 0.4, 0.3, 0.1 }, 'Previous free colour')
  btn('>', 'onSeatNext', 2.15, -1.3, 220, { 0.4, 0.3, 0.1 }, 'Next free colour')

  -- row 1: play mode
  local m = modeInfo(state.mode)
  btn('<', 'onModePrev', -2.15, -0.75, 220, { 0.2, 0.3, 0.45 }, 'Previous play mode')
  btn('Mode: ' .. m.name, 'noop', 0, -0.75, 1700, { 0.12, 0.18, 0.3 }, m.blurb)
  btn('>', 'onModeNext', 2.15, -0.75, 220, { 0.2, 0.3, 0.45 }, 'Next play mode')

  -- row 2: new-player flags, one toggle per seated human
  local humans = seatedHumans()
  if #humans == 0 then
    label('New players: nobody seated yet', 0, -0.2, 80)
  else
    local w = math.min(1.5, 4.7 / #humans)
    for i, c in ipairs(humans) do
      local x = (i - (#humans + 1) / 2) * w
      local on = state.newPlayers[c] == true
      local rgb = Color.fromString(c)
      self.createButton({
        click_function = 'onNewPlayer' .. c, function_owner = self,
        label = (on and '* ' or '') .. (#humans > 3 and c:sub(1, 3) or c), tooltip = 'Toggle: ' .. c .. ' is a new player (' .. NAME .. ' goes easy and explains more)',
        position = at(x, 0.25, -0.2), scale = elemScale, width = math.floor(w * 440), height = 210, font_size = 80,
        color = on and { rgb.r, rgb.g, rgb.b } or { rgb.r * 0.35, rgb.g * 0.35, rgb.b * 0.35 },
        font_color = on and { 0, 0, 0 } or { 1, 1, 1 },
      })
    end
  end

  -- row 3: optional player pads, how long silence waits, flow fallback
  local nPads = #playerPads()
  btn(nPads > 0 and 'Pads: on' or 'Pads: off', 'onTogglePlayerPads', -1.65, 0.35, 780, { 0.25, 0.35, 0.25 },
    'Optional: a small Yes / No / Respond pad for every seated player. Chat answers work without them.')
  btn(state.roundWait > 0 and ('Wait ' .. state.roundWait .. 's') or 'Wait: off', 'onRoundWait', 0, 0.35, 780, { 0.3, 0.25, 0.4 },
    'When ' .. NAME .. ' asks "any responses?", silence for this long counts as no responses. Say "wait" in chat to hold it.')
  btn('Your turn', 'onYourTurn', 1.65, 0.35, 780, { 0.16, 0.45, 0.7 }, 'Fallback: hand the turn to ' .. NAME .. ' (normally automatic)')

  -- row 4: answers to pad requests + safety
  btn('Yes', 'onYes', -1.8, 0.9, 700, { 0.15, 0.5, 0.2 }, 'Confirm a request on this pad (seat claim)')
  btn('No', 'onNo', -0.6, 0.9, 700, { 0.6, 0.15, 0.15 }, 'Decline a request on this pad')
  btn('Undo', 'onUndo', 0.6, 0.9, 700, { 0.4, 0.4, 0.15 }, 'Ask ' .. NAME .. ' to undo its last action')
  btn(state.stopped and 'RESUME' or 'STOP', 'onStop', 1.8, 0.9, 700,
    state.stopped and { 0.1, 0.5, 0.1 } or { 0.7, 0.1, 0.1 }, 'Freeze/unfreeze all of ' .. NAME .. '\'s actions')

  -- status line
  label(statusText, 0, 1.4, 80)
end

---------------------------------------------------------------------------
-- play modes and new-player flags
---------------------------------------------------------------------------
-- The pad only stores and shows the mode; modes/<id>.md says how the AI plays
-- it. These four are the defaults until nemesis-mcp sends the table's list.

MODES = MODES or {
  { id = 'slow_grow', name = 'Slow Grow', blurb = 'Casual default: build up, let the table get big, no early knockouts' },
  { id = 'group_hug', name = 'Group Hug', blurb = 'Hand out resources, help whoever is behind' },
  { id = 'chaos', name = 'Meme / Chaos', blurb = 'No real win condition, funniest play over the strongest' },
  { id = 'full_send', name = 'Full Send', blurb = 'Play to win' },
}

function modeInfo(id)
  for _, m in ipairs(MODES) do if m.id == id then return m end end
  return MODES[1]
end

-- nemesis-mcp: the AI's name and the play modes from modes/*.md
function ops.configure(req)
  local changed = false
  if type(req.name) == 'string' and req.name ~= '' and req.name ~= NAME then
    NAME = req.name; changed = true
    if state.seat then statusText = NAME .. ' is ' .. state.seat end
  end
  if type(req.modes) == 'table' and #req.modes > 0 then
    local same = #req.modes == #MODES
    for i, m in ipairs(req.modes) do
      local o = MODES[i]
      if not (o and o.id == m.id and o.name == m.name and o.blurb == m.blurb) then same = false end
    end
    if not same then
      MODES = {}
      for _, m in ipairs(req.modes) do
        MODES[#MODES + 1] = { id = tostring(m.id), name = tostring(m.name), blurb = tostring(m.blurb or '') }
      end
      changed = true
    end
    local known = false
    for _, m in ipairs(MODES) do if m.id == state.mode then known = true end end
    if not known then state.mode = req.defaultMode or MODES[1].id; changed = true end
  end
  if changed then buildControls(); refreshPlayerPads() end
  return { name = NAME, mode = state.mode, modes = #MODES, changed = changed }
end

state.mode = state.mode or 'slow_grow'
state.newPlayers = state.newPlayers or {}

function cycleMode(dir, color)
  local idx = 1
  for i, m in ipairs(MODES) do if m.id == state.mode then idx = i end end
  idx = ((idx - 1 + dir) % #MODES) + 1
  state.mode = MODES[idx].id
  local p = Player[color]
  setStatus('Mode: ' .. MODES[idx].name .. ' (set by ' .. (p and p.steam_name or color) .. ')')
  broadcastToAll('[Nemesis] Play mode: ' .. MODES[idx].name .. ' - ' .. MODES[idx].blurb, { 0.6, 0.75, 1 })
  emit({ ev = 'mode', mode = state.mode, name = MODES[idx].name, by = color })
end
function onModePrev(_, color) cycleMode(-1, color) end
function onModeNext(_, color) cycleMode(1, color) end

-- seated colours other than Claude's, in turn order
function seatedHumans()
  pcall(refreshColors)
  local out = {}
  for _, c in ipairs(COLORS) do
    if c ~= state.seat and Player[c].seated then out[#out + 1] = c end
  end
  return out
end

function toggleNewPlayer(c, by)
  state.newPlayers[c] = not state.newPlayers[c] or nil
  local on = state.newPlayers[c] == true
  setStatus(c .. (on and ' is flagged as a new player' or ' is no longer flagged'))
  emit({ ev = 'new_player', color = c, on = on, by = by })
end
for _, c in ipairs(CANON) do
  _G['onNewPlayer' .. c] = function(_, by) toggleNewPlayer(c, by) end
end

function newPlayerList()
  local out = {}
  for _, c in ipairs(CANON) do if state.newPlayers[c] then out[#out + 1] = c end end
  return out
end

function onPlayerChangeColor() buildControls() end

function noop() end

function setStatus(msg)
  statusText = msg
  buildControls()
end

function freeColors()
  pcall(refreshColors)
  local out = {}
  for _, c in ipairs(COLORS) do
    if not Player[c].seated then out[#out + 1] = c end
  end
  return out
end

function cycleSeat(dir, player)
  local free = freeColors()
  if #free == 0 then
    setStatus('Every colour is taken - someone must stand up first.')
    return
  end
  local idx = 0
  for i, c in ipairs(free) do if c == state.seat then idx = i end end
  idx = ((idx - 1 + dir) % #free) + 1
  state.seat = free[idx]
  setStatus(NAME .. ' is ' .. state.seat .. ' (set by ' .. player.steam_name .. ')')
  emit({ ev = 'seat', color = state.seat, by = player.color })
end

function onSeatPrev(_, color) cycleSeat(-1, Player[color]) end
function onSeatNext(_, color) cycleSeat(1, Player[color]) end

function button(id, color)
  local p = Player[color]
  emit({ ev = 'button', id = id, color = color, name = p and p.steam_name or '' })
end

function onYourTurn(_, color)
  button('your_turn', color)
  if state.seat and not state.claudeTurn then startClaudeTurn(Turns.turn_color) end
end
function onRespond(_, color) button('respond', color) end
function onPass(_, color) button('pass', color) end
function onYes(_, color)
  if state.pending and state.pending.kind == 'claim' then
    state.seat = state.pending.color
    state.pending = nil
    setStatus(NAME .. ' is ' .. state.seat .. ' (confirmed by ' .. Player[color].steam_name .. ')')
    emit({ ev = 'seat', color = state.seat, by = color })
  end
  button('yes', color)
end
function onNo(_, color)
  if state.pending then
    state.pending = nil
    setStatus('Request declined.')
  end
  button('no', color)
end
function onUndo(_, color) button('undo', color) end
function onStop(_, color)
  state.stopped = not state.stopped
  setStatus(state.stopped and NAME .. ' is FROZEN' or NAME .. ' may act again')
  button(state.stopped and 'stop' or 'resume', color)
end

---------------------------------------------------------------------------
-- player pads and response rounds
---------------------------------------------------------------------------
-- A round (ops.ask) collects one answer per asked seat and emits a single
-- ev=round when it is settled:
--   responses: 'respond' from anyone ends it at once; otherwise it ends
--              'all_no' when every asked seat passed, or when nobody spoke
--              up for the round's wait (timedOut=true)
--   yesno:     ends 'answered' with every seat's answer ('timeout' with
--              whatever came in, if the wait runs out)
-- Players answer in chat ("no", "go", "wait", "yes"). Tables that like
-- buttons can deploy optional player pads (Yes / No / Respond); presses come
-- back through playerPress(). Outside a round they are plain button events
-- ('respond' = "Claude, do you want to respond to this?", 'no' = "I pass").

state.roundWait = state.roundWait or 15
local WAITS = { 10, 15, 20, 30, 45, 0 }

function onRoundWait(_, color)
  local idx = 1
  for i, w in ipairs(WAITS) do if w == state.roundWait then idx = i end end
  state.roundWait = WAITS[(idx % #WAITS) + 1]
  setStatus(state.roundWait > 0 and ('Silence counts as no responses after ' .. state.roundWait .. ' s')
    or 'Rounds wait until everyone answers')
end

-- chat words that answer an open round; the first word decides, so
-- "no responses" passes and "wait, I have one" holds
local PASS_WORDS = { 'no', 'nope', 'nah', 'pass', 'passing', 'ok', 'okay', 'k', 'go', 'resolve', 'resolves', 'np',
  'nothing', 'fine', 'sure', 'yep', 'yeah', 'yes', 'y', 'n', 'continue', 'gogo', 'ty' }
local HOLD_WORDS = { 'wait', 'hold', 'respond', 'response', 'responding', 'stop', 'counter', 'counterspell', 'hang', 'actually' }

local function firstWord(text)
  return (text:lower():match('^%s*([%a]+)') or '')
end

local function hasWord(list, w)
  for _, x in ipairs(list) do if x == w then return true end end
  return false
end

function chatAnswer(color, text)
  local r = state.round
  if r == nil or type(text) ~= 'string' or color == state.seat then return end
  local w = firstWord(text)
  local lower = text:lower()
  if r.kind == 'responses' then
    -- anyone may hold the round ("in response..." too); passes count only
    -- from the seats asked
    if hasWord(HOLD_WORDS, w) or lower:match('^%s*in response') then
      r.answers[color] = 'respond'
      r.pending[color] = nil
      return finishRound('respond', color)
    end
    if not r.pending[color] then return end
    if lower:match('^%s*all good') then w = 'ok' end
    if hasWord(PASS_WORDS, w) then
      r.pending[color] = nil
      r.answers[color] = 'no'
      if next(r.pending) == nil then return finishRound('all_no') end
      refreshPlayerPads()
    end
  else
    if not r.pending[color] then return end
    local a = nil
    if w == 'yes' or w == 'yeah' or w == 'yep' or w == 'y' or w == 'sure' or w == 'ok' or w == 'okay' or w == 'please' then a = 'yes'
    elseif w == 'no' or w == 'nope' or w == 'nah' or w == 'n' then a = 'no' end
    if a == nil then return end
    r.pending[color] = nil
    r.answers[color] = a
    if next(r.pending) == nil then return finishRound('answered') end
    refreshPlayerPads()
  end
end

PLAYER_PAD_SCRIPT = [==[
nemesisPlayerPad = true
SEAT, ADMIN = nil, nil
NAME = 'Kestrel' -- the admin pad sends the real name with every hint
local hint, asking = '', false

function onLoad(s)
  local ok, d = pcall(function() return JSON.decode(s or '') end)
  if ok and type(d) == 'table' then SEAT = d.seat; ADMIN = d.admin; NAME = d.name or NAME end
  draw()
end

function onSave() return JSON.encode({ seat = SEAT, admin = ADMIN, name = NAME }) end

function draw()
  self.clearButtons()
  local s = self.getScale()
  local es = { 1 / s.x, 1 / s.y, 1 / s.z }
  local function at(x, z) return { x / s.x, 0.12 / s.y, z / s.z } end
  local function b(lbl, fn, x, z, w, h, fs, col, fcol, tip)
    self.createButton({ click_function = fn, function_owner = self, label = lbl, tooltip = tip or '',
      position = at(x, z), scale = es, width = w, height = h, font_size = fs,
      color = col or { 0, 0, 0, 0 }, font_color = fcol or { 1, 1, 1 } })
  end
  b((SEAT or '?') .. ' - answer ' .. NAME, 'noop', 0, -0.42, 0, 0, 75, nil, { 1, 0.85, 0.4 })
  b('Yes', 'onYes', -1.05, 0.08, 450, 220, 100, { 0.15, 0.5, 0.2 }, nil, 'Yes')
  b('No', 'onNo', 0, 0.08, 450, 220, 100, { 0.6, 0.15, 0.15 }, nil, 'No / no response / I pass')
  b('Respond', 'onRespond', 1.05, 0.08, 450, 220, 85, { 0.75, 0.45, 0.1 }, nil, 'Hold on, I have a response (or: ' .. NAME .. ', want to respond?)')
  if hint ~= '' then b(hint, 'noop', 0, 0.5, 0, 0, 60, nil, asking and { 1, 0.9, 0.5 } or { 0.8, 0.8, 0.8 }) end
end

function noop() end

local function press(id, color)
  local a = ADMIN and getObjectFromGUID(ADMIN) or nil
  if a == nil then
    for _, o in ipairs(getAllObjects()) do if o.getVar('nemesisPad') == true then a = o; ADMIN = o.getGUID() end end
  end
  if a == nil then broadcastToColor('The Nemesis pad is not on the table', color, { 1, 0.4, 0.4 }) return end
  a.call('playerPress', { seat = SEAT, clicker = color, id = id })
end
function onYes(_, c) press('yes', c) end
function onNo(_, c) press('no', c) end
function onRespond(_, c) press('respond', c) end

function setHint(p)
  if p and type(p.name) == 'string' and p.name ~= '' then NAME = p.name end
  hint = (p and p.text) or ''
  asking = p and p.ask == true
  self.setColorTint(asking and { 0.55, 0.35, 0.08 } or { 0.16, 0.16, 0.18 })
  draw()
end
]==]

function playerPads()
  local out = {}
  for _, o in ipairs(getAllObjects()) do
    if o.getVar('nemesisPlayerPad') == true then out[#out + 1] = o end
  end
  return out
end

-- player pad spot: text facing the player (same yaw as the mat)
local function playerPadSpot(c)
  local d = seatData(c)
  if d == nil or d.playmat == nil then return nil end
  local mat = d.playmat
  local sc = mat.getScale()
  -- just past the far edge of the mat, on the player's left (the mat's +right axis)
  local p = matWorld(mat, -(sc.z / 2 + 1.0), sc.x / 2 - 2.5)
  return { x = p.x, y = 1.2, z = p.z }, mat.getRotation().y
end

function deployPlayerPads()
  local made = {}
  local have = {}
  for _, o in ipairs(playerPads()) do have[o.getVar('SEAT') or ''] = true end
  for _, c in ipairs(seatedHumans()) do
    if not have[c] then
      local pos, yaw = playerPadSpot(c)
      if pos then
        spawnObjectData({ data = {
          Name = 'BlockSquare', Nickname = 'Nemesis player pad (' .. c .. ')', Locked = true,
          Transform = { posX = pos.x, posY = pos.y, posZ = pos.z, rotX = 0, rotY = yaw, rotZ = 0, scaleX = 3.4, scaleY = 0.15, scaleZ = 1.4 },
          ColorDiffuse = { r = 0.16, g = 0.16, b = 0.18 },
          LuaScript = PLAYER_PAD_SCRIPT,
          LuaScriptState = JSON.encode({ seat = c, admin = self.getGUID() }),
        } })
        made[#made + 1] = c
      end
    end
  end
  return made
end

function removePlayerPads()
  local n = 0
  for _, o in ipairs(playerPads()) do destroyObject(o); n = n + 1 end
  return n
end

function onTogglePlayerPads(_, color)
  if #playerPads() > 0 then
    local n = removePlayerPads()
    Wait.frames(function() setStatus('Removed ' .. n .. ' player pads') end, 2)
  else
    local made = deployPlayerPads()
    Wait.frames(function()
      setStatus(#made > 0 and ('Player pads for ' .. table.concat(made, ', ')) or 'No seated players to give a pad')
    end, 10)
  end
end

local function roundView(r)
  if r == nil then return nil end
  local waiting = {}
  for _, c in ipairs(r.asked) do if r.pending[c] then waiting[#waiting + 1] = c end end
  return { id = r.id, kind = r.kind, text = r.text, asked = r.asked, waitingOn = waiting, answers = r.answers }
end

function refreshPlayerPads()
  local r = state.round
  for _, o in ipairs(playerPads()) do
    local seat = o.getVar('SEAT')
    local hint, ask = '', false
    if r and r.pending[seat] then
      ask = true
      hint = NAME .. ': ' .. r.text:sub(1, 60) .. (r.kind == 'responses' and '  (No = pass)' or '')
    elseif r and r.answers[seat] then
      hint = 'You answered ' .. r.answers[seat] .. ', waiting on the others'
    end
    pcall(function() o.call('setHint', { text = hint, ask = ask, name = NAME }) end)
  end
end

function finishRound(result, by, timedOut, card)
  local r = state.round
  if r == nil then return end
  state.round = nil
  -- events must stay flat: a nested table never reached the listener live
  -- (only the catch-up buffer had it), so answers travel as 'Blue=no, White=yes'
  local parts = {}
  for _, c in ipairs(r.asked) do if r.answers[c] then parts[#parts + 1] = c .. '=' .. r.answers[c] end end
  emit({ ev = 'round', id = r.id, kind = r.kind, result = result, by = by, answers = table.concat(parts, ', '), text = r.text,
    timedOut = timedOut == true, card = card and card.name or nil, cardDesc = card and card.desc or nil, cardGuid = card and card.guid or nil })
  if r.quiet and result ~= 'respond' then
    -- a cast window: the caller announces the outcome
  elseif result == 'respond' then
    broadcastToAll('[Nemesis] ' .. tostring(by) .. ' has a response - hold on', { 1, 0.7, 0.3 })
  elseif result == 'all_no' then
    printToAll(timedOut and ('[Nemesis] No responses (' .. r.wait .. ' s)') or '[Nemesis] No responses', { 0.7, 0.7, 0.7 })
  elseif result == 'timeout' then
    printToAll('[Nemesis] Out of time; answers so far: ' .. (#parts > 0 and table.concat(parts, ', ') or 'none'), { 0.7, 0.7, 0.7 })
  end
  refreshPlayerPads()
end

-- a press on a player pad (called by the player pad's script)
function playerPress(p)
  if type(p) ~= 'table' or p.seat == nil then return end
  local seat, id = p.seat, p.id
  local pl = Player[seat]
  emit({ ev = 'button', id = id, color = seat, clicker = p.clicker, name = pl and pl.steam_name or '', pad = 'player' })
  local r = state.round
  if r and r.pending[seat] then
    r.pending[seat] = nil
    r.answers[seat] = id
    if r.kind == 'responses' and id == 'respond' then return finishRound('respond', seat) end
    if next(r.pending) == nil then return finishRound(r.kind == 'responses' and 'all_no' or 'answered') end
    refreshPlayerPads()
  end
end

---------------------------------------------------------------------------
-- api: the only entry point the Go side uses
---------------------------------------------------------------------------


-- Called as pad.call('api', {json='{"op":...}'}). Returns a JSON string.
function api(p)
  local req = p
  if type(p) == 'table' and type(p.json) == 'string' then
    local ok, decoded = pcall(function() return JSON.decode(p.json) end)
    if not ok or type(decoded) ~= 'table' then return JSON.encode({ error = 'bad json' }) end
    req = decoded
  end
  local fn = ops[req.op]
  if fn == nil then return JSON.encode({ error = 'unknown op: ' .. tostring(req.op) }) end
  pcall(refreshColors)
  local ok, res = pcall(fn, req)
  if not ok then return JSON.encode({ error = 'lua: ' .. tostring(res) }) end
  if type(res) ~= 'table' then res = { result = res } end
  return JSON.encode(res)
end

---------------------------------------------------------------------------
-- table layouts
---------------------------------------------------------------------------
-- The 4-seat table's Global data links every zone a seat has (library,
-- graveyard, exile, command, land row, life tracker, commander damage). The
-- 6-seat "MTG EDH 6-player (π)" table links only library, graveyard and
-- playmat. adaptSeat() fills in the rest so the ops below see one shape:
--   * lifeTracker / command mats: objects whose description is the colour
--     (the tracker script keys its owner off its description), with a
--     position fallback (the seat's side panel sits ~15 to the player's left)
--   * exile: the spot the table's own move2exile uses (library + forward 3.17,
--     card turned -90); land row: a band near the player's edge of the mat.
--     Both get a Nemesis-owned scripting zone so reads and zone events work.
--   * commander damage: no per-victim trackers, so the pad keeps a tally.


-- seat colours the table knows, in TTS turn order
function refreshColors()
  local d = Global.getTable('data')
  if d == nil then return end
  local out = {}
  for _, c in ipairs(CANON) do if d[c] ~= nil then out[#out + 1] = c end end
  if #out > 0 then COLORS = out end
end

state.layout = state.layout or {}   -- colour -> { exileZone=, landZone=, commandZone=, lifeTracker=, commandSpot= }
state.cmdr = state.cmdr or {}       -- victim colour -> { [from colour] = damage } (adapted tables only)

-- playmat-local coordinates of a world point: along = towards the player
function matLocal(mat, pos)
  local p, f, r = mat.getPosition(), mat.getTransformForward(), mat.getTransformRight()
  local dx, dz = pos.x - p.x, pos.z - p.z
  return dx * f.x + dz * f.z, dx * r.x + dz * r.z
end

function matWorld(mat, along, right, y)
  local p, f, r = mat.getPosition(), mat.getTransformForward(), mat.getTransformRight()
  return { x = p.x + f.x * along + r.x * right, y = y or p.y, z = p.z + f.z * along + r.z * right }
end

-- the seat's side panel (life tracker, command mats) in mat-local terms
local function inSidePanel(mat, pos)
  local along, right = matLocal(mat, pos)
  return right <= -10 and right >= -20 and along >= -14 and along <= 4
end

local function findOwned(color, name, mat)
  local byDesc, byPos = {}, {}
  for _, o in ipairs(getAllObjects()) do
    if o.getName() == name then
      if o.getDescription() == color then byDesc[#byDesc + 1] = o
      elseif inSidePanel(mat, o.getPosition()) then byPos[#byPos + 1] = o end
    end
  end
  return #byDesc > 0 and byDesc or byPos
end

local function zoneNamed(name)
  for _, o in ipairs(getAllObjects()) do
    if o.type == 'Scripting' and o.getName() == name then return o end
  end
  return nil
end

local function ensureZone(name, pos, yaw, scale)
  local z = zoneNamed(name)
  if z then
    z.setPosition(pos); z.setRotation({ 0, yaw, 0 }); z.setScale(scale)
    return z
  end
  z = spawnObject({ type = 'ScriptingTrigger', position = pos, rotation = { 0, yaw, 0 }, scale = scale,
    callback_function = function(o) o.setName(name) end })
  pcall(function() z.setName(name) end)
  return z
end

local function guidObj(g) if g == nil then return nil end return getObjectFromGUID(g) end

-- mat-local 'along' of the line between battlefield and land box on the
-- six-seat mat art (the box runs from here to the player's edge)
local LAND_BOX_LINE = 4.55

-- fill in the zones the 6-seat table does not link
function adaptSeat(color, d)
  local L = state.layout[color] or {}
  local mat, lib = d.playmat, d.libraryZone
  local yaw = mat.getRotation().y

  local tr = guidObj(L.lifeTracker)
  if tr == nil then
    local found = findOwned(color, 'Life Tracker', mat)
    tr = found[1]
    L.lifeTracker = tr and tr.getGUID() or nil
  end

  local cz = guidObj(L.commandZone)
  if cz == nil or L.commandSpot == nil then
    local mats = findOwned(color, 'Commander Zone', mat)
    local lo, hi, spot, spotAlong = nil, nil, nil, nil
    for _, m in ipairs(mats) do
      local a = matLocal(mat, m.getPosition())
      if lo == nil or a < lo then lo = a end
      if hi == nil or a > hi then hi = a end
      if spotAlong == nil or a > spotAlong then spot, spotAlong = m.getPosition(), a end
    end
    if lo == nil then lo, hi = -5, -2 end
    local c = matWorld(mat, (lo + hi) / 2, -15, mat.getPosition().y)
    if spot == nil then spot = c end
    cz = ensureZone('Nemesis ' .. color .. ' command', { c.x, 2, c.z }, yaw, { 4, 3, (hi - lo) + 3.4 })
    L.commandZone = cz.getGUID()
    L.commandSpot = { x = spot.x, y = spot.y, z = spot.z }
  end

  local ez = guidObj(L.exileZone)
  if ez == nil and lib ~= nil then
    local p, f = lib.getPosition(), lib.getTransformForward()
    ez = ensureZone('Nemesis ' .. color .. ' exile', { p.x + f.x * 3.17, 2, p.z + f.z * 3.17 }, lib.getRotation().y, { 3.6, 3, 2.4 })
    L.exileZone = ez.getGUID()
  end
  L.exileYaw = lib and (lib.getRotation().y - 90) or yaw

  -- the land box printed on the mat: from the line at along 4.55 (measured
  -- off the table art) to the mat's edge by the player, full mat width
  local half = mat.getScale().z / 2
  local boxNear, boxFar = LAND_BOX_LINE, half - 0.1
  local boxScale = { mat.getScale().x, 3, boxFar - boxNear }
  local lz = guidObj(L.landZone)
  if lz == nil or math.abs(lz.getScale().z - boxScale[3]) > 0.05 then
    local c = matWorld(mat, (boxNear + boxFar) / 2, 0)
    lz = ensureZone('Nemesis ' .. color .. ' lands', { c.x, 2, c.z }, yaw, boxScale)
    L.landZone = lz.getGUID()
  end

  state.layout[color] = L
  d.lifeTracker = tr
  d.commandZone = cz
  d.commandSpot = L.commandSpot
  d.exileZone = ez
  d.exileYaw = L.exileYaw
  d.landZone = lz
  d.adapted = true
end

function seatData(color)
  local all = Global.getTable('data')
  if all == nil then return nil end
  local d = all[color]
  if d == nil or d.playmat == nil then return d end
  if d.exileZone == nil or d.commandZone == nil or d.landZone == nil or d.lifeTracker == nil then
    adaptSeat(color, d)
  end
  return d
end

-- A seat's hand zones and what each one is for. Index 1 is the hand; the
-- 4-seat tables add a small secondary hand, and the (S) fork adds a private
-- token zone per seat (a hand zone with a 'TokenZone' object on it), which
-- holds tokens, not cards in hand.
local tokenSpots = { t = -10, list = {} }
function handZones(color)
  if Time.time - tokenSpots.t > 5 then
    tokenSpots = { t = Time.time, list = {} }
    for _, o in ipairs(getAllObjects()) do
      if o.getName() == 'TokenZone' then tokenSpots.list[#tokenSpots.list + 1] = o.getPosition() end
    end
  end
  local out = {}
  local ok, n = pcall(function() return Player[color].getHandCount() end)
  if not ok or type(n) ~= 'number' then n = 2 end
  for idx = 1, n do
    local kind = idx == 1 and 'hand' or 'secondary'
    local okt, tr = pcall(function() return Player[color].getHandTransform(idx) end)
    if okt and tr and tr.position then
      for _, p in ipairs(tokenSpots.list) do
        if math.abs(p.x - tr.position.x) < 3 and math.abs(p.z - tr.position.z) < 3 then kind = 'tokens' end
      end
    end
    out[#out + 1] = { idx = idx, kind = kind }
  end
  return out
end

function handObjects(color, idx)
  local ok, objs = pcall(function() return Player[color].getHandObjects(idx) end)
  return ok and objs or {}
end

function requireSeat()
  if state.seat == nil then error(NAME .. ' has no seat yet - pick one on the pad') end
  if state.stopped then error(NAME .. ' is frozen (STOP on the pad)') end
  return state.seat
end

-- rotation of a card relative to its zone: ~0 untapped, ~90 tapped
local function isTapped(obj, zone)
  local zr = zone and zone.getRotation().y or 0
  local diff = (obj.getRotation().y - zr + 180) % 360 - 180
  return math.abs(diff) > 45 and math.abs(diff) < 135
end

function isFaceDown(obj)
  local z = obj.getRotation().z
  return z > 90 and z < 270
end

local function round(n) return math.floor(n * 100 + 0.5) / 100 end

local function posOf(obj)
  local p = obj.getPosition()
  return { x = round(p.x), y = round(p.y), z = round(p.z) }
end

-- objects in a zone that are game pieces (skip the mat, zones, the pad...)
local function pieces(zone)
  local out = {}
  if zone == nil then return out end
  for _, o in ipairs(zone.getObjects()) do
    local t = o.type
    if t == 'Card' or t == 'Deck' or t == 'Tile' or t == 'Generic' or t == 'Figurine' or t == 'Chip' or t == 'Block' or t == 'Dice' then
      if o.getName() ~= 'Highlight Mat' and o.getGUID() ~= self.getGUID() and not o.getLock() then
        out[#out + 1] = o
      end
    end
  end
  return out
end

-- describe a public pile (graveyard/exile/command): cards are listed
local function publicPile(zone)
  local cards = {}
  for _, o in ipairs(pieces(zone)) do
    if o.type == 'Card' then
      cards[#cards + 1] = { guid = o.getGUID(), nick = o.getName(), desc = o.getDescription(), faceDown = isFaceDown(o) }
    elseif o.type == 'Deck' then
      for _, c in ipairs(o.getObjects()) do
        cards[#cards + 1] = { guid = c.guid, nick = c.name, desc = c.description, inPile = o.getGUID() }
      end
    end
  end
  return cards
end

-- count only: libraries are never enumerated here
local function pileCount(zone)
  local n = 0
  for _, o in ipairs(pieces(zone)) do
    if o.type == 'Card' then n = n + 1
    elseif o.type == 'Deck' then n = n + o.getQuantity() end
  end
  return n
end

-- Something resting on a card (a +X/+Y tile, a keyword token, a die) is a
-- counter on that card. The tile from the table's '+X/+Y Counter' bag renames
-- itself to its label ('+2/+2'), so the name is the value; dice report their
-- face; anything else is reported by name plus whatever numeric var it keeps.
local function counterValue(o)
  if o.type == 'Dice' then
    local ok, v = pcall(function() return o.getValue() end)
    if ok and v ~= nil then return tostring(v) end
  end
  for _, k in ipairs({ 'val', 'count', 'value' }) do
    local ok, v = pcall(function() return tostring(o.getVar(k)) end)
    if ok and v ~= nil and v ~= 'nil' then return v end
  end
  return nil
end

-- On-card state from the table's Encoder (Pie's modules): +1/+1 counters,
-- generic counters, loyalty, P/T tweaks, keyword icons, notepad, phased out,
-- token marker. This is what players use instead of tile counters. Read-only
-- here; ops.mark writes it for Claude's own cards.
local KEYWORD_IDS = {
  frozen = 'mtg_frozen', deathtouch = 'mtg_deathtouchcounter', defender = 'mtg_defendercounter',
  ['double strike'] = 'mtg_doublestrikecounter', ['first strike'] = 'mtg_firststrikecounter',
  flying = 'mtg_flyingcounter', haste = 'mtg_hastecounter', hexproof = 'mtg_hexproofcounter',
  indestructible = 'mtg_indestructiblecounter', lifelink = 'mtg_lifelinkcounter', menace = 'mtg_menacecounter',
  monstrous = 'mtg_monstrous', reach = 'mtg_reachcounter', trample = 'mtg_tramplecounter',
  vigilance = 'mtg_vigilancecounter', stun = 'mtg_stuncounter', exert = 'mtg_exert', renowned = 'mtg_renowned',
  suspend = 'mtg_suspendcounter',
}
local KEYWORD_NAMES = {}
for name, id in pairs(KEYWORD_IDS) do KEYWORD_NAMES[id] = name end

local function encoder()
  local ok, enc = pcall(function() return Global.getVar('Encoder') end)
  if ok and enc ~= nil then return enc end
  return nil
end

local function encoderInfo(o)
  local enc = encoder()
  if enc == nil then return nil end
  local ok, out = pcall(function()
    if not enc.call('APIobjectExists', { obj = o }) then return nil end
    local props = enc.call('APIobjGetProps', { obj = o }) or {}
    local data = enc.call('APIobjGetAllData', { obj = o }) or {}
    local e = {}
    local tu = data.tyrantUnified
    if type(tu) == 'table' then
      local face = (type(tu.cardFaces) == 'table') and tu.cardFaces[tu.activeFace or 1] or nil
      if (tu.plusOneCounters or 0) ~= 0 then e.plusOne = tu.plusOneCounters end
      if face and face.isPlaneswalker then
        e.loyalty = tu.namedCounters or 0
      elseif (tu.namedCounters or 0) ~= 0 then
        e.counters = tu.namedCounters
      end
      if (tu.power or 0) ~= 0 then e.power = tu.power end
      if (tu.toughness or 0) ~= 0 then e.toughness = tu.toughness end
      if face then
        local bp, bt = tonumber(face.basePower), tonumber(face.baseToughness)
        if bp and bt and (bp ~= 0 or bt ~= 0) then e.basePower = bp; e.baseToughness = bt end
      end
    end
    if props['πCounter'] and (tonumber(data.picounter) or 0) ~= 0 then e.piCounter = data.picounter end
    if props['πKeywords'] then
      local kws = {}
      for id, name in pairs(KEYWORD_NAMES) do
        local v = data[id]
        if type(v) == 'number' and v > 0 then
          kws[#kws + 1] = v > 1 and (name .. ' x' .. v) or name
        elseif v == true then
          kws[#kws + 1] = name
        end
      end
      table.sort(kws)
      if #kws > 0 then e.keywords = kws end
    end
    if props['πotepad'] and type(data.note) == 'table' and data.note.text and data.note.text ~= '' then
      e.note = tostring(data.note.text):sub(1, 200)
    end
    if props['MTG_Phase'] and data.mtg_phased == true then e.phased = true end
    if props['MTG_Token'] and data.mtg_token == true then e.token = true end
    -- base P/T alone is just the printed card: nothing to report
    local interesting = false
    for k in pairs(e) do if k ~= 'basePower' and k ~= 'baseToughness' then interesting = true end end
    if not interesting then return nil end
    return e
  end)
  if ok then return out end
  return nil
end

-- which card (world-space bounds, any rotation) is this piece sitting on?
local function cardUnder(o, cardsByGuid)
  local p = o.getPosition()
  local best, bestD = nil, nil
  for _, c in pairs(cardsByGuid) do
    local b = c.bounds
    local cx, cz = b.center.x, b.center.z
    local hx, hz = b.size.x / 2 + 0.15, b.size.z / 2 + 0.15
    if math.abs(p.x - cx) <= hx and math.abs(p.z - cz) <= hz and p.y >= b.center.y - 0.5 then
      local dd = (p.x - cx) ^ 2 + (p.z - cz) ^ 2
      if bestD == nil or dd < bestD then best, bestD = c, dd end
    end
  end
  return best
end

local function battlefield(d, mine)
  local out = {}
  local landGuids = {}
  if d.landZone then
    for _, o in ipairs(d.landZone.getObjects()) do landGuids[o.getGUID()] = true end
  end
  local cardsByGuid = {}
  local loose = {}
  for _, o in ipairs(pieces(d.playmat)) do
    local e = { guid = o.getGUID(), type = o.type, pos = posOf(o), tapped = isTapped(o, d.playmat), counters = {} }
    if landGuids[e.guid] then e.row = 'land' end
    if o.type == 'Card' then
      e.faceDown = isFaceDown(o)
      if e.faceDown and not mine then
        e.nick = '(face-down card)'
      else
        e.nick = o.getName()
        e.desc = o.getDescription()
      end
      cardsByGuid[e.guid] = { entry = e, bounds = o.getBounds() }
      e.enc = encoderInfo(o)
      out[#out + 1] = e
    elseif o.type == 'Deck' then
      e.nick = o.getName()
      e.count = o.getQuantity()
      out[#out + 1] = e
    else
      e.nick = o.getName()
      e.desc = o.getDescription()
      loose[#loose + 1] = { obj = o, entry = e }
    end
  end
  for _, l in ipairs(loose) do
    local host = cardUnder(l.obj, cardsByGuid)
    if host then
      local c = host.entry.counters
      c[#c + 1] = { guid = l.entry.guid, name = l.entry.nick, value = counterValue(l.obj), type = l.obj.type }
    else
      out[#out + 1] = l.entry
    end
  end
  return out
end

function ops.ping(req)
  return { ok = true, version = NEMESIS_VERSION, seat = state.seat, stopped = state.stopped }
end

-- Poison / Energy / Experience counter tiles (val) beside a seat's mat. The
-- (S) table has them per seat; tables without them report nothing.
local PLAYER_COUNTERS = { ['Poison Counter'] = 'poison', ['Energy Counter'] = 'energy', ['Experience Counter'] = 'experience' }
local counterTiles = { t = -10, list = {} }
function seatCounters(color, d)
  if d == nil or d.playmat == nil then return nil end
  if Time.time - counterTiles.t > 5 then
    counterTiles = { t = Time.time, list = {} }
    for _, o in ipairs(getAllObjects()) do
      if PLAYER_COUNTERS[o.getName()] then counterTiles.list[#counterTiles.list + 1] = o end
    end
  end
  local out, any = {}, false
  for _, o in ipairs(counterTiles.list) do
    if not o.isDestroyed() then
      -- nearest mat wins: compare against every seat's mat
      local p, best, bestD = o.getPosition(), nil, nil
      local all = Global.getTable('data') or {}
      for c, sd in pairs(all) do
        if type(sd) == 'table' and sd.playmat then
          local mp = sd.playmat.getPosition()
          local dd = (mp.x - p.x) ^ 2 + (mp.z - p.z) ^ 2
          if bestD == nil or dd < bestD then best, bestD = c, dd end
        end
      end
      if best == color then
        local ok, v = pcall(function() return tonumber(o.getVar('val')) end)
        if ok and v and v ~= 0 then out[PLAYER_COUNTERS[o.getName()]] = v; any = true end
      end
    end
  end
  return any and out or nil
end

-- Everything public about the table, plus counts for the hidden parts.
function ops.snapshot(req)
  local seats = {}
  for _, c in ipairs(COLORS) do
    local d = seatData(c)
    local pl = Player[c]
    local s = { color = c, seated = pl.seated, name = pl.steam_name or '', isClaude = (c == state.seat) }
    s.handCount, s.hand2Count, s.tokenZoneCount = 0, 0, 0
    for _, h in ipairs(handZones(c)) do
      local n = #handObjects(c, h.idx)
      if h.kind == 'hand' then s.handCount = s.handCount + n
      elseif h.kind == 'tokens' then s.tokenZoneCount = s.tokenZoneCount + n
      else s.hand2Count = s.hand2Count + n end
    end
    s.playerCounters = seatCounters(c, d)
    if d then
      if d.lifeTracker then s.life = tonumber(d.lifeTracker.getVar('count')) end
      s.libraryCount = pileCount(d.libraryZone)
      s.graveyard = publicPile(d.graveyard)
      s.exile = publicPile(d.exileZone)
      s.command = publicPile(d.commandZone)
      s.battlefield = battlefield(d, c == state.seat)
      s.commanderDamage = {}
      if d.adapted then
        for _, from in ipairs(COLORS) do
          local n = (state.cmdr[c] or {})[from]
          if n and n > 0 then s.commanderDamage[#s.commanderDamage + 1] = { from = from, amount = n } end
        end
      end
      -- Global keeps these as GUID strings, not object refs
      for _, ref in ipairs(d.commanderDamage or {}) do
        local tr = ref
        if type(ref) == 'string' then tr = getObjectFromGUID(ref) end
        if tr ~= nil then
          local okv, v = pcall(function() return tr.getVar('val') end)
          s.commanderDamage[#s.commanderDamage + 1] = { from = tr.getDescription(), amount = okv and tonumber(v) or nil }
        end
      end
    end
    seats[#seats + 1] = s
  end
  local turns = { enabled = Turns.enable, active = Turns.turn_color, claudeTurn = state.claudeTurn == true }
  if state.claudeTurn then
    turns.active = state.seat
    turns.next = neighbourSeat(1)
  else
    local okn, nxt = pcall(function() return Turns.getNextTurnColor() end)
    if okn then turns.next = nxt end
  end
  return {
    seats = seats, turns = turns,
    pad = { seat = state.seat, stopped = state.stopped, pending = state.pending, version = NEMESIS_VERSION,
      mode = state.mode, modeName = modeInfo(state.mode).name, newPlayers = newPlayerList(),
      round = roundView(state.round), playerPads = #playerPads(), roundWait = state.roundWait },
  }
end

-- Claude's own hand only.
function ops.hand(req)
  local seat = state.seat
  if seat == nil then error('no seat') end
  local out = {}
  for _, h in ipairs(handZones(seat)) do
    for _, o in ipairs(handObjects(seat, h.idx)) do
      out[#out + 1] = { guid = o.getGUID(), nick = o.getName(), desc = o.getDescription(), hand = h.idx, zone = h.kind }
    end
  end
  return { seat = seat, cards = out }
end

-- Claude asks for a colour; a human must press Yes on the pad.
function ops.claim(req)
  local want = req.color
  local okc = false
  for _, c in ipairs(COLORS) do if c == want then okc = true end end
  if not okc then error('unknown colour ' .. tostring(want)) end
  if Player[want].seated then error(want .. ' is taken by ' .. Player[want].steam_name) end
  state.pending = { kind = 'claim', color = want }
  setStatus(NAME .. ' asks to sit as ' .. want .. ' - press Yes or No')
  broadcastToAll('[Nemesis] ' .. NAME .. ' would like to sit as ' .. want .. '. Press Yes on the pad to confirm.', { 1, 0.85, 0.4 })
  return { pending = want }
end

function ops.say(req)
  requireSeat()
  local text = tostring(req.text or '')
  if text == '' then error('nothing to say') end
  local col = Color.fromString(state.seat)
  -- broadcastToAll also writes the line to chat, so a loud line must not be
  -- printed as well or it shows up twice
  if req.loud then broadcastToAll('[' .. NAME .. '] ' .. text, col) else printToAll('[' .. NAME .. '] ' .. text, col) end
  return { said = text }
end

-- Ask the table something and collect one answer per seat on the player
-- pads (see playerPress). kind = 'responses' ("any responses?") or 'yesno'.
-- seats defaults to every seated human. A new ask replaces an open one.
function ops.ask(req)
  local seat = requireSeat()
  local kind = req.kind == 'yesno' and 'yesno' or 'responses'
  local seats = {}
  if type(req.seats) == 'table' and #req.seats > 0 then
    for _, c in ipairs(req.seats) do if c ~= seat then seats[#seats + 1] = c end end
  else
    seats = seatedHumans()
  end
  if #seats == 0 then error('nobody to ask') end
  local text = tostring(req.text or '')
  if text == '' then text = kind == 'responses' and 'Any responses?' or 'Yes or no?' end
  state.roundSeq = (state.roundSeq or 0) + 1
  local wait = tonumber(req.wait)
  if wait == nil or wait < 0 then
    wait = state.roundWait
    -- a yes/no question deserves longer than "any responses?"
    if kind == 'yesno' and wait > 0 then wait = math.max(wait * 2, 30) end
  end
  local r = { id = state.roundSeq, kind = kind, text = text, asked = seats, pending = {}, answers = {}, wait = wait, quiet = req.quiet == true }
  for _, c in ipairs(seats) do r.pending[c] = true end
  local replaced = state.round and state.round.id or nil
  state.round = r
  local how
  if kind == 'responses' then
    how = '  [say "no" to pass, "wait" to respond' .. (wait > 0 and ('; ' .. wait .. ' s]') or ']')
  else
    how = '  [answer yes or no in chat]'
  end
  if r.quiet then
    -- a spell going on the stack: no question, just the window
    printToAll('[' .. NAME .. '] ' .. text .. '  [drop a response on my mat or say "wait"' .. (wait > 0 and ('; ' .. wait .. ' s]') or ']'),
      Color.fromString(seat))
  else
    broadcastToAll('[' .. NAME .. '] ' .. text .. how, Color.fromString(seat))
  end
  refreshPlayerPads()
  if wait > 0 then
    Wait.time(function()
      if state.round ~= r then return end
      finishRound(r.kind == 'responses' and 'all_no' or 'timeout', nil, true)
    end, wait)
  end
  return { round = r.id, kind = kind, asked = seats, playerPads = #playerPads(), replaced = replaced, wait = wait }
end

function ops.cancel_round(req)
  requireSeat()
  local r = state.round
  if r == nil then return { cancelled = false } end
  state.round = nil
  refreshPlayerPads()
  return { cancelled = true, id = r.id, answers = r.answers }
end

-- Claude can deploy or remove the player pads too (setup help)
function ops.player_pads(req)
  requireSeat()
  if req.remove then return { removed = removePlayerPads() } end
  return { deployed = deployPlayerPads() }
end

---------------------------------------------------------------------------
-- acting: everything below moves Claude's own objects only
---------------------------------------------------------------------------

-- GUIDs inside Claude's library that look{} has handed out; the only way a
-- library card may be addressed.
state.looked = {}

local function myData()
  local seat = requireSeat()
  local d = seatData(seat)
  if d == nil then error('table has no data for ' .. seat) end
  return seat, d
end

local function yawOf(zone) return zone.getRotation().y end

-- position helpers relative to Claude's mat: forward points at the player
local function matPoint(d, along, right, y)
  local p = d.playmat.getPosition()
  local f = d.playmat.getTransformForward()
  local r = d.playmat.getTransformRight()
  return { x = p.x + f.x * along + r.x * right, y = y or 1.5, z = p.z + f.z * along + r.z * right }
end

local CARD_W, CARD_H = 2.2, 3.06   -- a card on the table, untapped
local COL_SPACING = 2.6            -- room for a tapped neighbour
local OVERLAP_SHIFT = 0.9          -- each overlap layer slides this far sideways

-- Battlefield layout, in mat-local rows (along: + towards the player).
-- Both tables' mats are printed with a land box along the player's edge (the
-- land zone covers it) and the battlefield in front of it; the pod plays
-- creatures up front (nearest the table centre), other permanents behind
-- them, lands only inside the box.

local function colsFor(width)
  return math.max(1, math.floor((width - CARD_W) / COL_SPACING) + 1)
end

-- { rows = {along...} nearest-the-player first, cols = {right...} left to right
-- as the player sees it }; the stack is a single centred spot
local function rowSpec(d, row)
  local mat = d.playmat
  local function centred(n, step, mid)
    local out = {}
    -- player's left is the mat's +right
    for i = 1, n do out[i] = mid + ((n + 1) / 2 - i) * step end
    return out
  end
  if row == 'land' then
    local z = d.landZone
    local along0, right0 = matLocal(mat, z.getPosition())
    local s = z.getScale()
    local nRows = math.max(1, math.floor(s.z / CARD_H))
    local rows = {}
    for k = 1, nRows do rows[k] = along0 + ((nRows - 1) / 2 - (k - 1)) * CARD_H end
    return { rows = rows, cols = centred(colsFor(s.x), COL_SPACING, right0) }
  end
  local half = mat.getScale().z / 2
  local landNear = matLocal(mat, d.landZone.getPosition()) - d.landZone.getScale().z / 2
  local front = -half + CARD_H / 2 + 0.2
  local step = CARD_H + 0.1
  local cols = centred(colsFor(mat.getScale().x), COL_SPACING, 0)
  if row == 'creature' then return { rows = { front, front + step }, cols = cols } end
  if row == 'stack' then
    local otherTop = front + 2 * step + CARD_H / 2
    return { rows = { (otherTop + landNear) / 2 }, cols = cols }
  end
  return { rows = { front + 2 * step }, cols = cols }
end

-- where the stack (announced spells, reveals) goes; offset in columns
local function stackPoint(d, offset)
  local spec = rowSpec(d, 'stack')
  return matPoint(d, spec.rows[1], (offset or 0) * COL_SPACING, 1.5)
end

-- Slots handed out in the last few seconds: a card glides to its slot over
-- about a second, so two quick plays must not both pick the same empty slot
-- (two Forests landing on one spot merge into a deck).
state.reserved = {}

-- next free slot in a row: fill each row left to right, nearest the player
-- first; when every slot is taken, overlap the earlier cards slightly offset
local function rowSlot(d, row)
  local spec = rowSpec(d, row)
  local now = Time.time
  local taken = {}
  for _, o in ipairs(pieces(d.playmat)) do
    local a, r = matLocal(d.playmat, o.getPosition())
    taken[#taken + 1] = { a = a, r = r }
  end
  local keep = {}
  for _, s in ipairs(state.reserved) do
    if now - s.t < 3 then
      keep[#keep + 1] = s
      local a, r = matLocal(d.playmat, s.pos)
      taken[#taken + 1] = { a = a, r = r }
    end
  end
  state.reserved = keep
  local function free(a, r, layer)
    -- layer 0: nothing inside the slot's footprint; overlap layers: nothing
    -- sitting on that exact offset spot yet
    local da, dr = CARD_H / 2, COL_SPACING / 2
    if layer > 0 then da, dr = 0.45, 0.45 end
    for _, t in ipairs(taken) do
      if math.abs(t.a - a) < da and math.abs(t.r - r) < dr then return false end
    end
    return true
  end
  for layer = 0, 6 do
    for _, a in ipairs(spec.rows) do
      for _, c in ipairs(spec.cols) do
        -- overlaps slide towards the middle so they stay inside the box
        local r = c + layer * OVERLAP_SHIFT * (c > 0 and -1 or 1)
        if free(a, r, layer) then
          local pos = matPoint(d, a, r, 1.5 + 0.3 * layer)
          state.reserved[#state.reserved + 1] = { pos = pos, t = now }
          return pos
        end
      end
    end
  end
  return matPoint(d, spec.rows[1], spec.cols[1], 3)
end

-- the deck (or lone card) sitting in Claude's library zone
local function libraryTop(d)
  local best = nil
  for _, o in ipairs(pieces(d.libraryZone)) do
    if o.type == 'Deck' then return o end
    if o.type == 'Card' then best = o end
  end
  return best
end

-- where may Claude take an object from? hand, own mat, own public piles,
-- or a library card previously revealed to it by look{}
local function myObject(seat, d, guid)
  if guid == nil then error('guid required') end
  for _, h in ipairs(handZones(seat)) do
    for _, o in ipairs(handObjects(seat, h.idx)) do if o.getGUID() == guid then return o, 'hand' end end
  end
  local zones = { playmat = d.playmat, graveyard = d.graveyard, exile = d.exileZone, command = d.commandZone }
  for name, z in pairs(zones) do
    for _, o in ipairs(pieces(z)) do
      if o.getGUID() == guid then return o, name end
      if o.type == 'Deck' and name ~= 'playmat' then
        for _, c in ipairs(o.getObjects()) do
          if c.guid == guid then return o.takeObject({ guid = guid, position = o.getPosition() + Vector(0, 2, 0) }), name end
        end
      end
    end
  end
  if state.looked[guid] then
    local lib = libraryTop(d)
    if lib and lib.type == 'Deck' then
      local p = lib.getPosition()
      local c = lib.takeObject({ guid = guid, position = { p.x, p.y + 3, p.z } })
      if c then state.looked[guid] = nil; return c, 'library' end
    elseif lib and lib.getGUID() == guid then
      state.looked[guid] = nil
      return lib, 'library'
    end
  end
  error('not one of my objects: ' .. tostring(guid))
end

-- Cards held by a hand zone ignore smooth moves, so leave the hand with an
-- instant hop first, then glide into place.
local function inAnyHand(obj)
  for _, c in ipairs(COLORS) do
    for _, h in ipairs(handZones(c)) do
      for _, o in ipairs(handObjects(c, h.idx)) do if o == obj then return true end end
    end
  end
  return false
end

local function faceUpAt(obj, pos, yaw)
  state.touched[obj.getGUID()] = Time.time
  if inAnyHand(obj) then
    -- a smooth move that starts inside the hand volume is cancelled by the
    -- hand re-grabbing the card, so teleport straight to the destination
    obj.setPosition(pos)
    obj.setRotation({ 0, yaw, 0 })
    return
  end
  obj.setPositionSmooth(pos, false, true)
  obj.setRotationSmooth({ 0, yaw, 0 }, false, true)
end

function ops.draw(req)
  local seat, d = myData()
  local n = tonumber(req.n) or 1
  local lib = libraryTop(d)
  if lib == nil then error('library is empty') end
  local avail = lib.type == 'Deck' and lib.getQuantity() or 1
  if n > avail then n = avail end
  -- let a queued library move (see ops.move) finish before dealing
  local delay = math.max(0, (state.libBusyUntil or 0) - Time.time)
  state.dealingUntil = Time.time + delay + 3 -- the dealt cards are mine: no zone events
  if delay > 0 then
    Wait.time(function() local l = libraryTop(d); if l then l.deal(n, seat) end end, delay)
  else
    lib.deal(n, seat)
  end
  return { drew = n, libraryLeft = avail - n }
end

function ops.shuffle(req)
  local _, d = myData()
  local lib = libraryTop(d)
  if lib == nil or lib.type ~= 'Deck' then error('no deck to shuffle') end
  lib.shuffle()
  state.looked = {}
  return { shuffled = lib.getQuantity() }
end

-- London mulligan step 1: hand back into the library, shuffle, draw 7.
-- Cards to bottom afterwards go through move{to='library_bottom'}.
function ops.mulligan(req)
  local seat, d = myData()
  local lib = libraryTop(d)
  if lib == nil then error('library is empty') end
  local hand = Player[seat].getHandObjects(1)
  for _, c in ipairs(hand) do state.touched[c.getGUID()] = Time.time; lib = lib.putObject(c) or lib end
  state.dealingUntil = Time.time + 5
  Wait.time(function()
    local deck = libraryTop(d)
    if deck then
      deck.shuffle()
      Wait.time(function() deck.deal(7, seat) end, 0.6)
    end
  end, 0.8)
  return { returned = #hand, drawing = 7 }
end

-- hand/command zone -> battlefield row ('land' | 'creature' | 'other'), or
-- -> 'stack' (announce a spell; resolve it later with move{})
function ops.play(req)
  local seat, d = myData()
  local obj, from = myObject(seat, d, req.guid)
  local row = req.row or 'other'
  if row ~= 'land' and row ~= 'creature' and row ~= 'other' and row ~= 'stack' then error('row must be land/creature/other/stack') end
  local pos = (row == 'stack') and stackPoint(d) or rowSlot(d, row)
  faceUpAt(obj, pos, yawOf(d.playmat))
  return { played = obj.getName(), from = from, row = row, guid = obj.getGUID() }
end

local function setTapped(d, obj, tapped)
  state.touched[obj.getGUID()] = Time.time
  local r = obj.getRotation()
  local yaw = yawOf(d.playmat) + (tapped and 90 or 0)
  obj.setRotationSmooth({ r.x, yaw, r.z }, false, true)
end

function ops.tap(req)
  local seat, d = myData()
  local done = {}
  for _, g in ipairs(req.guids or {}) do
    local obj, from = myObject(seat, d, g)
    if from ~= 'playmat' then error(obj.getName() .. ' is not on my battlefield') end
    setTapped(d, obj, true)
    done[#done + 1] = obj.getName()
  end
  return { tapped = done }
end

function ops.untap(req)
  local seat, d = myData()
  local done = {}
  if req.all then
    for _, o in ipairs(pieces(d.playmat)) do
      if isTapped(o, d.playmat) then setTapped(d, o, false); done[#done + 1] = o.getName() end
    end
  else
    for _, g in ipairs(req.guids or {}) do
      local obj, from = myObject(seat, d, g)
      if from ~= 'playmat' then error(obj.getName() .. ' is not on my battlefield') end
      setTapped(d, obj, false)
      done[#done + 1] = obj.getName()
    end
  end
  return { untapped = done }
end

function ops.flip(req)
  local seat, d = myData()
  local obj = myObject(seat, d, req.guid)
  obj.flip()
  return { flipped = obj.getName() }
end

-- Put a card on top (or the bottom) of my library. The card is lifted out of
-- any hand or pile first and held above the deck until it has finished
-- spawning: putObject on a card still coming out of a graveyard pile, or
-- still held by the hand zone after a look, sometimes left it out, and the
-- next draw was another card. Afterwards the deck is checked, and a card on
-- the wrong end is taken out and put back once more.
function toLibrary(d, obj, bottom, try)
  if obj == nil or obj.isDestroyed() then return end
  local guid, name = obj.getGUID(), obj.getName()
  local p, yaw = d.libraryZone.getPosition(), yawOf(d.libraryZone)
  state.touched[guid] = Time.time
  obj.setLock(true)
  obj.setPosition({ p.x, p.y + 2.5, p.z })
  obj.setRotation({ 0, yaw, bottom and 0 or 180 })
  local function check()
    local deck = libraryTop(d)
    if deck == nil or deck.type ~= 'Deck' then return end
    local list = deck.getObjects()
    local want = bottom and list[#list] or list[1]
    if want and want.guid == guid then return end
    for _, c in ipairs(list) do
      if c.guid == guid and try < 2 then
        state.libBusyUntil = math.max(state.libBusyUntil or 0, Time.time + 2.5)
        local again = deck.takeObject({ guid = guid, position = { p.x, p.y + 2.5, p.z }, smooth = false })
        Wait.time(function() toLibrary(d, again, bottom, try + 1) end, 0.3)
        return
      end
    end
    emit({ ev = 'log', text = name .. ' may not be on the ' .. (bottom and 'bottom' or 'top') .. ' of your library: check it with look' })
  end
  local function put()
    if obj == nil or obj.isDestroyed() then return end
    obj.setLock(false)
    local lib = libraryTop(d)
    if lib == nil or lib == obj then return end -- library empty: it drops face down onto the spot
    if not bottom then
      lib.putObject(obj)
      Wait.time(check, 0.5)
      return
    end
    -- putObject stacks on top, so flip the deck, add, flip back: the
    -- physical bottom becomes the top for a moment.
    lib.flip()
    Wait.time(function()
      local deck = lib.putObject(obj) or lib
      Wait.time(function() deck.flip(); Wait.time(check, 0.5) end, 0.3)
    end, 0.5)
  end
  Wait.condition(put, function() return obj == nil or obj.isDestroyed() or not obj.spawning end, 3, put)
end

-- move one of my objects: graveyard | exile | command | hand | stack |
-- library_top | library_bottom | battlefield (row via req.row)
function ops.move(req)
  local seat, d = myData()
  local obj, from = myObject(seat, d, req.guid)
  local to = req.to
  local yaw = yawOf(d.playmat)
  local name = obj.getName()
  if to == 'graveyard' then
    local p = d.graveyard.getPosition(); faceUpAt(obj, { p.x, p.y + 2, p.z }, yawOf(d.graveyard))
  elseif to == 'exile' then
    local p = d.exileZone.getPosition(); faceUpAt(obj, { p.x, p.y + 2, p.z }, d.exileYaw or yawOf(d.exileZone))
  elseif to == 'command' then
    local p = d.commandSpot or d.commandZone.getPosition(); faceUpAt(obj, { p.x, p.y + 2, p.z }, yawOf(d.commandZone))
  elseif to == 'hand' then
    state.touched[obj.getGUID()] = Time.time
    obj.deal(1, seat)
  elseif to == 'stack' then
    faceUpAt(obj, stackPoint(d), yaw)
  elseif to == 'battlefield' then
    faceUpAt(obj, rowSlot(d, req.row or 'other'), yaw)
  elseif to == 'library_top' or to == 'library_bottom' then
    -- Library moves are queued: a bottom move flips the deck for about a
    -- second, and a second move landing inside that window put its card on
    -- the wrong end (three hideaway bottoms in a row left one on top).
    local delay = math.max(0, (state.libBusyUntil or 0) - Time.time)
    state.libBusyUntil = Time.time + delay + (to == 'library_bottom' and 2.2 or 1.4)
    state.touched[obj.getGUID()] = Time.time + delay
    Wait.time(function() toLibrary(d, obj, to == 'library_bottom', 1) end, delay)
  else
    error('unknown destination ' .. tostring(to))
  end
  return { moved = name, from = from, to = to }
end

-- Top n of MY library. Cards inside a deck have no GUID, so they are taken
-- off the top (takeObject with no index = top card) into Claude's secondary
-- hand zone, which only Claude can read. Put them back with
-- move{to='library_top'|'library_bottom'} in the order wanted, or keep them
-- (hand/graveyard/exile) when the effect says so.
function ops.look(req)
  local seat, d = myData()
  local n = tonumber(req.n) or 1
  local wait = (state.libBusyUntil or 0) - Time.time
  if wait > 0 then error(string.format('library is still settling from a move, try again in %.1f s', wait)) end
  local lib = libraryTop(d)
  if lib == nil then return { cards = {} } end
  local out = {}
  local ht = Player[seat].getHandTransform(2)
  local hp = ht and ht.position or d.libraryZone.getPosition()
  local avail = lib.type == 'Deck' and lib.getQuantity() or 1
  if n > avail then n = avail end
  -- fan along the hand's own axis and keep the fan narrow: a wide one
  -- (2.2 apart, world x) spilled a 16-card look into the next seat's hand
  local hr = (ht and ht.right) or Vector(1, 0, 0)
  local step = n > 1 and math.min(2.2, 9 / (n - 1)) or 0
  local function fanAt(i)
    local off = (i - 1) * step - (n - 1) * step / 2
    return { hp.x + hr.x * off, hp.y + 1 + i * 0.05, hp.z + hr.z * off }
  end
  for i = 1, n do
    local c
    if lib.type == 'Deck' and lib.getQuantity() > 1 then
      c = lib.takeObject({ position = fanAt(i), flip = true })
      -- taking the second-to-last card turns the deck into its last card
      if lib.remainder ~= nil then lib = lib.remainder end
    else
      c = lib
      c.setPositionSmooth(fanAt(i), false, true)
    end
    if c then
      state.touched[c.getGUID()] = Time.time
      state.looked[c.getGUID()] = true
      out[#out + 1] = { guid = c.getGUID(), nick = c.getName(), desc = c.getDescription() }
    end
  end
  return { cards = out, order = 'top first', note = 'cards are in the secondary hand zone; move them back or keep them' }
end

-- show cards to everyone: announced in chat and laid face up on the stack spot
-- Tutor: take up to n cards whose name/type line matches query out of
-- Claude's library and put them where the effect says, then shuffle. Only the
-- cards taken are reported (plus how many matched), never the rest of the
-- library, so this reveals no more than a real search would.
function ops.search(req)
  local seat, d = myData()
  local lib = libraryTop(d)
  if lib == nil then error('library is empty') end
  local q = string.lower(tostring(req.query or ''))
  if q == '' then error('query required') end
  local n = tonumber(req.n) or 1
  local to = req.to or 'battlefield'
  -- plain-name cards (other importers): name is the card name, description is
  -- "{cost}\nType line\n\nOracle text". Match name + the header lines
  -- (everything before the first blank line) unless req.desc asks for all.
  local function haystack(name, desc)
    local dsc = desc or ''
    if not req.desc then dsc = dsc:match('^(.-)\n\n') or dsc end
    return string.lower(name or '') .. '\n' .. string.lower(dsc)
  end
  local function matching()
    local out = {}
    if lib.type == 'Deck' then
      -- cards still inside an imported deck have no GUIDs: take by index
      for _, c in ipairs(lib.getObjects()) do
        if string.find(haystack(c.name, c.description), q, 1, true) then out[#out + 1] = c end
      end
    elseif string.find(haystack(lib.getName(), lib.getDescription()), q, 1, true) then
      out[1] = { index = 0, name = lib.getName() }
    end
    return out
  end
  local matched = #matching()
  local taken = {}
  for i = 1, math.min(n, matched) do
    local c
    if lib.type == 'Deck' and lib.getQuantity() > 1 then
      local m = matching()[1] -- indices shift after every take: rescan
      if m == nil then break end
      local p = d.libraryZone.getPosition()
      c = lib.takeObject({ index = m.index, position = { p.x, p.y + 3, p.z }, smooth = false })
    else
      c = lib
    end
    if c then
      state.touched[c.getGUID()] = Time.time
      if to == 'hand' then
        c.deal(1, seat)
      else
        c.setPosition(rowSlot(d, req.row or 'other'))
        c.setRotation({ 0, yawOf(d.playmat) + (req.tapped and 90 or 0), 0 })
      end
      taken[#taken + 1] = { guid = c.getGUID(), nick = c.getName(), desc = c.getDescription() }
    end
  end
  local left = libraryTop(d)
  if not req.noShuffle and left and left.type == 'Deck' then left.shuffle() end
  state.looked = {}
  return { taken = taken, matched = matched, to = to, shuffled = not req.noShuffle }
end

-- Write on-card state (Encoder) onto one of Claude's own cards, the same way
-- the right-click buttons do it. Deltas for counters, names for keywords.
function ops.mark(req)
  local seat, d = myData()
  local obj = myObject(seat, d, req.guid)
  local enc = encoder()
  if enc == nil then error('this table has no Encoder (on-card counters unavailable); use counter tiles') end
  if not enc.call('APIobjectExists', { obj = obj }) then enc.call('APIencodeObject', { obj = obj }) end
  local done = {}
  local function easyDelta(var, display, delta)
    delta = tonumber(delta) or 0
    if delta == 0 then return end
    local pid = '_MTG_Simplified_UNIFIED'
    if not enc.call('APIobjIsPropEnabled', { obj = obj, propID = pid }) then enc.call('APIobjEnableProp', { obj = obj, propID = pid }) end
    -- same update the module's click handlers do (UpdateEncoderDataValue),
    -- done here because object refs do not survive a cross-object .call
    local pd = enc.call('APIobjGetPropData', { obj = obj, propID = pid })
    local tu = pd and pd.tyrantUnified
    if type(tu) ~= 'table' then error('card has no unified data; is it a real card?') end
    tu[var] = (tonumber(tu[var]) or 0) + delta
    if display and tu[display] == false then tu[display] = true end
    pd.tyrantUnified = tu
    enc.call('APIobjSetPropData', { obj = obj, propID = pid, data = pd })
    enc.call('APIrebuildButtons', { obj = obj })
    done[#done + 1] = var .. ' ' .. (delta > 0 and '+' or '') .. delta .. ' = ' .. tostring(tu[var])
  end
  easyDelta('plusOneCounters', 'displayPlusOne', req.plus_one)
  easyDelta('namedCounters', 'displayCounters', req.counters)
  easyDelta('namedCounters', 'displayCounters', req.loyalty)
  easyDelta('power', 'displayPowTou', req.power)
  easyDelta('toughness', 'displayPowTou', req.toughness)
  for _, kw in ipairs(req.keywords or {}) do
    local id = KEYWORD_IDS[string.lower(kw)]
    if id == nil then error('unknown keyword ' .. tostring(kw)) end
    local pid = 'πKeywords'
    if not enc.call('APIobjIsPropEnabled', { obj = obj, propID = pid }) then enc.call('APIobjEnableProp', { obj = obj, propID = pid }) end
    local prop = enc.call('APIgetProp', { propID = pid })
    if req.remove_keywords then
      local data = enc.call('APIobjGetPropData', { obj = obj, propID = pid })
      if type(data[id]) == 'number' then data[id] = 0 else data[id] = false end
      local icons = {}
      for _, k in ipairs(data.activeIcons or {}) do if k ~= id then icons[#icons + 1] = k end end
      data.activeIcons = icons
      enc.call('APIobjSetPropData', { obj = obj, propID = pid, data = data })
      enc.call('APIrebuildButtons', { obj = obj })
      done[#done + 1] = 'removed ' .. kw
    else
      prop.funcOwner.call('toggleStatus' .. id, obj)
      done[#done + 1] = 'keyword ' .. kw
    end
  end
  if req.note ~= nil then
    local pid = 'πotepad'
    if not enc.call('APIobjIsPropEnabled', { obj = obj, propID = pid }) then enc.call('APIobjEnableProp', { obj = obj, propID = pid }) end
    local data = enc.call('APIobjGetPropData', { obj = obj, propID = pid }) or {}
    if type(data.note) ~= 'table' then data.note = {} end
    data.note.text = tostring(req.note)
    enc.call('APIobjSetPropData', { obj = obj, propID = pid, data = data })
    enc.call('APIrebuildButtons', { obj = obj })
    done[#done + 1] = 'note set'
  end
  if req.phased ~= nil then
    local pid = 'MTG_Phase'
    local cur = enc.call('APIobjIsPropEnabled', { obj = obj, propID = pid }) and (enc.call('APIobjGetPropData', { obj = obj, propID = pid }) or {}).mtg_phased == true
    if cur ~= (req.phased == true) then
      local prop = enc.call('APIgetProp', { propID = pid })
      prop.funcOwner.call('phaseOut', obj)
    end
    done[#done + 1] = req.phased and 'phased out' or 'phased in'
  end
  if req.token ~= nil then
    local pid = 'MTG_Token'
    local cur = enc.call('APIobjIsPropEnabled', { obj = obj, propID = pid }) and (enc.call('APIobjGetPropData', { obj = obj, propID = pid }) or {}).mtg_token == true
    if cur ~= (req.token == true) then
      local prop = enc.call('APIgetProp', { propID = pid })
      prop.funcOwner.call('tToken', obj)
    end
    done[#done + 1] = req.token and 'marked token' or 'unmarked token'
  end
  return { on = obj.getName():match('^[^\n]*'), done = done, now = encoderInfo(obj) }
end

-- Reveal cards from the top of Claude's library until one matches (cascade,
-- reveal until you hit a creature...). Cards land face-up on the stack row in
-- order; the first match is reported separately. Move them afterwards (cast
-- it, bottom the rest...) with move().
function ops.reveal_until(req)
  local seat, d = myData()
  local q = string.lower(tostring(req.query or ''))
  if q == '' then error('query required') end
  local maxN = tonumber(req.max) or 20
  local maxCMC = tonumber(req.max_cmc)
  local revealed, hit = {}, nil
  for i = 1, maxN do
    local lib = libraryTop(d)
    if lib == nil then break end
    local pos = stackPoint(d, ((i - 1) % 9) - 4 + math.floor((i - 1) / 9) * 0.35)
    local c
    if lib.type == 'Deck' and lib.getQuantity() > 1 then
      c = lib.takeObject({ position = pos, rotation = { 0, yawOf(d.playmat), 0 }, smooth = false })
    else
      c = lib
      c.setPosition(pos)
      c.setRotation({ 0, yawOf(d.playmat), 0 })
    end
    if c == nil then break end
    state.touched[c.getGUID()] = Time.time
    local name, desc = c.getName(), c.getDescription()
    local hay = string.lower(name .. '\n' .. (desc:match('^(.-)\n\n') or desc))
    -- mana value: klrmngr cards say "3CMC" in the nick; plain-name cards put the
    -- cost line first in the description ("{2}{G}")
    local cmc = tonumber((name .. ' ' .. desc):match('(%d+)%s*CMC'))
    if cmc == nil then
      cmc = 0
      local cost = desc:match('^([^' .. string.char(10) .. ']*)') or ''
      for sym in cost:gmatch('{([^}]+)}') do
        local n = tonumber(sym:match('^(%d+)'))
        if n then cmc = cmc + n elseif sym ~= 'X' and sym ~= 'Y' then cmc = cmc + 1 end
      end
    end
    local entry = { guid = c.getGUID(), nick = name, desc = desc, cmc = cmc }
    revealed[#revealed + 1] = entry
    if string.find(hay, q, 1, true) and (maxCMC == nil or cmc < maxCMC) then hit = entry; break end
  end
  return { revealed = revealed, hit = hit, note = 'cards are face-up on the stack row; move() them where the effect says' }
end

-- New game: destroy everything of Claude's (cards, decks, tokens, counter
-- tiles) in its zones and hands, restore life to 40 and zero the commander
-- damage trackers through the table's own reset, so a fresh import_deck can
-- follow. Only Claude's seat is touched.
function ops.reset_board(req)
  local seat, d = myData()
  local gone, seen = 0, {}
  local function wipe(objs, cardsOnly)
    for _, o in ipairs(objs or {}) do
      if o ~= nil and not seen[o.getGUID()] and not o.getLock() and o.getGUID() ~= self.getGUID() then
        local t = o.type
        local ok = (t == 'Card' or t == 'Deck')
        if not cardsOnly then ok = ok or t == 'Tile' or t == 'Generic' or t == 'Chip' or t == 'Figurine' or t == 'Dice' end
        if ok and o.getName() ~= 'Highlight Mat' then
          seen[o.getGUID()] = true
          state.touched[o.getGUID()] = Time.time
          pcall(function() destroyObject(o) end)
          gone = gone + 1
        end
      end
    end
  end
  for _, k in ipairs({ 'libraryZone', 'graveyard', 'exileZone', 'commandZone' }) do
    if d[k] then wipe(d[k].getObjects(), true) end
  end
  if d.playmat then wipe(d.playmat.getObjects(), false) end
  for _, h in ipairs(handZones(seat)) do wipe(handObjects(seat, h.idx), true) end
  if d.adapted then
    -- this table has no reset of its own: life back to 40, tally cleared
    if d.lifeTracker then setLife(d.lifeTracker, 40) end
    state.cmdr[seat] = nil
  else
    pcall(function() Global.call('resetLifeAndCommanderDamage', seat) end)
    pcall(function() Global.call('resetMulliganCount', seat) end)
  end
  state.looked = {}
  state.claudeTurn = false
  Turns.enable = true
  return { seat = seat, removed = gone, life = 40 }
end

function ops.reveal(req)
  local seat, d = myData()
  local names = {}
  local i = 0
  for _, g in ipairs(req.guids or {}) do
    local obj = myObject(seat, d, g)
    faceUpAt(obj, stackPoint(d, i - 1 - (#req.guids - 1) / 2), yawOf(d.playmat))
    names[#names + 1] = obj.getName():match('^[^\n]*')
    i = i + 1
  end
  broadcastToAll('[' .. NAME .. '] reveals: ' .. table.concat(names, ', '), Color.fromString(seat))
  return { revealed = names }
end

-- write a life tracker (both tables' trackers keep `count`, show it on
-- button 0 and persist through updateSave)
function setLife(tr, v)
  tr.setVar('count', v)
  tr.editButton({ index = 0, label = tostring(v) })
  pcall(function() tr.call('updateSave') end)
  return v
end

-- life change on ANY seat (Claude deals damage too). Mirrors Global.loseLife.
function ops.life(req)
  requireSeat()
  local color = req.color
  local delta = tonumber(req.delta) or 0
  local d = seatData(color)
  if d == nil or d.lifeTracker == nil then error('no life tracker for ' .. tostring(color)) end
  local tr = d.lifeTracker
  local count = setLife(tr, (tonumber(tr.getVar('count')) or 0) + delta)
  local verb = delta < 0 and 'loses' or 'gains'
  printToAll(color .. ' ' .. verb .. ' ' .. math.abs(delta) .. ' life (' .. NAME .. ') -> ' .. count, stringColorToRGB(color))
  return { color = color, life = count }
end

-- new game: set a seat's life back to a value (default 40)
function ops.reset_life(req)
  requireSeat()
  local d = seatData(req.color)
  if d == nil or d.lifeTracker == nil then error('no life tracker for ' .. tostring(req.color)) end
  local v = setLife(d.lifeTracker, tonumber(req.value) or 40)
  -- a new game for this seat: the pad's commander damage tally goes too
  if d.adapted then state.cmdr[req.color] = nil end
  return { color = req.color, life = v }
end

-- commander damage: `to` took `delta` from `from`'s commander
function ops.cmdr_damage(req)
  requireSeat()
  local d = seatData(req.to)
  if d == nil then error('no seat ' .. tostring(req.to)) end
  if d.adapted then
    -- no per-victim trackers on this table: the pad keeps the tally and
    -- takes the damage off the victim's life tracker, like the 4-seat
    -- table's auto-life setting does
    local from = tostring(req.from)
    local delta = tonumber(req.delta) or 0
    state.cmdr[req.to] = state.cmdr[req.to] or {}
    local old = state.cmdr[req.to][from] or 0
    local v = old + delta
    if v < 0 then v = 0 end
    state.cmdr[req.to][from] = v
    local applied = v - old
    local life = nil
    if applied ~= 0 and d.lifeTracker then
      life = setLife(d.lifeTracker, (tonumber(d.lifeTracker.getVar('count')) or 0) - applied)
    end
    printToAll(req.to .. ' took ' .. applied .. ' commander damage from ' .. from .. ' (total ' .. v .. ')'
      .. (life and (', life ' .. life) or ''), stringColorToRGB(req.to))
    return { to = req.to, from = from, total = v, lifeAutoDeducted = life ~= nil, life = life }
  end
  for _, ref in ipairs(d.commanderDamage or {}) do
    local tr = type(ref) == 'string' and getObjectFromGUID(ref) or ref
    if tr and tr.getDescription() == req.from then
      local old = tonumber(tr.getVar('val')) or 0
      local v = old + (tonumber(req.delta) or 0)
      if v < 0 then v = 0 end
      if v > 21 then v = 21 end
      tr.setVar('val', v)
      pcall(function() tr.call('updateVal') end)
      pcall(function() tr.call('updateSave') end)
      -- same path as clicking the tracker: the table's commanderDamageDealt
      -- takes the damage off the life total when cmdrDamageAutoLife is on,
      -- so the caller must NOT also adjust_life for it.
      -- Global.getSetting(color, key) takes two args, which .call cannot pass:
      -- replicate it from the settings tables (enforced > per-player > default)
      local autoLife = false
      pcall(function()
        local enforced = Global.getTable('enforcedSettings') or {}
        local per = Global.getTable('playerSettings') or {}
        local defaults = Global.getTable('settingsDefaults') or {}
        local key = 'cmdrDamageAutoLife'
        if enforced[key] and enforced[key].enforced then autoLife = enforced[key].value == true
        elseif per[req.to] and per[req.to][key] ~= nil then autoLife = per[req.to][key] == true
        else autoLife = defaults[key] == true end
      end)
      if v ~= old then
        pcall(function() Global.call('commanderDamageDealt', { guid = tr.getGUID(), delta = v - old }) end)
      end
      return { to = req.to, from = req.from, total = v, lifeAutoDeducted = autoLife }
    end
  end
  error('no commander damage tracker for ' .. tostring(req.from) .. ' -> ' .. tostring(req.to))
end

-- drop a counter/token from one of the table's infinite bags onto a card
function ops.counter(req)
  local seat, d = myData()
  local obj = myObject(seat, d, req.guid)
  local want = string.lower(tostring(req.kind or '+X/+Y Counter'))
  local bag, loose = nil, nil
  for _, o in ipairs(getAllObjects()) do
    if o.type == 'Infinite' then
      local n = string.lower(o.getName())
      if n == want then bag = o end
      if loose == nil and string.find(n, want, 1, true) then loose = o end
    end
  end
  bag = bag or loose
  if bag == nil then error('no bag named ' .. want) end
  local p = obj.getPosition()
  local n = tonumber(req.n) or 1
  local spawned = {}
  -- the table's +X/+Y tile keeps its value in valP/valT; set it once its
  -- script has loaded (a frame or so after takeObject) if a value was asked for
  local setP, setT = tonumber(req.p), tonumber(req.t)
  for i = 1, n do
    -- smooth=false: a gliding token would trip zone events on the way over
    local t = bag.takeObject({ position = { p.x + (i - 1) * 0.6 - 0.3 * (n - 1), p.y + 1 + i * 0.2, p.z + 0.8 }, smooth = false })
    if t then
      state.touched[t.getGUID()] = Time.time
      spawned[#spawned + 1] = t.getGUID()
      if setP or setT then
        Wait.frames(function()
          pcall(function()
            if setP then t.setVar('valP', setP) end
            if setT then t.setVar('valT', setT) end
            t.call('updateVal')
            t.call('updateSave')
          end)
        end, 15)
      end
    end
  end
  return { on = obj.getName(), bag = bag.getName(), guids = spawned, p = setP, t = setT }
end

-- spawn a TTS object (ndjson line from the importer) onto my 'other' row
function ops.token(req)
  local _, d = myData()
  if type(req.json) ~= 'string' then error('json required') end
  local pos = rowSlot(d, req.row or 'creature')
  local o = spawnObjectJSON({ json = req.json, position = pos, rotation = { 0, yawOf(d.playmat), 0 } })
  if o then state.touched[o.getGUID()] = Time.time end
  return { guid = o and o.getGUID() or nil }
end

-- Deck import through the importer the MTG tables use (rikrassen's; the URL
-- comes from nemesis-mcp). Async: emits {ev='import', ...} when done.
-- req.commanders names the commander(s) so the piles can be sorted whatever
-- order the server sends them in.
function ops.import(req)
  local seat, d = myData()
  local url = (type(req.url) == 'string' and req.url ~= '') and req.url or IMPORTER
  local body = JSON.encode({
    data = tostring(req.data or ''), useStates = true, preferOriginalPrinting = false,
    hand = Player[seat].getHandTransform(1),
  })
  WebRequest.custom(url .. '/build', 'POST', true, body, {
    ['Content-Type'] = 'application/json', Accept = 'application/x-ndjson',
    ['X-Client-Version'] = 'Nemesis/' .. NEMESIS_VERSION,
  }, function(resp) onImport(resp, seat, d, req.commanders) end)
  setStatus('Importing a deck for ' .. seat .. '...')
  return { started = true, importer = url }
end

local function cardName(nick)
  return ((nick or ''):match('^[^\n]*') or ''):lower()
end

function onImport(resp, seat, d, commanders)
  if resp.error ~= nil then
    local msg = resp.error
    if resp.text and string.find(resp.text, '"error"', 1, true) then
      local ok, body = pcall(function() return JSON.decode(resp.text) end)
      if ok and type(body) == 'table' and body.error then msg = body.error end
    end
    setStatus('Import failed')
    emit({ ev = 'import', ok = false, error = msg })
    return
  end
  local issues, lines = {}, {}
  for line in string.gmatch(resp.text, '[^\r\n]+') do
    if string.sub(line, 1, 9) == '{"error":' then
      local ok, e = pcall(function() return JSON.decode(line) end)
      if ok and e.error then issues[#issues + 1] = e.error end
    else
      lines[#lines + 1] = line
    end
  end
  -- Sort the piles: a pile made only of the named commanders goes to the
  -- command zone, the biggest of the rest is the library, anything else is
  -- a sideboard. Without names, lone cards beside a bigger pile are taken
  -- for commanders.
  local cmdr = {}
  for _, n in ipairs(commanders or {}) do cmdr[tostring(n):lower()] = true end
  local haveNames = next(cmdr) ~= nil
  local kind, sizes = {}, {}
  for i, line in ipairs(lines) do
    local okd, obj = pcall(function() return JSON.decode(line) end)
    local names = {}
    if okd and type(obj) == 'table' then
      if type(obj.ContainedObjects) == 'table' and #obj.ContainedObjects > 0 then
        for _, c in ipairs(obj.ContainedObjects) do names[#names + 1] = cardName(c.Nickname) end
      else
        names[1] = cardName(obj.Nickname)
      end
    end
    sizes[i] = #names
    local allCmdr = #names > 0
    for _, n in ipairs(names) do if not cmdr[n] then allCmdr = false end end
    if haveNames and allCmdr then kind[i] = 'command' end
  end
  local libIdx = nil
  for i = 1, #lines do
    if kind[i] == nil and (libIdx == nil or sizes[i] > sizes[libIdx]) then libIdx = i end
  end
  for i = 1, #lines do
    if kind[i] == nil and i ~= libIdx then
      kind[i] = (not haveNames and sizes[i] == 1 and libIdx and sizes[libIdx] > 1) and 'command' or 'sideboard'
    end
  end
  if libIdx then kind[libIdx] = 'library' end
  local f = d.playmat.getTransformForward()
  for i, line in ipairs(lines) do
    local pos
    if kind[i] == 'library' then
      local p = d.libraryZone.getPosition(); pos = { p.x, p.y + 2, p.z }
    elseif kind[i] == 'command' then
      local p = d.commandSpot or d.commandZone.getPosition(); pos = { p.x, p.y + 2 + i, p.z }
    else
      -- sideboard: lifted, between library and the mat
      local p = d.libraryZone.getPosition()
      local r = d.playmat.getTransformRight()
      pos = { p.x + r.x * 4, p.y + 2, p.z + r.z * 4 }
    end
    local o = spawnObjectJSON({ json = line, position = pos })
    if o then state.touched[o.getGUID()] = Time.time end
  end
  state.looked = {}
  -- the server builds decks in list order: shuffle once the deck object has
  -- settled (a shuffle on a still-spawning deck is silently dropped, which
  -- handed Claude the first 7 cards of its list twice), then once more later
  local function shuffleLib()
    local lib = libraryTop(d)
    if lib and lib.type == 'Deck' then lib.shuffle() end
    return lib
  end
  Wait.condition(function()
    shuffleLib()
    Wait.time(shuffleLib, 2)
    setStatus('Deck ready for ' .. seat .. ' (' .. #lines .. ' piles)')
    emit({ ev = 'import', ok = true, piles = #lines, problems = table.concat(issues, '; ') })
  end, function()
    local lib = libraryTop(d)
    return lib ~= nil and lib.type == 'Deck' and not lib.spawning and lib.resting
  end, 10, function()
    shuffleLib()
    emit({ ev = 'import', ok = true, piles = #lines, problems = 'deck did not settle in 10s; shuffled anyway' })
  end)
end

-- begin_turn calls this: a turn the table handed over in chat ("you go
-- first") never went through the marker, and end_turn needs to know
function ops.start_turn(req)
  requireSeat()
  if state.claudeTurn then return { started = false, already = true } end
  startClaudeTurn(Turns.turn_color)
  return { started = true }
end

function ops.end_turn(req)
  local seat = requireSeat()
  if not state.claudeTurn and not req.force then error("it isn't my turn (marker on " .. tostring(Turns.turn_color) .. ")") end
  state.claudeTurn = false
  local nxt = neighbourSeat(1)
  if nxt == nil then error('nobody else is seated') end
  state.justEnded = Time.time
  state.passedTo = nxt
  Turns.enable = true
  Turns.turn_color = nxt
  setStatus(nxt .. "'s turn")
  return { passedTo = nxt }
end

---------------------------------------------------------------------------
-- attacks: creatures tapped on their controller's turn
---------------------------------------------------------------------------
-- Tables tap cards in different ways (rotate keys, their own scripts), so the
-- pad looks rather than listens: every 1.5 s it reads the active human's
-- creatures. One that was seen untapped this turn and is now tapped joins a
-- batch; 3 s after the last one the batch goes out as ev=attack. Mana
-- creatures ("{T}: Add") and the land row are left out, and creatures that
-- arrive tapped never count. Events stay flat, so the cards are a JSON string.
state.tapWatch = { seat = nil, untapped = {} }
state.taps = {}

function pollTaps()
  local w = state.tapWatch
  local seat = Turns.turn_color
  if state.seat == nil or state.claudeTurn or not Turns.enable or seat == nil or seat == '' or seat == state.seat then
    w.seat = nil
    return
  end
  local d = seatData(seat)
  if d == nil or d.playmat == nil then return end
  if w.seat ~= seat then w.seat = seat; w.untapped = {} end
  local land = {}
  if d.landZone then for _, o in ipairs(d.landZone.getObjects()) do land[o.getGUID()] = true end end
  local newly = {}
  for _, o in ipairs(pieces(d.playmat)) do
    local g = o.getGUID()
    if o.type == 'Card' and not land[g] and o.held_by_color == nil and not isFaceDown(o) then
      local nick, desc = o.getName(), o.getDescription()
      if (nick .. '\n' .. desc):find('Creature') and not desc:find('{T}: Add') then
        if isTapped(o, d.playmat) then
          if w.untapped[g] then
            newly[#newly + 1] = { guid = g, name = nick, desc = desc }
            w.untapped[g] = nil
          end
        else
          w.untapped[g] = true
        end
      end
    end
  end
  if #newly == 0 then return end
  local b = state.taps[seat]
  if b == nil then
    b = { cards = {} }
    state.taps[seat] = b
  end
  for _, c in ipairs(newly) do b.cards[#b.cards + 1] = c end
  b.t = Time.time
  local stamp = b.t
  Wait.time(function()
    local cur = state.taps[seat]
    if cur ~= b or cur.t ~= stamp then return end -- more taps came in
    state.taps[seat] = nil
    local pl = Player[seat]
    emit({ ev = 'attack', color = seat, player = pl and pl.steam_name or '', cards = JSON.encode(b.cards) })
  end, 3)
end

-- what is already on each battlefield when the pad loads is not a new play
function seedField()
  for _, c in ipairs(COLORS) do
    local d = seatData(c)
    if d and d.playmat then
      for _, o in ipairs(pieces(d.playmat)) do
        if o.type == 'Card' then state.fieldOf[o.getGUID()] = c end
      end
    end
  end
end
