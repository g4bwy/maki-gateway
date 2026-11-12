-- The one turn a gateway started, from the ask to the answer.
--
-- WIRE 7.1 answers `session/prompt` at the end of the turn, and refuses a second
-- prompt for a tab that is already running one: queueing is `_sh.maki/queue`.
-- Maki itself queues, so this file is where the plugin mirrors the ACP
-- admission, and where the answer waits on the host's own `TurnEnd` rather than
-- on a clock. A turn that runs for twenty minutes is legal.
--
-- One claim per tab. The host answers a prompt with "started" or "queued", and
-- "queued" means the terminal started a turn between the admission check and the
-- call, so that claim waits for the `TurnStart` of its own text before it
-- listens for the end of a turn.

local protocol = require("gateway.protocol")

local M = {}

local TURN_POLL_MS = 100
-- The prompt of a tab nothing runs and whose host queue is empty has been taken
-- out at the terminal. Several waits in a row must say so before the plugin
-- gives up on it, because the host empties the queue one step before the turn
-- it holds begins.
local QUIET_WAITS = 4
local STATUS_WORKING = "working"

-- session id -> claim
local claims = {}

--- Whether this tab has a prompt of this plugin parked on it.
function M.parked(session)
  return claims[session] ~= nil
end

--- Stop {session} from taking another prompt until its turn is settled. Answers
--- nil when a prompt is already parked there, which the caller answers busy.
function M.claim(session, text, entry)
  if M.parked(session) then
    return nil
  end
  local claim = { session = session, text = text, entry = entry, quiet = 0 }
  claims[session] = claim
  return claim
end

--- Let the tab take its next prompt.
local function release(claim)
  claim.done = true
  if claims[claim.session] == claim then
    claims[claim.session] = nil
  end
end

M.release = release

--- The host took the text. "started" means a turn runs now, so its end is the
--- end of this prompt. "queued" means the tab was busy after all.
function M.arm(claim, state)
  claim.armed = state ~= "queued"
end

--- A turn began in this tab. An unarmed claim takes it when the host shows the
--- same text, or shows no text at all, which is what a plugin that was not
--- granted the content of WIRE 13 gets.
function M.turn_start(session, text)
  local claim = claims[session]
  if claim == nil or claim.armed or claim.done then
    return
  end
  if text == nil or text == "" or text == claim.text then
    claim.armed = true
  end
end

--- The stopReason WIRE 7.1 gives the host's word for why a turn ended.
function M.stop_for(reason)
  if reason == "max_tokens" then
    return "max_tokens"
  end
  if reason == "max_turns" then
    return "max_turn_requests"
  end
  if reason == "cancelled" then
    return "cancelled"
  end
  if reason == "dropped" then
    return "refusal"
  end
  return "end_turn"
end

--- A turn ended in this tab. {reason} is why the host says it ended.
function M.turn_end(session, reason)
  local claim = claims[session]
  if claim == nil or not claim.armed or claim.done then
    return
  end
  claim.stop = M.stop_for(reason)
  claim.done = true
end

--- The turn died on an error. The prompt is answered rather than left parked: the
--- host put its words on the screen and in the log, and WIRE 7.1 has no stop
--- reason for a run that never reached the model.
function M.turn_error(session)
  local claim = claims[session]
  if claim == nil or not claim.armed or claim.done then
    return
  end
  claim.stop = "end_turn"
  claim.done = true
end

--- The tab is gone, so its turn can never end. WIRE 13's -32000 carries the
--- words of 13.1, so the caller reads a tab that is gone.
function M.session_end(session)
  local claim = claims[session]
  if claim == nil then
    return
  end
  local why = protocol.NOT_LIVE .. ": " .. session .. " (the tab closed)"
  claim.failure = protocol.err(protocol.E_HELD, why)
  claim.done = true
end

--- True when the host runs no turn here and holds no queued text: the person at
--- the terminal took this prompt out of the queue.
local function quiet(claim)
  local called, snap = pcall(maki.session.read, { session = claim.session })
  if not called or type(snap) ~= "table" or snap.status == STATUS_WORKING then
    claim.quiet = 0
    return false
  end
  local queue = snap.queue
  if queue == nil or tonumber(queue.count) ~= 0 then
    claim.quiet = 0
    return false
  end
  claim.quiet = claim.quiet + 1
  return claim.quiet >= QUIET_WAITS
end

--- Park the calling task until the turn of {claim} is settled, and answer the
--- stopReason or the refusal. No deadline (WIRE 7.1).
function M.wait(claim)
  while not claim.done do
    if claim.entry == nil or claim.entry.closed then
      release(claim)
      return nil, protocol.err(protocol.E_INTERNAL, "the connection that asked this prompt closed")
    end
    if not claim.armed and claims[claim.session] == claim and quiet(claim) then
      release(claim)
      local why = "the terminal took this prompt out of the queue"
      return nil, protocol.err(protocol.E_INTERNAL, why)
    end
    local called, why = pcall(maki.async.sleep, TURN_POLL_MS)
    if not called then
      maki.log.debug("gateway: turn wait stopped (" .. tostring(why) .. ")")
    end
  end
  local stop, failure = claim.stop, claim.failure
  release(claim)
  if failure ~= nil then
    return nil, failure
  end
  return stop
end

--- Drop every claim of a connection that went away: nothing can read its answer.
function M.drop_conn(entry)
  for session, claim in pairs(claims) do
    if claim.entry == entry then
      release(claim)
      maki.log.debug("gateway: a prompt of " .. session .. " lost its connection")
    end
  end
end

return M
