-- The permission bridge: answer a tool permission prompt from the gateway.
--
-- `permission.prompt` fires where the human prompt would show, and a layer may
-- park, so this wrapper publishes a `permission` event, waits for the matching
-- `respond`, and otherwise hands the call back to the terminal prompt. The
-- host gives a layer 60 seconds before it stops listening, so the wait stops at
-- 55 and names the withdrawal on the wire.
--
-- Every wait is a `maki.async.sleep`, which yields the shared Lua thread. No
-- lock is held here, and no state is kept outside these two module tables.

local protocol = require("gateway.protocol")
local conns = require("gateway.conns")

local M = {}

local POLL_MS = 500
local PERMISSION_WAIT_MS = 55000
local EXPIRED = "expired"

local bridge_on = false
local counter = 0
local pending = {}

--- Whose call it is. ctx is the agent ctx, then the focused tab, then "".
local function session_id(ctx)
  if ctx then
    local called, id = pcall(function()
      return ctx:session_id()
    end)
    if called and id ~= nil then
      return protocol.str(id)
    end
  end
  local current = maki.session.current()
  return protocol.str(current)
end

function M.set_enabled(on)
  bridge_on = on and true or false
  maki.log.info("gateway: permission bridge " .. (bridge_on and "on" or "off"))
  return true
end

function M.is_enabled()
  return bridge_on
end

--- Answer a `permission` event. request_id is the one the event carried.
function M.respond(request_id, decision, guidance)
  if type(request_id) ~= "number" then
    return nil, "request_id must be a number"
  end
  if decision ~= "allow" and decision ~= "deny" then
    return nil, "decision must be allow or deny"
  end
  local entry = pending[math.floor(request_id)]
  if not entry or entry.answer or entry.timed_out then
    return nil, EXPIRED
  end
  entry.answer = { decision = decision, guidance = guidance }
  return true
end

--- The layer for the `permission.prompt` slot. Registered once, at load.
function M.wrapper(prev, req, ctx)
  if not bridge_on or conns.count() == 0 then
    return prev(req, ctx)
  end
  counter = counter + 1
  local id = counter
  local event = {
    request_id = id,
    session = session_id(ctx),
    tool = protocol.str(req and req.tool),
    input = protocol.object(req and req.input),
  }
  local entry = { event = event }
  pending[id] = entry
  protocol.emit("permission", event)
  local waited = 0
  while waited < PERMISSION_WAIT_MS do
    if entry.answer then
      break
    end
    local called, err = pcall(maki.async.sleep, POLL_MS)
    if not called then
      maki.log.debug("gateway: permission wait stopped (" .. tostring(err) .. ")")
      break
    end
    waited = waited + POLL_MS
  end
  if entry.answer then
    pending[id] = nil
    if entry.answer.decision == "deny" then
      return { decision = "deny", guidance = protocol.str(entry.answer.guidance) }
    end
    return { decision = "allow" }
  end
  -- Nobody answered. The terminal prompt takes the call, and the gateway gets
  -- word that its dialog is gone so it can drop the request id.
  entry.timed_out = true
  if pending[id] == entry then
    pending[id] = nil
  end
  protocol.emit("permission_cancel", { request_id = id })
  return prev(req, ctx)
end

return M
