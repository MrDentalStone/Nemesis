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
NEMESIS_VERSION = 'v1.4.0'

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
      if s.decisionPads == false then state.decisionPads = false end
      if type(s.draft) == 'table' then
        state.draft = s.draft
        -- Time.time starts again at load
        for _, h in pairs(state.draft.hands or {}) do h.since = 0 end
      end
    end
  end
  pcall(refreshColors)
  if state.seat then statusText = NAME .. ' is ' .. state.seat end
  if state.claudeTurn then Turns.enable = false; statusText = NAME .. "'s turn" end
  buildControls()
  emit({ ev = 'pad', what = 'loaded', seat = state.seat })
  Wait.time(function() pcall(seedField) end, 1)
  -- rounds do not survive a reload, so neither do their pads
  Wait.time(function()
    if stale() then return end
    pcall(removeDecisionPads); pcall(ensureAttackPad)
    -- a human mid-Winston-pick gets their pad back
    local dr = state.draft
    if dr and dr.format == 'winston' and dr.phase == 'picking' and dr.view and dr.turn ~= state.seat then
      dr.pad = nil
      Wait.time(function() pcall(function() showWinstonPad(dr.turn, dr.pile, #dr.view, wNextPileFor(dr.pile + 1) == nil) end) end, 1)
    end
  end, 2)
  local tapTimer
  local tick = 0
  tapTimer = Wait.time(function()
    if stale() then Wait.stop(tapTimer) return end
    pcall(pollTaps)
    pcall(pollDraft)
    tick = tick + 1
    if tick % 2 == 0 then pcall(pollMana) end
  end, 1.5, -1)
end

function onSave()
  return JSON.encode({ seat = state.seat, stopped = state.stopped, claudeTurn = state.claudeTurn == true, cmdr = state.cmdr,
    mode = state.mode, newPlayers = state.newPlayers, name = NAME, modes = MODES, roundWait = state.roundWait, draft = state.draft,
    decisionPads = state.decisionPads })
end

---------------------------------------------------------------------------
-- outbound events (-> nemesis-mcp on :39998)
---------------------------------------------------------------------------

-- Every event gets an epoch (this pad load) and a sequence number and is kept
-- in a ring, so a listener that was not running at the time (the MCP server
-- between sessions, the CLI between calls) can fetch what it missed with the
-- events op instead of losing chat and button presses.
ops = ops or {}
-- The epoch only ever grows, even for two loads in the same second (the pad
-- updating itself), so the server can tell an older load's events apart.
state.evEpoch = math.floor(os.time())
pcall(function()
  local last = tonumber(Global.getVar('nemesisPadEpoch')) or 0
  if state.evEpoch <= last then state.evEpoch = last + 1 end
  Global.setVar('nemesisPadEpoch', state.evEpoch)
end)
state.evSeq = 0
state.evBuf = {}
local EV_RING = 300

-- A script replaced in place (setLuaScript + reload, the pad updating itself)
-- can leave its repeating timers running in the old instance, which then
-- emit stale duplicates (every attack seen once per old load). Each load
-- claims the newest epoch in Global; an instance whose epoch is not the
-- newest is stale and goes quiet.
function stale()
  local ok, cur = pcall(function() return tonumber(Global.getVar('nemesisPadEpoch')) end)
  return ok and cur ~= nil and cur ~= state.evEpoch
end

function emit(ev)
  if stale() then return end
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
  draftChat(player.color, message)
end

-- Turns. TTS's turn system bounces the marker straight off a colour with no
-- seated player, so Claude's turn is virtual: the pad freezes the marker
-- (Turns.enable=false) while Claude plays and hands it to the next seated
-- player on end_turn. Seat order follows the table's colour order.
local function seatIndex(color)
  for i, c in ipairs(COLORS) do if c == color then return i end end
  return 1
end

-- The tables mark a player who is out (dead or scooped) with a Turn Skipper
-- puck on their mat; its own script skips their turn, but it has thrown Lua
-- errors on some tables, so the pad passes over such a seat itself.
function seatSkipped(color)
  local ok, out = pcall(function()
    local d = seatData(color)
    if d == nil or d.playmat == nil then return false end
    for _, o in ipairs(zoneObjects(d.playmat)) do
      if string.sub(o.getName(), 1, 12) == 'Turn Skipper' then return true end
    end
    return false
  end)
  return ok and out
end

-- next/previous SEATED colour after/before Claude's slot, passing over seats
-- the table has marked out with a Turn Skipper
function neighbourSeat(dir)
  local i = seatIndex(state.seat)
  for step = 1, #COLORS do
    local c = COLORS[((i - 1 + dir * step) % #COLORS) + 1]
    if Player[c].seated and not seatSkipped(c) then return c end
  end
  return nil
end

function startClaudeTurn(prev)
  if state.seat == nil then return end
  state.claudeTurn = true
  Turns.enable = false
  pcall(clearAttackCall)
  broadcastToAll("[Nemesis] " .. NAME .. "'s turn (" .. state.seat .. ")", Color.fromString(state.seat))
  setStatus(NAME .. "'s turn")
  -- the first turn after reset_board: the table may not have settled who
  -- goes first yet (a marker pass at set-up is not always a real turn)
  local first = state.gameTurns == 0
  if state.gameTurns then state.gameTurns = state.gameTurns + 1 end
  emit({ ev = 'turn', color = state.seat, prev = prev, claude = true, first = first })
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
  pcall(clearAttackCall)
  if state.draft and state.draft.phase == 'done' then endDraft() end -- the first game turn after a draft
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
  if state.claudeTurn then
    -- the marker is frozen during Claude's turn, so a human switched turns
    -- back on and moved it: the table took the turn back (a wrong start,
    -- "Mickle goes first"). Without this the pad still thought it was
    -- Claude's turn and let every later pass skip Claude's seat.
    state.claudeTurn = false
    setStatus(player.color .. "'s turn")
    emit({ ev = 'turn', color = player.color, prev = prev, tookBack = true })
    return
  end
  if state.seat and passesClaude(prev, player.color) then
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

-- A locked card or pile is a table script's prop, not a card in play: MTG
-- Lotus Helper (the (S) table's 2026-10 update) floats locked preview basics
-- in front of every fetch land (Evolving Wilds: a Forest, Mountain and Island
-- in the deck's colours) and deletes them when the land leaves. Players'
-- cards are never locked.
function isTableProp(obj)
  if obj == nil or (obj.type ~= 'Card' and obj.type ~= 'Deck') then return false end
  local ok, locked = pcall(function() return obj.getLock() end)
  return ok and locked == true
end

local function noteZone(object, dir, key)
  -- Nemesis's own objects (decision and attack pads, the set-up tile) are no news
  local okp, ours = pcall(function() return object.getVar('nemesisDecisionPad') or object.getVar('nemesisAttackPad') or object.getVar('nemesisSetupTile') end)
  if okp and ours then return end
  if isTableProp(object) then return end
  local g = object.getGUID()
  local m = state.zoneMoves[g]
  if m == nil then
    m = { left = {}, name = object.getName():match('^[^\n]*'), type = object.type }
    state.zoneMoves[g] = m
    local function settle()
      state.zoneMoves[g] = nil
      if recentlyTouched(object) then return end
      if isTableProp(object) then return end -- locked a moment after it spawned
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
  if draftQuiet() then return end -- picks and packs are not plays
  if zone and zone.type == 'Hand' then state.fieldOf[object.getGUID()] = nil end
  local seat, key = zoneOwner(zone)
  if seat == nil then return end
  if key == 'graveyard' or key == 'exileZone' or key == 'libraryZone' or key == 'commandZone' then
    state.fieldOf[object.getGUID()] = nil
  end
  local cn = state.countered[object.getGUID()]
  if cn and key == 'graveyard' and seat == cn.owner then counterSettled(object, seat, true, 'hand') end
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
  if draftQuiet() then return end
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
state.playBy = {}     -- guid -> seat that dropped it on another seat's mat (its caster)
local PLAY_TO = { playmat = 'battlefield', landZone = 'battlefield', graveyard = 'graveyard', exileZone = 'exile' }

function watchPlay(seat, key, obj)
  if PLAY_TO[key] == nil or obj.type ~= 'Card' then return end
  if isTableProp(obj) then return end
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
  -- who put it there: usually the seat itself, but a player can drop a spell
  -- aimed at someone onto that player's mat (a Cyclonic Rift on its target)
  local by = obj.held_by_color
  if by == nil or by == '' then
    local h = state.fromHuman[guid]
    if h and Time.time - h.t < 15 and PLAY_TO[h.key] == nil then by = h.seat end -- left a hand or zone
  end
  if by == seat or by == state.seat or not seatData(by or '') then by = nil end
  local origin = from and from.key or 'hand'
  if origin == 'libraryZone' then origin = 'library' elseif origin == 'commandZone' then origin = 'command' elseif origin == 'exileZone' then origin = 'exile' end
  -- wait until nobody holds it and it is at rest, still in the zone
  Wait.condition(function()
    if obj == nil or obj.isDestroyed() or isTableProp(obj) then return end
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
      for _, o in ipairs(zoneObjects(d.landZone)) do if o.getGUID() == guid then land = true end end
    end
    local pl = Player[by or seat]
    if by then state.playBy[guid] = by end
    emit({ ev = 'play', color = by or seat, onto = by and seat or nil, player = pl and pl.steam_name or '', guid = guid,
      from = origin, to = PLAY_TO[key], landRow = land, faceDown = face,
      name = face and '(face-down card)' or obj.getName(), desc = face and '' or obj.getDescription(),
      claudeTurn = state.claudeTurn == true })
  end, function()
    return obj == nil or obj.isDestroyed() or (obj.held_by_color == nil and obj.resting)
  end, 30)
end

---------------------------------------------------------------------------
-- holding the table for a response (interrupts)
---------------------------------------------------------------------------
-- The humans have no stack: a spell is a card that lands on their mat. When
-- one lands that the AI could counter or copy, nemesis-mcp asks for a hold
-- the moment the play reaches it (before the model has read it): "Hold on",
-- and the card lights up. The model answers with a response (release
-- quiet=true, then its own spell) or a pass (release); if it is slow the hold
-- runs out by itself, so the table never waits on it for long.
-- A counter that resolves puts two buttons on the countered card. Only its
-- owner (or a host) can press them: Countered moves it to their graveyard,
-- Not countered keeps it (they had an answer of their own). The owner moving
-- the card to their graveyard by hand counts as Countered too.
state.hold = nil
state.countered = {} -- guid -> { owner, name }

-- a card's name for chat: TTS nicknames carry the type line after a newline
function displayName(nick)
  return (tostring(nick or ''):match('^[^\n]*') or '')
end

-- a card the pad reported as another player's play: api() only takes GUIDs
-- it handed out itself
local function reportedPlay(guid)
  if type(guid) ~= 'string' or (state.playSeen[guid] == nil and state.fieldOf[guid] == nil) then
    error('not a card the pad reported as a play')
  end
  local obj = getObjectFromGUID(guid)
  if obj == nil then error('that card is no longer on the table') end
  return obj
end

function ops.hold(req)
  local seat = requireSeat()
  local obj = reportedPlay(req.guid)
  local secs = tonumber(req.secs) or (state.roundWait > 0 and state.roundWait or 20)
  secs = math.max(5, math.min(secs, 60))
  local name = tostring(req.name or displayName(obj.getName()))
  local h = { guid = req.guid, name = name, secs = secs }
  state.hold = h
  local col = Color.fromString(seat)
  broadcastToAll('[' .. NAME .. '] Hold on: ' .. name, col)
  pcall(function() obj.highlightOn(col, secs) end)
  Wait.time(function()
    if state.hold ~= h then return end
    state.hold = nil
    printToAll('[' .. NAME .. '] OK, ' .. name .. ' resolves', col)
    emit({ ev = 'hold', result = 'expired', guid = h.guid, name = name, secs = secs })
  end, secs)
  return { held = req.guid, secs = secs }
end

function ops.release(req)
  local seat = requireSeat()
  local h = state.hold
  state.hold = nil
  if h == nil then return { released = false } end
  local obj = getObjectFromGUID(h.guid)
  if obj then pcall(function() obj.highlightOff() end) end
  if not req.quiet then
    local msg = (type(req.text) == 'string' and req.text ~= '') and req.text or ('OK, ' .. h.name .. ' resolves')
    printToAll('[' .. NAME .. '] ' .. msg, Color.fromString(seat))
  end
  return { released = h.guid, name = h.name }
end

local COUNTER_FNS = { onCounterAccept = true, onCounterRefuse = true }

local function removeCounterButtons(obj)
  local ok, list = pcall(function() return obj.getButtons() end)
  if not ok or list == nil then return end
  -- other scripts (the Encoder) keep their own buttons on cards: remove only
  -- ours, highest index first so the rest keep their places
  for i = #list, 1, -1 do
    if COUNTER_FNS[list[i].click_function] then pcall(function() obj.removeButton(list[i].index) end) end
  end
end

-- {guid}: the AI's counter resolved; the owner settles their card
function ops.countered(req)
  local seat = requireSeat()
  local obj = reportedPlay(req.guid)
  local owner = nil
  for _, z in ipairs(obj.getZones()) do
    local s = zoneOwner(z)
    if s and s ~= seat then owner = s end
  end
  -- dropped on someone else's mat: the caster owns the spell, not the mat
  if owner and state.playBy[req.guid] then owner = state.playBy[req.guid] end
  if owner == nil then
    -- picked up again: back in a hand (never named: hands stay hidden), or
    -- off every zone
    for _, z in ipairs(obj.getZones()) do
      if z.type == 'Hand' then return { inHand = true, name = displayName(obj.getName()) } end
    end
    error('that card is not in another player\'s zones any more')
  end
  local name = displayName(obj.getName())
  local entry = { owner = owner, name = name }
  state.countered[req.guid] = entry
  removeCounterButtons(obj)
  local face = isFaceDown(obj) and -1 or 1 -- buttons go on the side facing up
  local function btn(fn, label, z, color)
    obj.createButton({ click_function = fn, function_owner = self, label = label,
      position = { 0, 0.3 * face, z }, rotation = { 0, 0, face < 0 and 180 or 0 },
      width = 1100, height = 260, font_size = 150, scale = { 0.5, 1, 0.5 },
      color = color, font_color = { 1, 1, 1 }, tooltip = owner .. ' only' })
  end
  btn('onCounterAccept', 'Countered', -0.35, { 0.6, 0.15, 0.15 })
  btn('onCounterRefuse', 'Not countered', 0.35, { 0.25, 0.25, 0.25 })
  broadcastToAll('[' .. NAME .. '] ' .. name .. ' is countered. ' .. owner .. ': press Countered on it', Color.fromString(seat))
  -- a counter nobody settles should not leave buttons on the card all game
  Wait.time(function()
    if state.countered[req.guid] ~= entry then return end
    state.countered[req.guid] = nil
    local o = getObjectFromGUID(req.guid)
    if o then removeCounterButtons(o) end
    emit({ ev = 'countered', result = 'unanswered', guid = req.guid, name = name, color = owner })
  end, 120)
  return { owner = owner, name = name }
end

function counterSettled(obj, color, accepted, by)
  local g = obj.getGUID()
  local c = state.countered[g]
  removeCounterButtons(obj)
  if c == nil then return end
  state.countered[g] = nil
  if accepted and by ~= 'hand' then
    local d = seatData(c.owner)
    if d and d.graveyard then
      local p = d.graveyard.getPosition()
      obj.setPositionSmooth({ p.x, p.y + 2, p.z }, false, true)
      obj.setRotationSmooth({ 0, d.graveyard.getRotation().y, 0 }, false, true)
    end
  end
  emit({ ev = 'countered', result = accepted and 'accepted' or 'refused', guid = g, name = c.name, color = c.owner, by = color })
end

local function counterClick(obj, color, accepted)
  local c = state.countered[obj.getGUID()]
  if c == nil then removeCounterButtons(obj) return end
  local p = Player[color]
  if color ~= c.owner and not (p and (p.host or p.admin)) then
    broadcastToColor('[Nemesis] Only ' .. c.owner .. ' decides about their ' .. c.name .. '.', color, { 1, 0.7, 0.3 })
    return
  end
  counterSettled(obj, color, accepted, 'button')
end

function onCounterAccept(obj, color) counterClick(obj, color, true) end
function onCounterRefuse(obj, color) counterClick(obj, color, false) end

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

  -- row 3: decision pads on/off, how long silence waits, flow fallback;
  -- during a Winston draft, the human on turn takes or passes their pile here
  local wColor, wPile = winstonHumanTurn()
  if wColor then
    btn('Take pile ' .. wPile, 'onWinstonTake', -1.2, 0.35, 1100, { 0.15, 0.5, 0.2 }, wColor .. ': keep the pile in your hand')
    btn('Pass pile ' .. wPile, 'onWinstonPass', 1.2, 0.35, 1100, { 0.5, 0.3, 0.15 }, wColor .. ': put it back with one more card')
  else
    btn(state.decisionPads and 'Big pads: on' or 'Big pads: off', 'onToggleDecisionPads', -1.65, 0.35, 780, { 0.25, 0.35, 0.25 },
      'Decision pads: for a big decision (a board wipe, a huge threat) a Yes / No / Wait pad with a countdown appears on the mat of each player it is for. Chat answers always work.')
    btn(state.roundWait > 0 and ('Wait ' .. state.roundWait .. 's') or 'Wait: off', 'onRoundWait', 0, 0.35, 780, { 0.3, 0.25, 0.4 },
      'When ' .. NAME .. ' asks "any responses?", silence for this long counts as no responses. Say "wait" in chat to hold it.')
    btn('Your turn', 'onYourTurn', 1.65, 0.35, 780, { 0.16, 0.45, 0.7 }, 'Fallback: hand the turn to ' .. NAME .. ' (normally automatic)')
  end

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
  if changed then buildControls(); refreshAttackPad() end
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
  -- the pad and the attack pad move once the clicking stops, not under
  -- the cursor of someone still cycling colours
  local at = Time.time
  state.seatPickedAt = at
  Wait.time(function()
    if state.seatPickedAt ~= at then return end
    pcall(placePad); pcall(ensureAttackPad)
  end, 4)
end

-- Spots for the pad and the attack pad, from the mat's far edge (towards the
-- table centre) and its left edge as the player sees it (+right). Chosen at
-- the (S) table after its 2026-10 update: the old pad spot on the strip
-- between seats sat on the next seat's life tracker.
local PAD_SPOT = { fromFar = 1.6, fromLeft = 10.92 }          -- flush with the mat's far edge
local ATTACK_PAD_SPOT = { fromFar = -0.11, fromLeft = 6.69 }  -- on the divider just past it

function cornerSpot(mat, spot)
  local sc = mat.getScale()
  return -(sc.z / 2) + spot.fromFar, sc.x / 2 - spot.fromLeft
end

-- The pad's own spot at its seat: PAD_SPOT on the mat's far edge, turned to
-- face across the table; when the table has something there, the old spot
-- on the strip beside the mat. A locked pad stays put.
function placePad()
  if state.seat == nil or self.getLock() then return end
  local d = seatData(state.seat)
  if d == nil or d.playmat == nil then return end
  local mat = d.playmat
  local sc = mat.getScale()
  local a, r = cornerSpot(mat, PAD_SPOT)
  local b, f, rt = self.getBounds().size, mat.getTransformForward(), mat.getTransformRight()
  local ha = (math.abs(f.x) * b.x + math.abs(f.z) * b.z) / 2
  local hr = (math.abs(rt.x) * b.x + math.abs(rt.z) * b.z) / 2
  local okf, blocked = pcall(function()
    -- the pad itself is furniture: test the spot as if it were not there
    for _, o in ipairs(matFurniture(mat)) do
      if not o.pad and not o.mine and math.abs(o.a - a) < o.ha + ha and math.abs(o.r - r) < o.hr + hr then return true end
    end
    return false
  end)
  if okf and blocked then a, r = -(sc.z / 2) + 3.75, sc.x / 2 + 2.75 end
  local p = matWorld(mat, a, r)
  local cur = self.getPosition()
  if math.abs(cur.x - p.x) < 1 and math.abs(cur.z - p.z) < 1 then return end
  self.setRotation({ 0, mat.getRotation().y + 180, 0 })
  self.setPositionSmooth({ p.x, cur.y + 1.5, p.z }, false, true)
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
    pcall(placePad); pcall(ensureAttackPad)
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
-- decision pads, the attack pad and response rounds
---------------------------------------------------------------------------
-- A round (ops.ask) collects one answer per asked seat and emits a single
-- ev=round when it is settled:
--   responses: 'respond' from anyone ends it at once; otherwise it ends
--              'all_no' when every asked seat passed, or when nobody spoke
--              up for the round's wait (timedOut=true)
--   yesno:     ends 'answered' with every seat's answer ('timeout' with
--              whatever came in, if the wait runs out)
-- Players answer in chat ("no", "go", "wait", "yes"). A round opened with
-- pads=true (a heavy decision) also puts a decision pad on each asked
-- seat's mat; presses come back through decisionPress().

state.roundWait = state.roundWait or 15
local WAITS = { 10, 15, 20, 30, 45, 0 }
HINT_TIMES = 3 -- rounds of each kind that explain how to answer

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
      removeDecisionPad(r, color)
      if next(r.pending) == nil then return finishRound('all_no') end
      refreshDecisionPads(r)
    end
  else
    if not r.pending[color] then return end
    local a = nil
    if w == 'yes' or w == 'yeah' or w == 'yep' or w == 'y' or w == 'sure' or w == 'ok' or w == 'okay' or w == 'please' then a = 'yes'
    elseif w == 'no' or w == 'nope' or w == 'nah' or w == 'n' then a = 'no' end
    if a == nil then return end
    r.pending[color] = nil
    r.answers[color] = a
    removeDecisionPad(r, color)
    if next(r.pending) == nil then return finishRound('answered') end
    refreshDecisionPads(r)
  end
end

-- Decision pads: a big pad that appears on the mat of each player a heavy
-- decision is for (a board wipe, a huge threat, a question that matters),
-- with the question, a countdown and Yes / No / Wait. Wait pauses the
-- countdown for everyone until it is pressed again. A seat's pad goes as
-- soon as that seat answered, the rest when the round settles. The admin
-- pad's "Decision pads" toggle turns them off (chat answers always work).
DECISION_PAD_SCRIPT = [==[
nemesisDecisionPad = true
SEAT, ADMIN, ROUND = nil, nil, nil
local view = { name = 'Kestrel', text = '', kind = 'yesno', left = 0, paused = false, by = '' }

function onLoad(s)
  local ok, d = pcall(function() return JSON.decode(s or '') end)
  if ok and type(d) == 'table' then
    SEAT, ADMIN, ROUND = d.seat, d.admin, d.round
    if type(d.view) == 'table' then view = d.view end
  end
  draw()
end

function onSave() return JSON.encode({ seat = SEAT, admin = ADMIN, round = ROUND, view = view }) end

-- break the question into lines of about n characters
local function wrap(text, n)
  local lines, line = {}, ''
  for word in tostring(text):gmatch('%S+') do
    if line ~= '' and #line + 1 + #word > n then lines[#lines + 1] = line; line = word
    else line = (line == '' and word) or (line .. ' ' .. word) end
  end
  if line ~= '' then lines[#lines + 1] = line end
  while #lines > 4 do table.remove(lines) end
  return table.concat(lines, '\n')
end

function draw()
  self.clearButtons()
  local s = self.getScale()
  local es = { 1 / s.x, 1 / s.y, 1 / s.z }
  local function at(x, z) return { x / s.x, 0.6 / s.y, z / s.z } end
  -- a label is a button of size 0 with no colour set: a transparent
  -- button colour hid the label text along with it
  local function b(lbl, fn, x, z, w, h, fs, col, fcol, tip)
    local spec = { click_function = fn, function_owner = self, label = lbl, tooltip = tip or '',
      position = at(x, z), scale = es, width = w, height = h, font_size = fs, font_color = fcol or { 1, 1, 1 } }
    if col then spec.color = col end
    self.createButton(spec)
  end
  self.setName(view.name .. ': ' .. tostring(view.text))
  b(view.title or (view.name .. ' asks ' .. (SEAT or '?')), 'noop', 0, -1.75, 0, 0, 130, nil, { 1, 0.85, 0.4 })
  b(wrap(view.text, 34), 'noop', 0, -0.75, 0, 0, 150, nil, { 1, 1, 1 })
  local clock
  if view.note then clock = view.note
  elseif view.paused then clock = 'PAUSED' .. (view.by ~= '' and (' by ' .. view.by) or '')
  elseif (view.left or 0) > 0 then clock = view.left .. ' s'
  else clock = 'no time limit' end
  b(clock, 'noop', 0, 0.45, 0, 0, view.note and 110 or 160, nil, view.paused and { 1, 0.75, 0.3 } or { 0.8, 0.8, 0.8 })
  local yesTip = view.yesTip or (view.kind == 'responses' and 'Yes, I respond: hold on' or 'Yes')
  local noTip = view.noTip or (view.kind == 'responses' and 'No response: let it resolve' or 'No')
  -- a choice with no clock (a Winston pile) has two buttons and no Wait
  local x1, x2 = -2.1, 0
  if view.noWait then x1, x2 = -1.4, 1.4 end
  b(view.yesLabel or 'Yes', 'onYes', x1, 1.45, 850, 380, 190, { 0.15, 0.5, 0.2 }, nil, yesTip)
  b(view.noLabel or 'No', 'onNo', x2, 1.45, 850, 380, 190, { 0.6, 0.15, 0.15 }, nil, noTip)
  if not view.noWait then
    b(view.paused and 'Resume' or 'Wait', 'onWait', 2.1, 1.45, 850, 380, 170, { 0.7, 0.5, 0.1 }, nil,
      view.paused and 'Start the countdown again' or 'Pause the countdown while you think')
  end
end

function noop() end

local function press(id, color)
  local a = ADMIN and getObjectFromGUID(ADMIN) or nil
  if a == nil then
    for _, o in ipairs(getAllObjects()) do if o.getVar('nemesisPad') == true then a = o; ADMIN = o.getGUID() end end
  end
  if a == nil then broadcastToColor('The Nemesis pad is not on the table', color, { 1, 0.4, 0.4 }) return end
  a.call('decisionPress', { seat = SEAT, clicker = color, id = id, round = ROUND })
end
function onYes(_, c) press('yes', c) end
function onNo(_, c) press('no', c) end
function onWait(_, c) press('wait', c) end

function show(p)
  if type(p) ~= 'table' then return end
  for k, v in pairs(p) do view[k] = v end
  draw()
end
]==]

-- The attack pad: one button beside the AI's life tracker. A player who
-- attacks the AI presses it; the tap watch still sees the attackers, and
-- the attack reaches the AI as "on you" only with the button lit (or when
-- the AI is the attacker's only opponent). It goes dark at the next turn.
ATTACK_PAD_SCRIPT = [==[
nemesisAttackPad = true
ADMIN, NAME, SEAT = nil, 'Kestrel', nil
local lit, by = false, ''

function onLoad(s)
  local ok, d = pcall(function() return JSON.decode(s or '') end)
  if ok and type(d) == 'table' then ADMIN = d.admin; NAME = d.name or NAME; SEAT = d.seat end
  draw()
end

function onSave() return JSON.encode({ admin = ADMIN, name = NAME, seat = SEAT }) end

function draw()
  self.clearButtons()
  local s = self.getScale()
  self.createButton({ click_function = 'onPress', function_owner = self,
    label = lit and (by .. '\nATTACKS ' .. NAME) or ('Attacking\n' .. NAME .. '?'),
    tooltip = lit and 'Press again to take it back' or ('Press when you attack ' .. NAME),
    position = { 0, 0.6 / s.y, 0 }, scale = { 1 / s.x, 1 / s.y, 1 / s.z }, width = 1150, height = 650, font_size = 130,
    color = lit and { 0.75, 0.12, 0.12 } or { 0.25, 0.1, 0.1 }, font_color = { 1, 1, 1 } })
end

function onPress(_, color)
  local a = ADMIN and getObjectFromGUID(ADMIN) or nil
  if a == nil then
    for _, o in ipairs(getAllObjects()) do if o.getVar('nemesisPad') == true then a = o; ADMIN = o.getGUID() end end
  end
  if a == nil then broadcastToColor('The Nemesis pad is not on the table', color, { 1, 0.4, 0.4 }) return end
  a.call('attackPress', { clicker = color })
end

function setLit(p)
  lit = p and p.lit == true
  by = (p and p.by) or ''
  if p and type(p.name) == 'string' and p.name ~= '' then NAME = p.name end
  self.setColorTint(lit and { 0.55, 0.08, 0.08 } or { 0.16, 0.16, 0.18 })
  draw()
end
]==]

local function objectsWith(var)
  local out = {}
  for _, o in ipairs(getAllObjects()) do
    local ok, v = pcall(function() return o.getVar(var) end)
    if ok and v == true then out[#out + 1] = o end
  end
  return out
end

function decisionPads() return objectsWith('nemesisDecisionPad') end

-- the middle of a seat's mat, turned to face its player
local function decisionPadSpot(c)
  local d = seatData(c)
  if d == nil or d.playmat == nil then return nil end
  local p = d.playmat.getPosition()
  return { x = p.x, y = p.y + 1.2, z = p.z }, d.playmat.getRotation().y
end

local function roundClock(r)
  local by = {}
  for c in pairs(r.paused or {}) do by[#by + 1] = c end
  table.sort(by)
  return { left = r.wait > 0 and math.max(0, math.ceil(r.left or 0)) or 0, paused = #by > 0, by = table.concat(by, ', ') }
end

-- one decision pad on a seat's mat; round is the round id, or 'winston'
-- for a Winston pile. Returns its guid.
function spawnDecisionPad(c, round, view)
  local pos, yaw = decisionPadSpot(c)
  if pos == nil then return nil end
  local o = spawnObjectData({ data = {
    Name = 'BlockSquare', Nickname = 'Nemesis decision (' .. c .. ')', Locked = true,
    Transform = { posX = pos.x, posY = pos.y, posZ = pos.z, rotX = 0, rotY = yaw, rotZ = 0, scaleX = 7, scaleY = 0.2, scaleZ = 4.6 },
    ColorDiffuse = { r = 0.1, g = 0.1, b = 0.13 },
    LuaScript = DECISION_PAD_SCRIPT,
    LuaScriptState = JSON.encode({ seat = c, admin = self.getGUID(), round = round, view = view }),
  }, callback_function = function(o) o.setLock(true) end })
  return o and o.getGUID() or nil
end

function spawnDecisionPads(r)
  r.pads = {}
  for _, c in ipairs(r.asked) do
    local clock = roundClock(r)
    r.pads[c] = spawnDecisionPad(c, r.id, { name = NAME, text = r.text, kind = r.kind, left = clock.left, paused = false, by = '' })
  end
end

-- Winston: the human looking at a pile gets Take / Pass on a pad too
function showWinstonPad(color, pile, n, last)
  removeWinstonPad()
  local dr = state.draft
  if dr == nil or not state.decisionPads then return end
  dr.pad = spawnDecisionPad(color, 'winston', { name = NAME, title = 'Winston draft: your pick, ' .. color,
    text = 'Pile ' .. pile .. ' (' .. n .. ' card' .. (n == 1 and '' or 's') .. ') is in your hand. Take it, or pass it back with one more card?',
    note = last and 'Passing pile 3 takes the top card of the stack' or ('Passing shows you pile ' .. (pile + 1)),
    kind = 'yesno', left = 0, paused = false, by = '', noWait = true, yesLabel = 'Take', noLabel = 'Pass',
    yesTip = 'Keep this pile', noTip = 'Put it back with one more card' })
end

function removeWinstonPad()
  local dr = state.draft
  local g = dr and dr.pad
  if g == nil then return end
  dr.pad = nil
  local o = getObjectFromGUID(g)
  if o then pcall(function() destroyObject(o) end) end
end

function removeDecisionPad(r, c)
  local g = r and r.pads and r.pads[c]
  if g == nil then return end
  r.pads[c] = nil
  local o = getObjectFromGUID(g)
  if o then pcall(function() destroyObject(o) end) end
end

-- every decision pad on the table (a reload loses the round they belonged to)
function removeDecisionPads()
  local n = 0
  for _, o in ipairs(decisionPads()) do pcall(function() destroyObject(o) end); n = n + 1 end
  return n
end

function refreshDecisionPads(r)
  if r == nil or r.pads == nil then return end
  local clock = roundClock(r)
  for c, g in pairs(r.pads) do
    local o = getObjectFromGUID(g)
    if o then pcall(function() o.call('show', clock) end) end
  end
end

state.decisionPads = state.decisionPads ~= false

function onToggleDecisionPads(_, color)
  state.decisionPads = not state.decisionPads
  if not state.decisionPads then removeDecisionPads() end
  setStatus(state.decisionPads and 'Decision pads on: big decisions get a pad on your mat' or 'Decision pads off: answer in chat')
end

-- a press on a decision pad (called by its script)
function decisionPress(p)
  if type(p) ~= 'table' or p.seat == nil then return end
  if p.round == 'winston' then
    local pl = Player[p.clicker]
    if p.clicker ~= p.seat and not (pl and (pl.host or pl.admin)) then
      broadcastToColor('[Nemesis] This pick is ' .. p.seat .. "'s.", p.clicker, { 1, 0.7, 0.3 })
      return
    end
    if p.id == 'yes' or p.id == 'no' then winstonChoice(p.seat, p.id == 'yes') end
    return
  end
  local r = state.round
  local seat, id = p.seat, p.id
  if r == nil or r.id ~= p.round then
    -- a pad left over from a settled round
    for _, o in ipairs(decisionPads()) do
      if o.getVar('ROUND') == p.round then pcall(function() destroyObject(o) end) end
    end
    return
  end
  local pl = Player[p.clicker]
  if p.clicker ~= seat and not (pl and (pl.host or pl.admin)) then
    broadcastToColor('[Nemesis] This decision is ' .. seat .. "'s.", p.clicker, { 1, 0.7, 0.3 })
    return
  end
  emit({ ev = 'button', id = id, color = seat, clicker = p.clicker, name = Player[seat] and Player[seat].steam_name or '', pad = 'decision' })
  if id == 'wait' then
    r.paused = r.paused or {}
    local paused = not r.paused[seat]
    r.paused[seat] = paused or nil
    emit({ ev = 'round_pause', id = r.id, color = seat, paused = paused, left = roundClock(r).left, anyPaused = next(r.paused) ~= nil })
    refreshDecisionPads(r)
    return
  end
  if not r.pending[seat] then return end
  r.pending[seat] = nil
  if r.paused then r.paused[seat] = nil end
  if r.kind == 'responses' then
    r.answers[seat] = id == 'yes' and 'respond' or 'no'
    removeDecisionPad(r, seat)
    if id == 'yes' then return finishRound('respond', seat) end
    if next(r.pending) == nil then return finishRound('all_no') end
  else
    r.answers[seat] = id
    removeDecisionPad(r, seat)
    if next(r.pending) == nil then return finishRound('answered') end
  end
  refreshDecisionPads(r)
end

-- the round's countdown: one tick a second, frozen while anyone pressed Wait
function startRoundClock(r)
  if r.wait <= 0 then return end
  r.left = r.wait
  local timer
  timer = Wait.time(function()
    if state.round ~= r or stale() then Wait.stop(timer) return end
    if r.paused and next(r.paused) ~= nil then return end
    r.left = r.left - 1
    if r.left <= 0 then
      Wait.stop(timer)
      finishRound(r.kind == 'responses' and 'all_no' or 'timeout', nil, true)
      return
    end
    refreshDecisionPads(r)
  end, 1, -1)
end

-- the attack pad ------------------------------------------------------------

function attackPads() return objectsWith('nemesisAttackPad') end

-- beside the AI's life tracker, towards the table's middle; without a
-- tracker, off the far left corner of its mat
local function attackPadSpot()
  local d = state.seat and seatData(state.seat)
  if d == nil or d.playmat == nil then return nil end
  local yaw = d.playmat.getRotation().y
  local ca, cr = cornerSpot(d.playmat, ATTACK_PAD_SPOT)
  if not blockedByFurniture(d.playmat, ca, cr, 0.75 + 0.15, 1.3 + 0.15, true) then
    local w = matWorld(d.playmat, ca, cr)
    return { x = w.x, y = 1.2, z = w.z }, yaw
  end
  local tr = d.lifeTracker
  if tr then
    local p = tr.getPosition()
    local mat = d.playmat
    local b = tr.getBounds().size
    local off = math.max(b.x, b.z) / 2 + 1.6
    -- towards the middle first (the old spot), then beside the tracker, then
    -- behind it; the (S) table's commander damage tiles now sit in front
    local ta, trr = matLocal(mat, p)
    local tries = { { -off, 0 }, { 0, off + 0.6 }, { 0, -(off + 0.6) }, { off, 0 }, { -(off + 1.6), 0 } }
    for _, t in ipairs(tries) do
      local a, r = ta + t[1], trr + t[2]
      if not blockedByFurniture(mat, a, r, 0.75 + 0.15, 1.3 + 0.15, true) then
        local w = matWorld(mat, a, r)
        return { x = w.x, y = p.y + 0.5, z = w.z }, yaw
      end
    end
    local w = matWorld(mat, ta - off, trr)
    return { x = w.x, y = p.y + 0.5, z = w.z }, yaw
  end
  local sc = d.playmat.getScale()
  local p = matWorld(d.playmat, -(sc.z / 2 + 1.5), -(sc.x / 2 - 1.5))
  return { x = p.x, y = 1.2, z = p.z }, yaw
end

-- one attack pad, for the AI's current seat; a pad a player moved stays put
function ensureAttackPad()
  if state.seat == nil then return end
  for _, o in ipairs(attackPads()) do
    if o.getVar('SEAT') == state.seat then
      if not o.getLock() then o.setLock(true) end
      -- a pad covering the table's own tiles (an older pad's spot) moves
      local sd = seatData(state.seat)
      local mat = sd and sd.playmat
      local a, r = 0, 0
      if mat then a, r = matLocal(mat, o.getPosition()) end
      if mat and blockedByFurniture(mat, a, r, 0.75, 1.3, true) then
        local pos = attackPadSpot()
        if pos then o.setPosition(pos) end
      end
      return
    end
    pcall(function() destroyObject(o) end) -- left from an earlier seat
  end
  local pos, yaw = attackPadSpot()
  if pos == nil then return end
  spawnObjectData({ data = {
    Name = 'BlockSquare', Nickname = 'Nemesis attack pad', Locked = true,
    Transform = { posX = pos.x, posY = pos.y, posZ = pos.z, rotX = 0, rotY = yaw + 180, rotZ = 0, scaleX = 2.6, scaleY = 0.2, scaleZ = 1.5 },
    ColorDiffuse = { r = 0.16, g = 0.16, b = 0.18 },
    LuaScript = ATTACK_PAD_SCRIPT,
    LuaScriptState = JSON.encode({ admin = self.getGUID(), name = NAME, seat = state.seat }),
  }, callback_function = function(o) o.setLock(true) end })
end

function refreshAttackPad()
  local call = state.attackCall
  for _, o in ipairs(attackPads()) do
    pcall(function() o.call('setLit', { lit = call ~= nil, by = call and call.color or '', name = NAME }) end)
  end
end

-- the turn moved on: the button goes dark
function clearAttackCall()
  state.lastAttack = nil
  if state.attackCall == nil then return end
  state.attackCall = nil
  refreshAttackPad()
end

-- a press on the attack pad (called by its script)
function attackPress(p)
  local c = type(p) == 'table' and p.clicker or nil
  if c == nil or c == state.seat then return end
  if state.attackCall then
    local was = state.attackCall
    clearAttackCall()
    emit({ ev = 'attack', color = was.color, by = c, takenBack = true })
    return
  end
  local turn = Turns.turn_color
  local attacker = (turn ~= nil and turn ~= '' and turn ~= state.seat and not state.claudeTurn) and turn or c
  local call = { color = attacker, by = c, t = Time.time }
  state.attackCall = call
  refreshAttackPad()
  local pl = Player[attacker]
  -- creatures that tapped before the press were at the AI after all
  local last = state.lastAttack
  if last and last.seat == attacker and Time.time - last.t < 90 then
    state.lastAttack = nil
    call.reported = true
    emit({ ev = 'attack', color = attacker, player = pl and pl.steam_name or '', cards = last.cards, onMe = true, late = true })
    return
  end
  -- attackers with vigilance never tap: say so if no taps follow
  Wait.time(function()
    if state.attackCall ~= call or call.reported or state.taps[attacker] then return end
    call.reported = true
    emit({ ev = 'attack', color = attacker, player = pl and pl.steam_name or '', cards = '[]', onMe = true, noTaps = true })
  end, 6)
end

local function roundView(r)
  if r == nil then return nil end
  local waiting = {}
  for _, c in ipairs(r.asked) do if r.pending[c] then waiting[#waiting + 1] = c end end
  return { id = r.id, kind = r.kind, text = r.text, asked = r.asked, waitingOn = waiting, answers = r.answers }
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
    printToAll('[Nemesis] No responses', { 0.7, 0.7, 0.7 })
  elseif result == 'timeout' then
    printToAll('[Nemesis] Out of time: ' .. (#parts > 0 and table.concat(parts, ', ') or 'no answers'), { 0.7, 0.7, 0.7 })
  end
  for c in pairs(r.pads or {}) do removeDecisionPad(r, c) end
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
-- Global.getTable copies the whole seat table across scripts; every op,
-- every zone event and the 1.5 s tap poll read it, so keep a copy for a
-- second (the table's layout does not change mid-game).
local dataCache = { t = -10, d = nil }
function globalData()
  if Time.time - dataCache.t > 1 or dataCache.d == nil then
    dataCache = { t = Time.time, d = Global.getTable('data') }
  end
  return dataCache.d
end

function refreshColors()
  local d = globalData()
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

-- Locked table furniture on and around a mat (counters, trackers, helper
-- tiles) as mat-local boxes { a, r, ha, hr, mine }, so cards and our pads are
-- not put on top of them. The (S) table's 2026-10 update put mana and energy
-- counters inside the land box and commander damage tiles where the front
-- creature row starts; pieces() skips locked objects, so slots landed on
-- them. Cards and piles are left to pieces(); scripting zones, the Highlight
-- Mat, mat-sized objects and anything parked under the table are skipped.
-- The pad counts although it is unlocked; attack pads are marked mine.
local furnitureCache = {}
function matFurniture(mat)
  local key = mat.getGUID()
  local c = furnitureCache[key]
  if c and Time.time - c.t < 5 then return c.list end
  local f, r, s = mat.getTransformForward(), mat.getTransformRight(), mat.getScale()
  local list = {}
  for _, o in ipairs(getAllObjects()) do
    local t = o.type
    local okv, v = pcall(function() return o.getVar('nemesisAttackPad') end)
    local mine = okv and v == true
    if (mine or o == self or o.getLock()) and o ~= mat and t ~= 'Card' and t ~= 'Deck' and t ~= 'Scripting'
        and o.getName() ~= 'Highlight Mat' then
      local okb, b = pcall(function() return o.getBounds() end)
      if okb and b and b.center and b.size and b.center.y > 0.5 then
        local a, rr = matLocal(mat, b.center)
        local ha = (math.abs(f.x) * b.size.x + math.abs(f.z) * b.size.z) / 2
        local hr = (math.abs(r.x) * b.size.x + math.abs(r.z) * b.size.z) / 2
        if ha < s.z / 3 and hr < s.x / 3
            and math.abs(a) - ha < s.z / 2 + 4 and math.abs(rr) - hr < s.x / 2 + 6 then
          list[#list + 1] = { a = a, r = rr, ha = ha, hr = hr, mine = mine, pad = o == self }
        end
      end
    end
  end
  furnitureCache[key] = { t = Time.time, list = list }
  return list
end

-- true when a box at mat-local (a, r) with half sizes (ha, hr) overlaps
-- furniture; skipMine leaves out the pad and the attack pad
function blockedByFurniture(mat, a, r, ha, hr, skipMine)
  for _, o in ipairs(matFurniture(mat)) do
    if not (skipMine and o.mine) and math.abs(o.a - a) < o.ha + ha and math.abs(o.r - r) < o.hr + hr then
      return true
    end
  end
  return false
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
  local all = globalData()
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
-- A zone with tags lists only objects carrying a matching tag; the table's
-- 2026-10-28 update tagged its zones and libraries read empty. Ask for
-- everything in the zone (ignore_tags) and fall back for plain objects.
-- ignore_tags also returns GUID-less scenery (the table itself): skip it.
function zoneObjects(z)
  local ok, objs = pcall(function() return z.getObjects(true) end)
  if not ok or type(objs) ~= 'table' then objs = z.getObjects() end
  local out = {}
  for _, o in ipairs(objs) do
    local okg, g = pcall(function() return o.getGUID() end)
    if okg and g ~= nil and g ~= '' then out[#out + 1] = o end
  end
  return out
end

local function pieces(zone)
  local out = {}
  if zone == nil then return out end
  for _, o in ipairs(zoneObjects(zone)) do
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
    for _, o in ipairs(zoneObjects(d.landZone)) do landGuids[o.getGUID()] = true end
  end
  local cardsByGuid = {}
  local loose = {}
  for _, o in ipairs(pieces(d.playmat)) do
    local e = { guid = o.getGUID(), type = o.type, pos = posOf(o), tapped = isTapped(o, d.playmat), counters = {} }
    if landGuids[e.guid] then e.row = 'land' end
    if o.type == 'Card' then
      e.faceDown = isFaceDown(o)
      -- during a draft your own face-down cards are unopened packs too
      if e.faceDown and (not mine or state.draft) then
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

-- Where things are around the AI's seat, in mat terms (along = the mat's
-- forward axis, right = its right axis, from the mat's centre): every
-- non-card object within reach, the pads, and the draft spots. For fitting
-- the pad to a new table layout; it reads positions and public text only.
function ops.layout(req)
  local seat = state.seat -- works while STOP is on, like snapshot
  if seat == nil then error(NAME .. ' has no seat yet') end
  local d = seatData(seat)
  if d == nil or d.playmat == nil then error('no mat for ' .. seat) end
  local mat = d.playmat
  local function at(p)
    local a, r = matLocal(mat, p)
    return { along = round(a), right = round(r), y = round(p.y) }
  end
  local out = { seat = seat, mat = { pos = at(mat.getPosition()), yaw = round(mat.getRotation().y),
    scale = { x = round(mat.getScale().x), z = round(mat.getScale().z) } }, objects = {}, cards = {} }
  local reach = tonumber(req.reach) or 26
  for _, o in ipairs(getAllObjects()) do
    local p = o.getPosition()
    local a, r = matLocal(mat, p)
    if math.abs(a) <= reach and math.abs(r) <= reach then
      local e = at(p)
      e.guid, e.type, e.name = o.getGUID(), o.type, (o.getName():match('^[^\n]*') or '')
      if o.type == 'Card' or o.type == 'Deck' then
        e.n = o.type == 'Deck' and o.getQuantity() or 1
        if isTableProp(o) then e.tableProp = true end -- e.g. Lotus Helper's fetch previews
        out.cards[#out.cards + 1] = e
      else
        e.desc = o.getDescription():sub(1, 60)
        e.locked = o.getLock()
        e.yaw = round(o.getRotation().y)
        local sc = o.getScale()
        e.scale = { x = round(sc.x), z = round(sc.z) }
        if o.type == 'Scripting' then
          e.scale.y = round(sc.y)
          local okt, tags = pcall(function() return o.getTags() end)
          if okt and type(tags) == 'table' and #tags > 0 then e.tags = table.concat(tags, ',') end
        end
        local ok, src = pcall(function() return o.getLuaScript() end)
        if ok and src and src ~= '' then
          local fns = {}
          for fn in src:gmatch('function%s+([%w_]+)%s*%(') do
            if #fns < 25 then fns[#fns + 1] = fn end
          end
          e.script = table.concat(fns, ',')
        end
        local okb, btns = pcall(function() return o.getButtons() end)
        if okb and btns and #btns > 0 then
          local labels = {}
          for i, b in ipairs(btns) do if i <= 6 then labels[#labels + 1] = tostring(b.label):sub(1, 20) end end
          e.buttons = table.concat(labels, '|')
        end
        if o == self then e.thisPad = true end
        out.objects[#out.objects + 1] = e
      end
    end
  end
  if d.lifeTracker then out.lifeTracker = at(d.lifeTracker.getPosition()) end
  out.slots = slotReport(d)
  local ap = attackPadSpot()
  if ap then out.attackPadSpot = at(ap) end
  -- which object the table links for each zone, and what it holds with and
  -- without tag filtering
  out.zones = {}
  for _, k in ipairs({ 'libraryZone', 'graveyard', 'exileZone', 'commandZone', 'landZone' }) do
    local z = d[k]
    if z ~= nil then
      local okp, plain = pcall(function() return #z.getObjects() end)
      out.zones[k] = { guid = z.getGUID(), plain = okp and plain or -1, all = #zoneObjects(z) }
    end
  end
  local dr = state.draft
  if dr and dr.format == 'winston' and #dr.seats == 2 then
    local s = winstonSpotsFor(dr.seats[1], dr.seats[2])
    out.winston = { stack = at(s.stack), piles = { at(s.piles[1]), at(s.piles[2]), at(s.piles[3]) } }
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
      local all = globalData() or {}
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
      round = roundView(state.round), decisionPads = state.decisionPads, roundWait = state.roundWait },
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
  if req.to and req.to ~= '' then
    -- a private line (a secret number, a hidden choice): only that seat
    -- reads it; everyone sees that a whisper happened
    local p = Player[req.to]
    if p == nil or not p.seated then error(tostring(req.to) .. ' is not seated') end
    printToColor('[' .. NAME .. ' whispers] ' .. text, req.to, col)
    printToAll('[' .. NAME .. '] (whispered to ' .. req.to .. ')', col)
    return { whispered = req.to }
  end
  -- broadcastToAll also writes the line to chat, so a loud line must not be
  -- printed as well or it shows up twice
  if req.loud then broadcastToAll('[' .. NAME .. '] ' .. text, col) else printToAll('[' .. NAME .. '] ' .. text, col) end
  return { said = text }
end

-- Ask the table something and collect one answer per seat, from chat or,
-- with pads=true (a heavy decision), from a decision pad on each asked
-- seat's mat. kind = 'responses' ("any responses?") or 'yesno'. seats
-- defaults to every seated human. A new ask replaces an open one.
function ops.ask(req)
  local seat = requireSeat()
  local kind = req.kind == 'yesno' and 'yesno' or 'responses'
  local seats = {}
  if type(req.seats) == 'table' and #req.seats > 0 then
    for _, c in ipairs(req.seats) do if c ~= seat then seats[#seats + 1] = c end end
  else
    for _, c in ipairs(seatedHumans()) do if not seatSkipped(c) then seats[#seats + 1] = c end end
  end
  if #seats == 0 then error('nobody to ask') end
  local text = tostring(req.text or '')
  if text == '' then text = kind == 'responses' and 'Any responses?' or 'Yes or no?' end
  state.roundSeq = (state.roundSeq or 0) + 1
  local pads = req.pads == true and state.decisionPads
  local wait = tonumber(req.wait)
  if wait == nil or wait < 0 then
    wait = state.roundWait
    -- a yes/no question or a heavy decision deserves longer than "any responses?"
    if (kind == 'yesno' or pads) and wait > 0 then wait = math.max(wait * 2, 30) end
  end
  local r = { id = state.roundSeq, kind = kind, text = text, asked = seats, pending = {}, answers = {}, wait = wait, quiet = req.quiet == true }
  for _, c in ipairs(seats) do r.pending[c] = true end
  local old = state.round
  local replaced = old and old.id or nil
  if old then for c in pairs(old.pads or {}) do removeDecisionPad(old, c) end end
  state.round = r
  -- TTS keeps no chat history, so every line counts: the how-to goes out
  -- with the first few rounds of each kind, after that only the seconds
  local secs = wait > 0 and (' (' .. wait .. ' s)') or ''
  local key = (pads and 'pads') or (r.quiet and 'quiet') or kind
  state.hints = state.hints or {}
  state.hints[key] = (state.hints[key] or 0) + 1
  local how = secs
  if state.hints[key] <= HINT_TIMES then
    if pads then how = '  [answer on the pad on your mat, or in chat' .. (wait > 0 and ('; ' .. wait .. ' s, Wait pauses]') or ']')
    elseif r.quiet then how = '  [drop a response on my mat or say "wait"' .. (wait > 0 and ('; ' .. wait .. ' s]') or ']')
    elseif kind == 'responses' then how = '  [say "no" to pass, "wait" to respond' .. (wait > 0 and ('; ' .. wait .. ' s]') or ']')
    else how = '  [answer yes or no in chat]' end
  end
  local line = '[' .. NAME .. '] ' .. text .. how
  -- a spell going on the stack is no question, just a quiet window (loud for
  -- a response to someone else's spell, so they stop and look)
  if r.quiet and not req.loud and not pads then printToAll(line, Color.fromString(seat)) else broadcastToAll(line, Color.fromString(seat)) end
  if pads then spawnDecisionPads(r) end
  startRoundClock(r)
  return { round = r.id, kind = kind, asked = seats, pads = pads == true, replaced = replaced, wait = wait }
end

function ops.cancel_round(req)
  requireSeat()
  local r = state.round
  if r == nil then return { cancelled = false } end
  state.round = nil
  for c in pairs(r.pads or {}) do removeDecisionPad(r, c) end
  return { cancelled = true, id = r.id, answers = r.answers }
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

-- What nemesis-mcp needs for a response window, in one light call: my own
-- battlefield cards without on-card state (no Encoder calls, no other seats)
-- and my hand. Works while STOP is on, like snapshot.
function ops.window(req)
  local seat = state.seat
  if seat == nil then error('no seat') end
  local d = seatData(seat)
  if d == nil or d.playmat == nil then error('table has no data for ' .. seat) end
  local lands = {}
  if d.landZone then
    for _, o in ipairs(zoneObjects(d.landZone)) do lands[o.getGUID()] = true end
  end
  local field = {}
  for _, o in ipairs(pieces(d.playmat)) do
    if o.type == 'Card' then
      local g = o.getGUID()
      field[#field + 1] = { guid = g, type = 'Card', nick = o.getName(), desc = o.getDescription(),
        tapped = isTapped(o, d.playmat), faceDown = isFaceDown(o), row = lands[g] and 'land' or nil }
    end
  end
  return { seat = seat, battlefield = field, cards = ops.hand(req).cards }
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

-- for table_layout: each row's slot count and the slots furniture blocks
-- (strings, not tables: TTS drops payloads holding an empty table)
function slotReport(d)
  local out = {}
  for _, row in ipairs({ 'land', 'creature', 'other' }) do
    local spec = rowSpec(d, row)
    local blocked = {}
    for i, a in ipairs(spec.rows) do
      for j, c in ipairs(spec.cols) do
        if blockedByFurniture(d.playmat, a, c, CARD_H / 2, CARD_W / 2) then
          blocked[#blocked + 1] = string.format('r%d c%d (%.1f, %.1f)', i, j, a, c)
        end
      end
    end
    out[row] = { slots = #spec.rows * #spec.cols, blocked = #blocked > 0 and table.concat(blocked, '; ') or 'none' }
  end
  out.furniture = #matFurniture(d.playmat)
  return out
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
    if blockedByFurniture(d.playmat, a, r, CARD_H / 2, CARD_W / 2) then return false end
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

-- An optional request field: the server sends '' for "not given", and '' is
-- truthy in Lua, so `req.row or 'creature'` gave '' (creature tokens went to
-- the 'other' row).
function optArg(v, default)
  if v == nil or v == '' then return default end
  return v
end

-- Deal into the main hand. On the 4-seat tables TTS sometimes put a dealt card
-- in the secondary hand (2026-10-30 game), so ask for hand 1 and, once the
-- cards have landed, move any newcomer there to hand 1. Cards `look` put in
-- the secondary hand stay.
local function dealToHand(obj, n, seat)
  local before = {}
  for _, h in ipairs(handZones(seat)) do
    if h.idx > 1 then
      for _, o in ipairs(handObjects(seat, h.idx)) do before[o.getGUID()] = true end
    end
  end
  obj.deal(n, seat, 1)
  Wait.time(function()
    local ok, tr = pcall(function() return Player[seat].getHandTransform(1) end)
    if not ok or not tr or not tr.position then return end
    for _, h in ipairs(handZones(seat)) do
      if h.kind == 'secondary' then
        for _, o in ipairs(handObjects(seat, h.idx)) do
          local g = o.getGUID()
          if not before[g] and not state.looked[g] then
            state.touched[g] = Time.time
            o.setPosition({ tr.position.x, tr.position.y + 1, tr.position.z })
          end
        end
      end
    end
  end, 1.5)
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
    Wait.time(function() local l = libraryTop(d); if l then dealToHand(l, n, seat) end end, delay)
  else
    dealToHand(lib, n, seat)
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
      Wait.time(function() dealToHand(deck, 7, seat) end, 0.6)
    end
  end, 0.8)
  return { returned = #hand, drawing = 7 }
end

-- hand/command zone -> battlefield row ('land' | 'creature' | 'other'), or
-- -> 'stack' (announce a spell; resolve it later with move{})
function ops.play(req)
  local seat, d = myData()
  local obj, from = myObject(seat, d, req.guid)
  local row = optArg(req.row, 'other')
  if row ~= 'land' and row ~= 'creature' and row ~= 'other' and row ~= 'stack' then error('row must be land/creature/other/stack') end
  local pos = (row == 'stack') and stackPoint(d) or rowSlot(d, row)
  faceUpAt(obj, pos, yawOf(d.playmat))
  return { played = obj.getName(), desc = obj.getDescription(), from = from, row = row, guid = obj.getGUID() }
end

local function setTapped(d, obj, tapped)
  state.touched[obj.getGUID()] = Time.time
  local r = obj.getRotation()
  local yaw = yawOf(d.playmat) + (tapped and 90 or 0)
  obj.setRotationSmooth({ r.x, yaw, r.z }, false, true)
end

-- True when a mana ability of the card sacrifices it ("{T}, Sacrifice this
-- token: Add...", "Sacrifice Lotus Petal: Add..."). A quoted ability it grants
-- to others (Goldspan Dragon's Treasures) does not count.
-- The colon is found with a plain find: '^(.-):' on a long line without one
-- ("As this land enters, you may reveal ...") is "pattern too complex" in TTS.
local function sacrificedForMana(nick, desc)
  local name = string.lower((nick or ''):match('^[^\n]*') or '')
  for line in string.gmatch(string.lower(desc or ''), '[^\n]+') do
    local colon = string.find(line, ':', 1, true)
    local cost = colon and string.sub(line, 1, colon - 1)
    local effect = colon and (string.gsub(string.sub(line, colon + 1), '^%s+', ''))
    if cost and effect:find('^add') and not cost:find('"', 1, true)
        and (cost:find('sacrifice this', 1, true) or (name ~= '' and cost:find('sacrifice ' .. name, 1, true))) then
      return true
    end
  end
  return false
end

-- A token: the encoder says so, or the card's type line (the nickname's second
-- line, "Treasure\nToken Artifact") does.
local function isTokenCard(obj)
  local info = encoderInfo(obj) or {}
  local nick = obj.getName()
  return info.token or string.find(string.lower(nick:match('\n([^\n]*)') or ''), 'token', 1, true) ~= nil
end

-- req.mana: the cards pay for a spell, so a Treasure-like source is sacrificed
-- instead of tapped. A token ceases to exist, a card goes to the graveyard, and
-- one card standing for N tokens (spawn_token count=N) drops to N-1.
function ops.tap(req)
  local seat, d = myData()
  local done, sacrificed, warn = {}, {}, nil
  for _, g in ipairs(req.guids or {}) do
    local obj, from = myObject(seat, d, g)
    if from ~= 'playmat' then error(obj.getName() .. ' is not on my battlefield') end
    local nick = obj.getName()
    local name = nick:match('^[^\n]*') or nick
    local sacMana = sacrificedForMana(nick, obj.getDescription())
    if sacMana and not req.mana then
      warn = name .. ' was only tapped: if it paid for mana, tap it with mana=true so it is sacrificed'
    end
    if req.mana and sacMana then
      local info = encoderInfo(obj) or {}
      local n = tonumber(info.counters) or 0
      local isToken = isTokenCard(obj)
      if n > 1 then
        ops.mark({ guid = g, counters = -1, note = string.format('%d %s tokens', n - 1, name) })
        sacrificed[#sacrificed + 1] = string.format('one %s (%d left on [%s])', name, n - 1, g)
      elseif isToken then
        obj.destruct()
        sacrificed[#sacrificed + 1] = name .. ' (token, gone)'
      else
        ops.move({ guid = g, to = 'graveyard' })
        sacrificed[#sacrificed + 1] = name .. ' (to graveyard)'
      end
    else
      setTapped(d, obj, true)
      done[#done + 1] = nick
    end
  end
  return { tapped = done, sacrificed = sacrificed, warning = warn }
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
  local short = name:match('^[^\n]*') or name
  local offField = to == 'graveyard' or to == 'exile' or to == 'hand' or to == 'library_top' or to == 'library_bottom'
  -- a token leaving the battlefield ceases to exist
  if offField and isTokenCard(obj) then
    obj.destruct()
    return { moved = name, from = from, to = to, note = short .. ' is a token: it ceased to exist' }
  end
  -- a card another player put on my mat (they gave it to me, or cast it
  -- there) goes to its owner's zones, not mine
  local owner = state.playBy[obj.getGUID()]
  local od = owner and seatData(owner)
  if offField and od then
    if to == 'graveyard' or to == 'exile' then
      local z = to == 'graveyard' and od.graveyard or od.exileZone
      if z == nil then error(short .. ' belongs to ' .. owner .. ', whose ' .. to .. ' I cannot find: ask them to take it') end
      local p = z.getPosition()
      faceUpAt(obj, { p.x, p.y + 2, p.z }, to == 'exile' and (od.exileYaw or yawOf(z)) or yawOf(z))
      state.playBy[obj.getGUID()] = nil
      broadcastToAll('[' .. NAME .. '] ' .. short .. ' goes to ' .. owner .. "'s " .. to .. ' (their card)', Color.fromString(seat))
      return { moved = name, from = from, to = owner .. "'s " .. to }
    end
    error(short .. ' belongs to ' .. owner .. ': ask them to put it in their ' .. (to == 'hand' and 'hand' or 'library'))
  end
  if to == 'graveyard' then
    local p = d.graveyard.getPosition(); faceUpAt(obj, { p.x, p.y + 2, p.z }, yawOf(d.graveyard))
  elseif to == 'exile' then
    local p = d.exileZone.getPosition(); faceUpAt(obj, { p.x, p.y + 2, p.z }, d.exileYaw or yawOf(d.exileZone))
  elseif to == 'command' then
    local p = d.commandSpot or d.commandZone.getPosition(); faceUpAt(obj, { p.x, p.y + 2, p.z }, yawOf(d.commandZone))
  elseif to == 'hand' then
    state.touched[obj.getGUID()] = Time.time
    dealToHand(obj, 1, seat)
  elseif to == 'stack' then
    faceUpAt(obj, stackPoint(d), yaw)
  elseif to == 'battlefield' then
    faceUpAt(obj, rowSlot(d, optArg(req.row, 'other')), yaw)
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

-- The header of a card description: everything before the first blank line.
-- A plain find, because TTS's Lua gives up on '^(.-)\n\n' over long oracle
-- text ("pattern too complex").
local function descHeader(desc)
  local i = string.find(desc, '\n\n', 1, true)
  if i then return string.sub(desc, 1, i - 1) end
  return desc
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
  local to = optArg(req.to, 'battlefield')
  -- plain-name cards (other importers): name is the card name, description is
  -- "{cost}\nType line\n\nOracle text". Match name + the header lines
  -- (everything before the first blank line) unless req.desc asks for all.
  -- Nicknames that carry the type line ("Krosan Verge\nLand 0CMC") come with a
  -- description that is only rules text: leave it out, or a Forest search
  -- takes Krosan Verge for its "search for a Forest card".
  local function haystack(name, desc)
    local dsc = desc or ''
    if not req.desc then
      if string.find(name or '', '\n', 1, true) then dsc = '' else dsc = descHeader(dsc) end
    end
    return string.lower(name or '') .. '\n' .. string.lower(dsc)
  end
  -- basic: the type line must say Basic Land (a Forest search otherwise takes
  -- Breeding Pool, whose type line is Land — Forest Island)
  -- every word must appear, in any order: 'basic land forest' finds a Forest
  -- whose type line reads Basic Land — Forest
  local function hit(name, desc)
    local h = haystack(name, desc)
    if not string.find(h, q, 1, true) then
      for w in string.gmatch(q, '%S+') do
        if not string.find(h, w, 1, true) then return false end
      end
    end
    if not req.basic then return true end
    local head = string.lower(name or '') .. '\n' .. string.lower(descHeader(desc or ''))
    return string.find(head, 'basic land', 1, true) ~= nil
  end
  local function matching()
    local out = {}
    if lib.type == 'Deck' then
      -- cards still inside an imported deck have no GUIDs: take by index
      for _, c in ipairs(lib.getObjects()) do
        if hit(c.name, c.description) then out[#out + 1] = c end
      end
    elseif hit(lib.getName(), lib.getDescription()) then
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
        dealToHand(c, 1, seat)
      else
        c.setPosition(rowSlot(d, optArg(req.row, 'other')))
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
    local hay = string.lower(name .. '\n' .. descHeader(desc))
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
    if d[k] then wipe(zoneObjects(d[k]), true) end
  end
  if d.playmat then wipe(zoneObjects(d.playmat), false) end
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
  state.gameTurns = 0
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
  local row = optArg(req.row, 'creature')
  local pos = rowSlot(d, row)
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
  if state.draft and state.draft.phase == 'done' then endDraft() end
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
  if state.seat == nil or state.draft or state.claudeTurn or not Turns.enable or seat == nil or seat == '' or seat == state.seat then
    w.seat = nil
    return
  end
  local d = seatData(seat)
  if d == nil or d.playmat == nil then return end
  if w.seat ~= seat then w.seat = seat; w.untapped = {} end
  local land = {}
  if d.landZone then for _, o in ipairs(zoneObjects(d.landZone)) do land[o.getGUID()] = true end end
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
    local cards = JSON.encode(b.cards)
    -- at the AI only with the attack pad lit, or when the AI is the
    -- attacker's one opponent; otherwise kept in case the press comes late
    local call = state.attackCall
    local pressed = call ~= nil and call.color == seat
    local others = 0
    for _, c in ipairs(seatedHumans()) do if c ~= seat and not seatSkipped(c) then others = others + 1 end end
    if pressed then call.reported = true end
    if not pressed then state.lastAttack = { seat = seat, cards = cards, t = Time.time } end
    emit({ ev = 'attack', color = seat, player = pl and pl.steam_name or '', cards = cards,
      onMe = pressed or others == 0, pressed = pressed, headsUp = others == 0 })
  end, 3)
end

-- The mana trackers: tables like "MTG EDH 4-player (S)" have a counter per
-- colour (White Mana ... Colorless Mana) by each land row. The pad keeps the
-- AI's showing what its untapped sources could make, so the table can see
-- what it has up: a dual land counts for both its colours, "any color" for
-- all five, Sol Ring for two colorless. Checked every few seconds, written
-- only when a number changes.
local MANA_COUNTERS = { W = 'White Mana', U = 'Blue Mana', B = 'Black Mana', R = 'Red Mana', G = 'Green Mana', C = 'Colorless Mana' }

-- what one untapped permanent could add, by colour
function manaOf(desc)
  local out = {}
  for clause in tostring(desc or ''):gmatch('{T}: Add ([^.\n]*)') do
    local lower = clause:lower()
    if lower:find('any color') or lower:find('any one color') then
      for _, k in ipairs({ 'W', 'U', 'B', 'R', 'G' }) do out[k] = math.max(out[k] or 0, 1) end
    elseif lower:find(' or ') then
      for k in clause:gmatch('{([WUBRGC])}') do out[k] = math.max(out[k] or 0, 1) end
    else
      local n = {}
      for k in clause:gmatch('{([WUBRGC])}') do n[k] = (n[k] or 0) + 1 end
      for k, v in pairs(n) do out[k] = math.max(out[k] or 0, v) end
    end
  end
  return out
end

-- the seat's own counters: the nearest of each name, beside its mat
local function manaCounters(seat, d)
  state.manaCounters = state.manaCounters or {}
  local c = state.manaCounters[seat]
  if c and c.W ~= nil and getObjectFromGUID(c.W) then return c end
  local half = d.playmat.getScale().x / 2
  c = {}
  for _, o in ipairs(getAllObjects()) do
    local name = o.getName()
    for k, want in pairs(MANA_COUNTERS) do
      if name == want then
        local along, right = matLocal(d.playmat, o.getPosition())
        local dist = along * along + right * right
        if math.abs(along) <= 15 and math.abs(right) <= half + 4 and (c[k] == nil or dist < c[k .. 'd']) then
          c[k], c[k .. 'd'] = o.getGUID(), dist
        end
      end
    end
  end
  state.manaCounters[seat] = c
  return c
end

function pollMana()
  if state.seat == nil or state.stopped or state.draft then return end
  local d = seatData(state.seat)
  if d == nil or d.playmat == nil then return end
  local counters = manaCounters(state.seat, d)
  if counters.W == nil then return end -- this table has no mana trackers
  local total = { W = 0, U = 0, B = 0, R = 0, G = 0, C = 0 }
  for _, o in ipairs(pieces(d.playmat)) do
    if o.type == 'Card' and not isFaceDown(o) and not isTapped(o, d.playmat) then
      for k, v in pairs(manaOf(o.getDescription())) do total[k] = total[k] + v end
    end
  end
  state.manaShown = state.manaShown or {}
  for k, v in pairs(total) do
    local g = counters[k]
    if g and state.manaShown[k] ~= v then
      local o = getObjectFromGUID(g)
      if o then
        pcall(function()
          o.setVar('val', v)
          o.call('updateVal')
        end)
        state.manaShown[k] = v
      end
    end
  end
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

---------------------------------------------------------------------------
-- drafting
---------------------------------------------------------------------------
-- Booster draft (one pick a pass), pick-two (two a pass, for 3 or 4
-- drafters) and Winston (two drafters). The pad deals and passes every pack
-- itself, through the hand zones: a pack in your hand is seen by you alone,
-- which is TTS's own privacy. A set's boosters come from the importer's own
-- /draft (real collation, three per drafter); a cube's packs are decided
-- here. Either way nemesis-mcp only learns the set or cube list and the
-- cards that reach the AI's own hand or pool.
--   booster / pick-two: every drafter gets a pack in hand. A human picks by
--     taking cards out of their hand (face-down onto their mat), the AI with
--     draft_pick. Once everyone has picked, every pack moves on at once:
--     packs 1 and 3 in seat order, pack 2 the other way.
--   winston: the stack and three piles sit between the two drafters. The
--     drafter on turn looks at pile 1 (a human gets it in hand) and takes it
--     or passes it: a passed pile gets the stack's top card and the next pile
--     is looked at; passing pile 3 takes the stack's top card blind.
-- The AI's picks go face-down to its pool on its mat. While a draft is on
-- the table the pad reports no plays or zone moves (picks would read as
-- casts), until the first game turn after it.

local function draftKey(name)
  local n = (tostring(name or ''):match('^[^\n]*') or ''):lower()
  n = n:gsub('%s*//.*$', '')
  n = n:gsub('^%s+', '')
  n = n:gsub('%s+$', '')
  return n
end

local function shuffled(list)
  local t = {}
  for i, v in ipairs(list) do t[i] = v end
  for i = #t, 2, -1 do
    local j = math.random(i)
    t[i], t[j] = t[j], t[i]
  end
  return t
end

-- cards and decks resting on a spot (packs, piles, the pool)
local function cardsAt(pos, radius)
  local out = {}
  local rad = radius or 1.5
  for _, o in ipairs(getAllObjects()) do
    if (o.type == 'Card' or o.type == 'Deck') and not o.getLock() then
      local p = o.getPosition()
      if math.abs(p.x - pos.x) < rad and math.abs(p.z - pos.z) < rad then out[#out + 1] = o end
    end
  end
  return out
end

local function countAt(pos)
  local n = 0
  for _, o in ipairs(cardsAt(pos)) do n = n + (o.type == 'Deck' and o.getQuantity() or 1) end
  return n
end

-- name, text and guid of every card on a spot (a deck's cards without
-- taking them out)
local function listAt(pos)
  local out = {}
  for _, o in ipairs(cardsAt(pos)) do
    if o.type == 'Deck' then
      for _, c in ipairs(o.getObjects()) do out[#out + 1] = { guid = c.guid, nick = c.name, desc = c.description } end
    else
      out[#out + 1] = { guid = o.getGUID(), nick = o.getName(), desc = o.getDescription() }
    end
  end
  return out
end

local function faceDownYaw(yaw) return { 0, yaw, 180 } end

-- every card on a spot, one by one, to place.pos(i) turned place.rot, then
-- place.done(card) (decks are taken apart; a deck's last card is its
-- remainder)
local function eachCardAt(pos, place)
  local i = 0
  for _, o in ipairs(cardsAt(pos)) do
    if o.type == 'Deck' then
      local deck = o
      local n = deck.getQuantity()
      for _ = 1, n do
        i = i + 1
        if deck.type == 'Deck' then
          local c = deck.takeObject({ position = place.pos(i), rotation = place.rot, smooth = false })
          if deck.remainder ~= nil then deck = deck.remainder end
          if c then place.done(c) end
        else
          deck.setRotation(place.rot)
          deck.setPosition(place.pos(i))
          place.done(deck)
        end
      end
    else
      i = i + 1
      o.setRotation(place.rot)
      o.setPosition(place.pos(i))
      place.done(o)
    end
  end
end

local function handSpot(color)
  local tr = Player[color].getHandTransform(1)
  return tr.position, tr.rotation
end

-- deal whatever sits on `pos` into a seat's hand; returns the cards' guids
local function spotToHand(pos, color)
  local hp, hr = handSpot(color)
  local guids = {}
  eachCardAt(pos, {
    pos = function(i) return { hp.x, hp.y + 0.3 + i * 0.05, hp.z } end,
    rot = { 0, hr.y + 180, 0 },
    done = function(c) state.touched[c.getGUID()] = Time.time; guids[#guids + 1] = c.getGUID() end,
  })
  return guids
end

-- move cards (by guid, wherever they are) face-down onto a spot
local function guidsToSpot(guids, pos, yaw)
  local base = countAt(pos)
  for i, g in ipairs(guids) do
    local o = getObjectFromGUID(g)
    if o then
      state.touched[g] = Time.time
      o.setRotation(faceDownYaw(yaw))
      o.setPosition({ pos.x, pos.y + 0.4 + (base + i) * 0.05, pos.z })
    end
  end
end

local function inHandSet(color)
  local s = {}
  for _, h in ipairs(handZones(color)) do
    if h.kind ~= 'tokens' then
      for _, o in ipairs(handObjects(color, h.idx)) do s[o.getGUID()] = true end
    end
  end
  return s
end

-- spots: a seat's packs on the row nearest the centre, the AI's pool on its
-- 'other' row, the Winston stack and piles between the two drafters
local function packSpot(color, r, rounds)
  local d = seatData(color)
  local off = (r - (rounds + 1) / 2) * 3.4
  return matPoint(d, rowSpec(d, 'creature').rows[1], off, d.playmat.getPosition().y + 0.6)
end

local function poolSpot()
  local d = seatData(state.seat)
  return matPoint(d, rowSpec(d, 'other').rows[1], 0, d.playmat.getPosition().y + 0.6)
end

-- The row's centre starts halfway between the mats' midpoint and the
-- table's centre (for two drafters facing each other, the seam between
-- their mats) and moves 4 towards the middle of the AI's own mat: cards
-- left on the seam slid off their spots and the pad lost the stack.
local function winstonSpots(a, b)
  local pa, pb = seatData(a).playmat.getPosition(), seatData(b).playmat.getPosition()
  local mid = { x = (pa.x + pb.x) / 4, y = math.max(pa.y, pb.y) + 0.6, z = (pa.z + pb.z) / 4 }
  local own = seatData(state.seat).playmat.getPosition()
  local ox, oz = own.x - mid.x, own.z - mid.z
  local olen = math.sqrt(ox * ox + oz * oz)
  if olen > 0.01 then mid.x, mid.z = mid.x + ox / olen * 4, mid.z + oz / olen * 4 end
  local dx, dz = pb.x - pa.x, pb.z - pa.z
  local len = math.sqrt(dx * dx + dz * dz)
  if len < 0.01 then dx, dz, len = 1, 0, 1 end
  local rx, rz = -dz / len, dx / len -- across the line between the two mats
  local function at(k) return { x = mid.x + rx * k * 3, y = mid.y, z = mid.z + rz * k * 3 } end
  return { stack = at(-1.5), piles = { at(-0.5), at(0.5), at(1.5) } }
end

-- for ops defined above the drafting section
function winstonSpotsFor(a, b) return winstonSpots(a, b) end

local function toPool(guids)
  guidsToSpot(guids, poolSpot(), yawOf(seatData(state.seat).playmat))
end

local function draftSay(msg) broadcastToAll('[Nemesis] ' .. msg, { 0.85, 0.75, 1 }) end

local function tellColor(color, msg, rgb)
  pcall(function() broadcastToColor('[Nemesis] ' .. msg, color, rgb or { 0.85, 0.75, 1 }) end)
end

-- a cube's packs: names only, decided here, dealt without replacement
local function makePacks(req, n, size)
  local deck = shuffled(req.cube or {})
  if #deck < n * size then error('the cube has ' .. #deck .. ' cards; ' .. n .. ' packs of ' .. size .. ' need ' .. n * size) end
  local packs = {}
  for p = 1, n do
    local pack = {}
    for i = 1, size do pack[i] = deck[(p - 1) * size + i] end
    packs[p] = pack
  end
  return packs
end

-- No pick: the token cards the importer adds for every card that makes
-- tokens, and the basic lands in a play booster's land slot (the table has
-- a box of basics; deck building adds them for free). Dual lands stay.
local function isNoPick(nick)
  local typeLine = tostring(nick or ''):match('\n([^\n]*)') or ''
  return typeLine:find('Token') ~= nil or (typeLine:find('Basic') ~= nil and typeLine:find('Land') ~= nil)
end

-- take the no-pick cards out of spawned decks and cards, and destroy them
local function stripTokens(objs)
  local n = 0
  for _, o in ipairs(objs) do
    if not o.isDestroyed() then
      if o.type == 'Deck' then
        local deck = o
        for _, c in ipairs(o.getObjects()) do
          if isNoPick(c.name) and deck.type == 'Deck' then
            local p = deck.getPosition()
            local t = deck.takeObject({ guid = c.guid, position = { p.x, p.y + 3, p.z }, smooth = false })
            if deck.remainder ~= nil then deck = deck.remainder end
            if t then destroyObject(t); n = n + 1 end
          end
        end
      elseif isNoPick(o.getName()) then
        destroyObject(o); n = n + 1
      end
    end
  end
  return n
end

-- A set's boosters, made by the importer itself (its /draft follows each
-- set's real collation): three packs per hand, each a deck nicknamed
-- 'Player N - Pack R'. Pack R of drafter N is spawned face-down at
-- spot(N, R); fn(spawned, issues) once they have settled. extra asks for
-- that many more hands' worth (Winston needs more than two drafters' packs).
local function importBoosters(url, set, seats, spot, yaw, fn, extra)
  local hands = {}
  for i = 1, #seats + (extra or 0) do
    local c = seats[((i - 1) % #seats) + 1]
    local tr = Player[c].getHandTransform(1)
    hands[#hands + 1] = { position = tr.position, forward = tr.forward, right = tr.right }
  end
  local body = JSON.encode({ set = set, hands = hands, useStates = true })
  WebRequest.custom(url .. '/draft', 'POST', true, body, {
    ['Content-Type'] = 'application/json', Accept = 'application/x-ndjson',
    ['X-Client-Version'] = 'Nemesis/' .. NEMESIS_VERSION,
  }, function(resp)
    if resp.error ~= nil then
      local msg = tostring(resp.error)
      local ok, e = pcall(function() return JSON.decode(resp.text) end)
      if ok and type(e) == 'table' and e.error then msg = e.error end
      fn(nil, { msg })
      return
    end
    local spawned, issues, k = {}, {}, 0
    for line in string.gmatch(resp.text, '[^\r\n]+') do
      if string.sub(line, 1, 9) == '{"error":' then
        local ok, e = pcall(function() return JSON.decode(line) end)
        if ok and e.error then issues[#issues + 1] = e.error end
      else
        k = k + 1
        local who, r = line:match('"Nickname":"Player (%d+) %- Pack (%d+)"')
        local at = spot(tonumber(who) or 1, tonumber(r) or 1)
        local o = spawnObjectJSON({ json = line, position = { at.x, at.y + 1 + k * 0.4, at.z }, rotation = faceDownYaw(yaw(tonumber(who) or 1)) })
        if o then state.touched[o.getGUID()] = Time.time; spawned[#spawned + 1] = o end
      end
    end
    Wait.condition(function() fn(spawned, issues) end, function()
      for _, o in ipairs(spawned) do
        if not o.isDestroyed() and (o.spawning or not o.resting) then return false end
      end
      return true
    end, 20, function() fn(spawned, issues) end)
  end)
end

-- build a list of names on the importer, spawn it face-down at `at`, then
-- call fn(spawned objects, problems) once they have settled
local function importNames(url, names, at, yaw, fn)
  local counts, order = {}, {}
  for _, n in ipairs(names) do
    if counts[n] == nil then order[#order + 1] = n; counts[n] = 0 end
    counts[n] = counts[n] + 1
  end
  local lines = {}
  for _, n in ipairs(order) do lines[#lines + 1] = counts[n] .. ' ' .. n end
  local body = JSON.encode({ data = table.concat(lines, '\n'), useStates = true, preferOriginalPrinting = false,
    hand = Player[state.seat].getHandTransform(1) })
  WebRequest.custom(url .. '/build', 'POST', true, body, {
    ['Content-Type'] = 'application/json', Accept = 'application/x-ndjson',
    ['X-Client-Version'] = 'Nemesis/' .. NEMESIS_VERSION,
  }, function(resp)
    if resp.error ~= nil then fn(nil, { tostring(resp.error) }) return end
    local spawned, issues, k = {}, {}, 0
    for line in string.gmatch(resp.text, '[^\r\n]+') do
      if string.sub(line, 1, 9) == '{"error":' then
        local ok, e = pcall(function() return JSON.decode(line) end)
        if ok and e.error then issues[#issues + 1] = e.error end
      else
        k = k + 1
        local o = spawnObjectJSON({ json = line, position = { at.x, at.y + 2 + k * 0.6, at.z }, rotation = faceDownYaw(yaw) })
        if o then state.touched[o.getGUID()] = Time.time; spawned[#spawned + 1] = o end
      end
    end
    Wait.condition(function() fn(spawned, issues) end, function()
      for _, o in ipairs(spawned) do
        if not o.isDestroyed() and (o.spawning or not o.resting) then return false end
      end
      return true
    end, 20, function() fn(spawned, issues) end)
  end)
end

-- split a seat's spawned pool into its packs by name, one spot per pack;
-- returns how many cards the importer did not build
local function splitPacks(spawned, packs, spots, yaw)
  local byName = {}
  local function push(key, e) byName[key] = byName[key] or {}; table.insert(byName[key], e) end
  for _, o in ipairs(spawned) do
    if not o.isDestroyed() then
      if o.type == 'Deck' then
        local src = { obj = o }
        for _, c in ipairs(o.getObjects()) do push(draftKey(c.name), { src = src, guid = c.guid }) end
      else
        push(draftKey(o.getName()), { card = o })
      end
    end
  end
  local short = 0
  for p, names in ipairs(packs) do
    local spot = spots[p]
    for i, n in ipairs(names) do
      local list = byName[draftKey(n)]
      local e = list and table.remove(list, 1)
      local pos = { spot.x, spot.y + 0.4 + i * 0.05, spot.z }
      if e == nil then
        short = short + 1
      elseif e.card then
        e.card.setRotation(faceDownYaw(yaw)); e.card.setPosition(pos)
      elseif e.src.obj.type == 'Deck' then
        local c = e.src.obj.takeObject({ guid = e.guid, position = pos, rotation = faceDownYaw(yaw), smooth = false })
        if e.src.obj.remainder ~= nil then e.src.obj = e.src.obj.remainder end
        if c then state.touched[c.getGUID()] = Time.time end
      else
        e.src.obj.setRotation(faceDownYaw(yaw)); e.src.obj.setPosition(pos) -- the deck's last card
      end
    end
  end
  -- whatever the importer built on top (a second printing, a helper card)
  -- would fall onto pack 1
  for _, list in pairs(byName) do
    for _, e in ipairs(list) do
      local o = e.card or e.src.obj
      if o and not o.isDestroyed() then pcall(function() destroyObject(o) end) end
    end
  end
  return short
end

local function tidy(pos)
  Wait.time(function()
    local objs = cardsAt(pos)
    if #objs > 1 then pcall(function() group(objs) end) end
  end, 1.5)
end

function draftQuiet() return state.draft ~= nil end

local FORMATS = { booster = 'Booster draft', pick_two = 'Pick-two draft', winston = 'Winston draft' }

local function emitMyPack()
  local dr = state.draft
  local h = dr and dr.hands[state.seat]
  if h == nil or #h.guids == 0 then return end
  local list = {}
  for _, g in ipairs(h.guids) do
    local o = getObjectFromGUID(g)
    if o then list[#list + 1] = { guid = g, nick = o.getName(), desc = o.getDescription() } end
  end
  emit({ ev = 'draft', what = 'pack', round = dr.round, rounds = dr.rounds, pick = dr.pick, need = h.need,
    pool = countAt(poolSpot()), cards = JSON.encode(list) })
end

function openRound(r)
  local dr = state.draft
  dr.round, dr.pick, dr.dir, dr.hands, dr.waiting = r, 1, (r % 2 == 1) and 1 or -1, {}, {}
  for _, c in ipairs(dr.seats) do
    local guids = spotToHand(packSpot(c, r, dr.rounds), c)
    dr.hands[c] = { guids = guids, need = math.min(dr.picks, #guids), since = Time.time }
  end
  local toward = dr.dir == 1 and dr.seats[2] or dr.seats[#dr.seats]
  draftSay('Pack ' .. r .. ' of ' .. dr.rounds .. ': passing towards ' .. toward .. '.')
  setStatus('Draft: pack ' .. r .. ', pick 1')
  Wait.time(emitMyPack, 1)
end

local function finishDraft()
  local dr = state.draft
  dr.phase, dr.hands, dr.waiting, dr.view = 'done', {}, {}, nil
  draftSay('The draft is over. Build a 40-card deck from your picks plus basic lands; the rest is your sideboard.')
  setStatus('Draft over: deck building')
  emit({ ev = 'draft', what = 'done', pool = countAt(poolSpot()) })
end

local function passPacks()
  local dr = state.draft
  local moves, total = {}, 0
  for i, c in ipairs(dr.seats) do
    local h = dr.hands[c]
    local inHand = inHandSet(c)
    local rest = {}
    for _, g in ipairs(h and h.guids or {}) do if inHand[g] then rest[#rest + 1] = g end end
    moves[dr.seats[((i - 1 + dr.dir) % #dr.seats) + 1]] = rest
    total = total + #rest
  end
  dr.hands = {}
  if total == 0 then
    if dr.round >= dr.rounds then finishDraft() else openRound(dr.round + 1) end
    return
  end
  dr.pick = dr.pick + 1
  for to, rest in pairs(moves) do
    local hp, hr = handSpot(to)
    for k, g in ipairs(rest) do
      local o = getObjectFromGUID(g)
      if o then
        state.touched[g] = Time.time
        o.setRotation({ 0, hr.y + 180, 0 })
        o.setPosition({ hp.x, hp.y + 0.3 + k * 0.05, hp.z })
      end
    end
    dr.hands[to] = { guids = rest, need = math.min(dr.picks, #rest), since = Time.time }
  end
  setStatus('Draft: pack ' .. dr.round .. ', pick ' .. dr.pick)
  Wait.time(emitMyPack, 1)
end

-- every 1.5 s: who has taken their pick(s) out of the pack in their hand?
-- A human counts as done after two polls in a row with the right number out
-- (a card dragged out to look at and put back is not a pick).
function pollDraft()
  local dr = state.draft
  if dr == nil or dr.phase ~= 'picking' or dr.format == 'winston' then return end
  local waiting = {}
  for _, c in ipairs(dr.seats) do
    local h = dr.hands[c]
    if h and h.need > 0 then
      local done = false
      if Time.time - h.since > 2.5 then
        local inHand = inHandSet(c)
        local left = 0
        for _, g in ipairs(h.guids) do if inHand[g] then left = left + 1 end end
        local picked = #h.guids - left
        h.ok = (picked == h.need) and (h.ok or 0) + 1 or 0
        if picked > h.need and not h.warned and c ~= state.seat then
          h.warned = true
          tellColor(c, 'Take only ' .. h.need .. ' from this pack: put ' .. (picked - h.need) .. ' back in your hand.', { 1, 0.6, 0.4 })
        end
        done = h.ok >= ((c == state.seat) and 1 or 2)
      end
      if not done then waiting[#waiting + 1] = c end
    end
  end
  local was = table.concat(dr.waiting or {}, ',')
  dr.waiting = waiting
  if #waiting == 0 then
    passPacks()
  elseif table.concat(waiting, ',') ~= was then
    setStatus('Draft: pack ' .. dr.round .. ', pick ' .. dr.pick .. ' - waiting on ' .. table.concat(waiting, ', '))
  end
end

-- Winston -------------------------------------------------------------------

local function wSpots() local dr = state.draft return winstonSpots(dr.seats[1], dr.seats[2]) end
local function wYaw() return yawOf(seatData(state.seat).playmat) end
local function wOther(c) local dr = state.draft return dr.seats[1] == c and dr.seats[2] or dr.seats[1] end

-- the stack's top card face-down onto a spot; nil when the stack is empty.
-- A basic land or token that slipped past the strip at build time (the
-- boosters were still merging) is destroyed and the next card dealt.
local function wTopTo(pos)
  for _ = 1, 20 do
    local o = cardsAt(wSpots().stack)[1]
    if o == nil then return nil end
    local at = { pos.x, pos.y + 0.4 + countAt(pos) * 0.05, pos.z }
    local c = o
    if o.type == 'Deck' then
      c = o.takeObject({ position = at, rotation = faceDownYaw(wYaw()), smooth = false })
    else
      o.setRotation(faceDownYaw(wYaw())); o.setPosition(at)
    end
    if c == nil then return nil end
    if isNoPick(c.getName()) then
      pcall(function() destroyObject(c) end)
    else
      state.touched[c.getGUID()] = Time.time
      return c
    end
  end
  return nil
end

local function wSizes()
  local s = wSpots()
  return { countAt(s.piles[1]), countAt(s.piles[2]), countAt(s.piles[3]) }, countAt(s.stack)
end

local function wNextPile(from)
  local sizes = wSizes()
  for i = from, 3 do if sizes[i] > 0 then return i end end
  return nil
end

function wNextPileFor(from) return wNextPile(from) end

-- quiet: the AI just passed and draft_pick already showed it the next pile
local function wLook(quiet)
  local dr = state.draft
  if dr == nil or dr.phase ~= 'picking' then return end
  local sizes, stack = wSizes()
  if dr.turn == state.seat then
    if quiet then setStatus('Winston: ' .. NAME .. ' looks at pile ' .. dr.pile) return end
    emit({ ev = 'draft', what = 'winston', pile = dr.pile, sizes = table.concat(sizes, '/'), stack = stack,
      pool = countAt(poolSpot()), cards = JSON.encode(listAt(wSpots().piles[dr.pile])) })
    setStatus('Winston: ' .. NAME .. ' looks at pile ' .. dr.pile)
  else
    -- what the hand held before the pile came: a pass sends back everything
    -- new, even a card whose guid the deal missed (a pile deck's last card)
    local spot = wSpots().piles[dr.pile]
    local n = countAt(spot)
    dr.before = inHandSet(dr.turn)
    dr.view = spotToHand(spot, dr.turn)
    dr.viewN = n
    local last = wNextPile(dr.pile + 1) == nil
    pcall(function() showWinstonPad(dr.turn, dr.pile, n, last) end)
    tellColor(dr.turn, 'Pile ' .. dr.pile .. ' (' .. n .. ' cards) is in your hand: say "take" or "pass" (or the pad buttons).')
    setStatus('Winston: ' .. dr.turn .. ' looks at pile ' .. dr.pile)
  end
end

local function wTurn(color)
  local dr = state.draft
  if dr == nil or dr.phase ~= 'picking' then return end
  local _, stack = wSizes()
  dr.turn, dr.view = color, nil
  dr.pile = wNextPile(1)
  if dr.pile == nil then
    if stack == 0 then finishDraft(); buildControls(); return end
    for i = 1, 3 do wTopTo(wSpots().piles[i]) end
    dr.pile = 1
  end
  wLook()
end

function winstonBegin(issues)
  local dr = state.draft
  local s = wSpots()
  for i = 1, 3 do wTopTo(s.piles[i]) end
  dr.phase = 'picking'
  local first = dr.seats[math.random(2)]
  emit({ ev = 'draft', what = 'ready', format = 'winston', seats = table.concat(dr.seats, ', '), packs = 2 * dr.rounds, size = dr.size,
    problems = table.concat(issues or {}, '; '), first = first })
  draftSay('Winston draft: ' .. (countAt(s.stack) + 3) .. ' cards between ' .. dr.seats[1] .. ' and ' .. dr.seats[2] ..
    '. On your turn the pile you look at goes to your hand: take it, or pass and it goes back with one more card. ' .. first .. ' starts.')
  Wait.time(function() wTurn(first) end, 1)
end

-- the drafter on turn takes the pile they are looking at; returns the cards
-- when it is the AI
local function wTake(color)
  local dr = state.draft
  local s = wSpots()
  local pile = dr.pile
  local took, n
  if color == state.seat then
    took = listAt(s.piles[pile])
    local p, yaw = poolSpot(), wYaw()
    local base = countAt(p)
    eachCardAt(s.piles[pile], {
      pos = function(i) return { p.x, p.y + 0.4 + (base + i) * 0.05, p.z } end,
      rot = faceDownYaw(yaw),
      done = function(c) state.touched[c.getGUID()] = Time.time end,
    })
    n = #took
  else
    n = dr.viewN or #(dr.view or {})
    dr.before = nil
  end
  dr.view = nil
  Wait.time(function() wTopTo(s.piles[pile]) end, 0.3)
  draftSay((color == state.seat and NAME or color) .. ' took pile ' .. pile .. ' (' .. n .. ' cards).')
  if color ~= state.seat then emit({ ev = 'draft', what = 'took', color = color, pile = pile, n = n }) end
  Wait.time(function() wTurn(wOther(color)) end, 1.2)
  buildControls()
  return took
end

-- the drafter on turn passes: the pile goes back with one more card, then
-- the next pile, or after the last pile the top card of the stack, blind
local function wPass(color)
  local dr = state.draft
  local s = wSpots()
  local nxt = wNextPile(dr.pile + 1)
  local _, stack = wSizes()
  if nxt == nil and stack == 0 then error('this is the last pile and the stack is empty: take it') end
  if color ~= state.seat then
    local back, seen = {}, {}
    for _, g in ipairs(dr.view or {}) do back[#back + 1] = g; seen[g] = true end
    -- without the snapshot (a pile dealt by an older pad) only the dealt
    -- guids go back: the hand may hold the drafter's earlier picks
    if dr.before then
      for g in pairs(inHandSet(color)) do
        if not dr.before[g] and not seen[g] then back[#back + 1] = g; seen[g] = true end
      end
    end
    guidsToSpot(back, s.piles[dr.pile], wYaw())
    dr.view, dr.before = nil, nil
  end
  wTopTo(s.piles[dr.pile])
  if nxt then
    dr.pile = nxt
    Wait.time(function() wLook(color == state.seat) end, 0.6)
    buildControls()
    return { pile = nxt }
  end
  local c = wTopTo(color == state.seat and poolSpot() or s.stack)
  local blind = nil
  if c and color == state.seat then
    blind = { guid = c.getGUID(), nick = c.getName(), desc = c.getDescription() }
  elseif c then
    local hp, hr = handSpot(color)
    c.setRotation({ 0, hr.y + 180, 0 }); c.setPosition({ hp.x, hp.y + 0.5, hp.z })
  end
  draftSay((color == state.seat and NAME or color) .. ' passed the last pile and took the top card of the stack.')
  if color ~= state.seat then emit({ ev = 'draft', what = 'took', color = color, blind = true, n = c and 1 or 0 }) end
  Wait.time(function() wTurn(wOther(color)) end, 1)
  buildControls()
  return { blind = blind }
end

function winstonChoice(color, take)
  local dr = state.draft
  if dr == nil or dr.format ~= 'winston' or dr.phase ~= 'picking' or color == state.seat then return end
  if dr.turn ~= color then tellColor(color, 'It is ' .. tostring(dr.turn) .. "'s pick.", { 1, 0.6, 0.4 }) return end
  if dr.view == nil then return end
  pcall(removeWinstonPad)
  local ok, err = pcall(function() if take then wTake(color) else wPass(color) end end)
  if not ok then tellColor(color, (tostring(err):gsub('^.-:%d+: ', '')), { 1, 0.6, 0.4 }) end -- pattern-ok: short error text
end
function onWinstonTake(_, color) winstonChoice(color, true) end
function onWinstonPass(_, color) winstonChoice(color, false) end

function draftChat(color, text)
  local dr = state.draft
  if dr == nil or dr.format ~= 'winston' or dr.turn ~= color or type(text) ~= 'string' then return end
  local w = firstWord(text)
  if w == 'take' then winstonChoice(color, true) elseif w == 'pass' then winstonChoice(color, false) end
end

function winstonHumanTurn()
  local dr = state.draft
  if dr ~= nil and dr.format == 'winston' and dr.phase == 'picking' and dr.turn ~= state.seat and dr.view ~= nil then
    return dr.turn, dr.pile
  end
  return nil
end

-- the AI's ops ---------------------------------------------------------------

-- {format=auto|booster|pick_two|winston, seats=[colours]?, url=importer,
--  set=code (the importer's own boosters, 3 per drafter) |
--  cube=[names], packs=3, size=15}
function ops.draft_start(req)
  local seat, d0 = myData()
  if state.draft and state.draft.phase ~= 'done' then error('a draft is already on (cancel_draft first)') end
  math.randomseed(os.time())
  local want = {}
  if type(req.seats) == 'table' then for _, c in ipairs(req.seats) do want[tostring(c)] = true end end
  local named = next(want) ~= nil
  local seats = {}
  for _, c in ipairs(COLORS) do
    if c == seat or (named and want[c]) or (not named and Player[c].seated) then
      local d = seatData(c)
      if d == nil or d.playmat == nil then error('seat ' .. c .. ' has no mat on this table') end
      seats[#seats + 1] = c
    end
  end
  if #seats < 2 then error('a draft needs at least one human drafter seated (or named in seats)') end
  local format = req.format
  if format == nil or format == '' or format == 'auto' then
    format = (#seats == 2 and 'winston') or (#seats <= 4 and 'pick_two') or 'booster'
  end
  if FORMATS[format] == nil then error('format must be auto, booster, pick_two or winston') end
  if format == 'winston' and #seats ~= 2 then error('Winston is for exactly two drafters') end
  local set = (type(req.set) == 'string' and req.set ~= '') and req.set:lower() or nil
  local rounds = math.max(1, math.min(6, tonumber(req.packs) or 3))
  local size = math.max(5, math.min(20, tonumber(req.size) or 15))
  if set then rounds, size = 3, 13 end -- the importer's play boosters, basics out
  local url = (type(req.url) == 'string' and req.url ~= '') and req.url or IMPORTER
  -- a cube's packs are checked before anything moves
  local first = nil
  if not set then
    first = makePacks(req, (format == 'winston' and 2 or #seats) * rounds, size)
    if #first[1] == 0 then error('no cards to make packs from') end
  end
  local dr = { format = format, picks = format == 'pick_two' and 2 or 1, seats = seats, rounds = rounds, size = size,
    phase = 'building', round = 0, pick = 0, hands = {}, waiting = {} }
  state.draft = dr
  local function failed(issues)
    if state.draft ~= dr then return end
    state.draft = nil
    setStatus('Draft: the packs could not be built')
    emit({ ev = 'draft', what = 'error', error = table.concat(issues or {}, '; ') })
  end
  if format == 'winston' then
    local spots = winstonSpots(seats[1], seats[2])
    local yaw = yawOf(d0.playmat)
    -- every pack goes onto the one stack, shuffled
    -- the standard Winston stack: 90 cards (six 15-card boosters' worth).
    -- Boosters spawned on one spot merge as they land, so the no-pick cards
    -- come out of whatever deck is there once it has been grouped.
    local function stacked(issues, cap)
      local objs = cardsAt(spots.stack, 2)
      if #objs > 1 then pcall(function() group(objs) end) end
      Wait.time(function()
        if cap then stripTokens(cardsAt(spots.stack, 2)) end
        for _, o in ipairs(cardsAt(spots.stack)) do
          if o.type == 'Deck' then
            o.shuffle()
            local extra = cap and (o.getQuantity() - cap) or 0
            local p = o.getPosition()
            for _ = 1, extra do
              local c = o.takeObject({ position = { p.x, p.y + 3, p.z }, smooth = false })
              if c then destroyObject(c) end
            end
          end
        end
        Wait.time(function() if state.draft == dr then winstonBegin(issues) end end, 0.8)
      end, 1.5)
    end
    if set then
      importBoosters(url, set, seats, function() return spots.stack end, function() return yaw end, function(spawned, issues)
        if state.draft ~= dr then return end
        if spawned == nil or #spawned == 0 then return failed(issues) end
        Wait.time(function() if state.draft == dr then stacked(issues, 90) end end, 0.5)
      end, 1) -- nine boosters: about 117 cards without basics, cut to 90
    else
      local names = {}
      for _, p in ipairs(first) do for _, n in ipairs(p) do names[#names + 1] = n end end
      importNames(url, names, spots.stack, yaw, function(spawned, issues)
        if state.draft ~= dr then return end
        if spawned == nil or #spawned == 0 then return failed(issues) end
        -- only the named cards: the importer's extra token cards go
        splitPacks(spawned, { names }, { spots.stack }, yaw)
        stacked(issues)
      end)
    end
    setStatus('Draft: building packs...')
    return { format = format, seats = seats, packs = set and 9 or 2 * rounds, size = size, picks = dr.picks, stack = set and 90 or 2 * rounds * size }
  end
  local function ready(issues, short)
    Wait.time(function()
      if state.draft ~= dr then return end
      dr.phase = 'picking'
      local probs = table.concat(issues, '; ')
      if short > 0 then probs = (probs ~= '' and probs .. '; ' or '') .. short .. ' cards could not be built' end
      emit({ ev = 'draft', what = 'ready', format = format, seats = table.concat(seats, ', '), packs = rounds, size = dr.size, problems = probs })
      draftSay(FORMATS[format] .. ': ' .. rounds .. ' packs of ' .. dr.size .. ' for ' .. table.concat(seats, ', ') ..
        '. Your pack lands in your hand: take ' .. dr.picks .. ' card' .. (dr.picks > 1 and 's' or '') ..
        ' out, face-down onto your mat. Packs move on once everyone has picked.')
      openRound(1)
    end, 2.5)
  end
  if set then
    importBoosters(url, set, seats,
      function(n, r) return packSpot(seats[n] or seats[1], r, rounds) end,
      function(n) return yawOf(seatData(seats[n] or seats[1]).playmat) end,
      function(spawned, issues)
        if state.draft ~= dr then return end
        if spawned == nil or #spawned == 0 then return failed(issues) end
        Wait.time(function()
          if state.draft ~= dr then return end
          for _, c in ipairs(seats) do for r = 1, rounds do stripTokens(cardsAt(packSpot(c, r, rounds))) end end
          local n = countAt(packSpot(seats[1], 1, rounds))
          if n > 0 then dr.size = n end
          for _, c in ipairs(seats) do for r = 1, rounds do tidy(packSpot(c, r, rounds)) end end
          ready(issues or {}, 0)
        end, 0.5)
      end)
  else
    local pending, built = #seats, 0
    local allIssues, short = {}, 0
    for si, c in ipairs(seats) do
      local packs, names, spots = {}, {}, {}
      for r = 1, rounds do
        packs[r] = first[(si - 1) * rounds + r]
        spots[r] = packSpot(c, r, rounds)
        for _, n in ipairs(packs[r]) do names[#names + 1] = n end
      end
      local yaw = yawOf(seatData(c).playmat)
      importNames(url, names, spots[1], yaw, function(spawned, issues)
        if state.draft ~= dr then return end
        for _, i in ipairs(issues or {}) do allIssues[#allIssues + 1] = i end
        if spawned and #spawned > 0 then
          built = built + 1
          short = short + splitPacks(spawned, packs, spots, yaw)
          for _, s in ipairs(spots) do tidy(s) end
        end
        pending = pending - 1
        if pending > 0 then return end
        if built < #seats then return failed(allIssues) end
        ready(allIssues, short)
      end)
    end
  end
  setStatus('Draft: building packs...')
  return { format = format, seats = seats, packs = rounds, size = size, picks = dr.picks }
end

-- {guids=[...]} (booster, pick-two) or {take=bool} (Winston)
function ops.draft_pick(req)
  local seat = requireSeat()
  local dr = state.draft
  if dr == nil or dr.phase ~= 'picking' then error('no draft is picking right now') end
  if dr.format == 'winston' then
    if dr.turn ~= seat then error('it is ' .. tostring(dr.turn) .. "'s pick, not yours") end
    if req.take == true then return { took = wTake(seat) } end
    local r = wPass(seat)
    if r.pile then
      local sizes, stack = wSizes()
      return { passed = true, pile = r.pile, sizes = table.concat(sizes, '/'), stack = stack, cards = listAt(wSpots().piles[r.pile]) }
    end
    return { passed = true, blind = r.blind }
  end
  local h = dr.hands[seat]
  if h == nil or h.need == 0 then error('no pack in your hand yet; it comes as a [draft] event') end
  local inHand = inHandSet(seat)
  local inPack, left = {}, 0
  for _, g in ipairs(h.guids) do inPack[g] = true; if inHand[g] then left = left + 1 end end
  local guids = req.guids or {}
  local need = h.need - (#h.guids - left)
  if need <= 0 then error('you already picked from this pack; the next one comes as a [draft] event') end
  if #guids ~= need then error('take exactly ' .. need .. ' card(s) from this pack') end
  local names = {}
  for _, g in ipairs(guids) do
    if not inPack[g] or not inHand[g] then error('not a card of your current pack: ' .. tostring(g)) end
    names[#names + 1] = displayName(getObjectFromGUID(g).getName())
  end
  local before = countAt(poolSpot())
  toPool(guids)
  local waiting = {}
  for _, c in ipairs(dr.waiting or {}) do if c ~= seat then waiting[#waiting + 1] = c end end
  return { picked = names, pool = before + #guids, waitingOn = waiting, round = dr.round, pick = dr.pick }
end

function ops.draft_view(req)
  local seat = requireSeat()
  local dr = state.draft
  if dr == nil then return { draft = false } end
  local out = { draft = true, format = dr.format, phase = dr.phase, round = dr.round, rounds = dr.rounds, pick = dr.pick,
    picks = dr.picks, seats = dr.seats, waitingOn = dr.waiting or {}, pool = listAt(poolSpot()) }
  local h = dr.hands[seat]
  if dr.format ~= 'winston' and h and h.need > 0 then
    local inHand = inHandSet(seat)
    local cards = {}
    for _, g in ipairs(h.guids) do
      local o = getObjectFromGUID(g)
      if o and inHand[g] then cards[#cards + 1] = { guid = g, nick = o.getName(), desc = o.getDescription() } end
    end
    out.pack, out.need = cards, h.need - (#h.guids - #cards)
  end
  if dr.format == 'winston' and dr.phase == 'picking' then
    local sizes, stack = wSizes()
    out.turn, out.pile, out.sizes, out.stack = dr.turn, dr.pile, table.concat(sizes, '/'), stack
    if dr.turn == seat then out.cards = listAt(wSpots().piles[dr.pile]) end
  end
  return out
end

-- {main=[pool guids], basics={Forest=8,...}, url=importer}: the chosen
-- cards and the basics become the AI's library (shuffled), the rest of the
-- pool its sideboard beside it; life goes to 20
function ops.draft_build(req)
  local seat, d = myData()
  local dr = state.draft
  if dr ~= nil and dr.phase ~= 'done' then error('the draft is still on: build once it is over') end
  local pool = poolSpot()
  if countAt(pool) == 0 then error('no pool on your mat to build from') end
  local want = {}
  for _, g in ipairs(req.main or {}) do want[g] = true end
  local lp = d.libraryZone.getPosition()
  local r = d.playmat.getTransformRight()
  local side = { x = lp.x + r.x * 4, y = lp.y, z = lp.z + r.z * 4 }
  if #cardsAt(lp) > 0 then error('your library spot is not empty: reset_board first') end
  local yaw = yawOf(d.playmat)
  local mainN, sideN = 0, 0
  eachCardAt(pool, {
    pos = function(i) return { side.x, side.y + 1 + i * 0.05, side.z } end,
    rot = faceDownYaw(yaw),
    done = function(c)
      state.touched[c.getGUID()] = Time.time
      if want[c.getGUID()] then
        mainN = mainN + 1
        c.setPosition({ lp.x, lp.y + 1 + mainN * 0.05, lp.z })
      else
        sideN = sideN + 1
      end
    end,
  })
  local names, nb = {}, 0
  for n, k in pairs(req.basics or {}) do
    for _ = 1, math.max(0, math.min(40, tonumber(k) or 0)) do names[#names + 1] = n; nb = nb + 1 end
  end
  local function finish(issues)
    Wait.time(function()
      for _, spot in ipairs({ lp, side }) do
        local objs = cardsAt(spot)
        if #objs > 1 then pcall(function() group(objs) end) end
      end
      Wait.time(function()
        local lib = libraryTop(d)
        if lib and lib.type == 'Deck' then lib.shuffle() end
        state.looked = {}
        emit({ ev = 'draft', what = 'built', main = mainN, basics = nb, sideboard = sideN, library = countAt(lp),
          problems = table.concat(issues or {}, '; ') })
      end, 1.5)
    end, 1)
  end
  if #names > 0 then
    importNames((type(req.url) == 'string' and req.url ~= '') and req.url or IMPORTER, names, lp, yaw,
      function(_, issues) finish(issues) end)
  else
    finish({})
  end
  if d.lifeTracker then setLife(d.lifeTracker, 20) end
  state.gameTurns = 0
  return { main = mainN, sideboard = sideN, basics = nb, missing = #(req.main or {}) - mainN }
end

-- the draft is over for the pad: zone and play reports come back
function endDraft()
  if state.draft == nil then return end
  pcall(removeWinstonPad)
  state.draft = nil
  pcall(seedField)
  buildControls()
end

function ops.draft_cancel(req)
  requireSeat()
  if state.draft == nil then return { cancelled = false } end
  endDraft()
  draftSay('The draft was called off; its cards stay where they are.')
  setStatus('Draft cancelled')
  return { cancelled = true }
end
