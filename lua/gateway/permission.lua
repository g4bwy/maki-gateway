-- The permission bridge: answer a tool permission prompt from the gateway.
--
-- `permission.prompt` fires where the human prompt would show, and a layer may
-- park, so this wrapper raises one native `session/request_permission` per
-- connection that finished the handshake, waits for the first answer, and lets
-- the terminal prompt take the call when nobody answered in time. The host gives
-- a layer 60 seconds before it stops listening, so the wait stops at 55.
--
-- The bridge is on while a client is connected (WIRE 10.4). v1's `permissions` op
-- and its `respond` op are gone: the ask is a request this side sends, and its
-- answer is the JSON-RPC answer to that request.
--
-- Every wait is a `maki.async.sleep`, which yields this task and no other. No
-- lock is held here, and the only state is the ask record conns.lua routes
-- answers into.

local protocol = require("gateway.protocol")
local conns = require("gateway.conns")

local M = {}

local POLL_MS = 500
local PERMISSION_WAIT_MS = 55000
local ALLOW = "allow_once"
local REJECT = "reject_once"
local SELECTED = "selected"
local GUIDANCE_KEY = "sh.maki/guidance"
local METHOD = "session/request_permission"
-- The host slot answers two ways and a browser dialog has two buttons, so the
-- option list holds exactly these two, in this order (WIRE 10). The slot cannot
-- say "remember this", so nothing else belongs on the list.
local OPTIONS = {
  { optionId = ALLOW, name = "Allow once", kind = "allow_once" },
  { optionId = REJECT, name = "Reject once", kind = "reject_once" },
}
-- The withdrawal of WIRE 10.2. Read-only: one table goes to every connection.
local CANCELLED = { outcome = { outcome = "cancelled" } }

--- The tab on screen, or "" when the host answers nothing.
local function focused_session()
  local called, id = pcall(maki.session.current)
  if not called then
    return ""
  end
  return protocol.str(id)
end

--- Whose call it is. The ctx of a tool names its own session, which is not always
--- the tab on screen: a subagent's call belongs to its parent tab.
local function session_of(ctx)
  if ctx ~= nil then
    local called, id = pcall(function()
      return ctx:session_id()
    end)
    if called and id ~= nil then
      return protocol.str(id)
    end
  end
  return focused_session()
end

--- The line the terminal shows, built the way `maki acp` builds it: the tool name
--- and the scopes it asked about (WIRE 10).
local function scope_line(req)
  local tool = protocol.str(req and req.tool, "tool")
  local scopes = {}
  local given = req and req.scopes
  if type(given) == "table" then
    for _, scope in ipairs(given) do
      scopes[#scopes + 1] = protocol.str(scope)
    end
  end
  if #scopes == 0 then
    return tool
  end
  return tool .. ": " .. table.concat(scopes, ", ")
end

local function ask_params(req, ctx)
  local call = {
    toolCallId = protocol.str(req and req.tool_id),
    title = scope_line(req),
    name = protocol.str(req and req.tool),
  }
  local input = req and req.input
  if input ~= nil and input ~= maki.NIL then
    call.rawInput = input
  end
  return { sessionId = session_of(ctx), toolCall = call, options = OPTIONS }
end

--- Free text the gateway sends with a deny. ACP has no field for it, so it rides
--- in the `_meta` of the result (WIRE 10.1), and a deny without it is a deny with
--- no words, which is all the terminal prompt gives.
local function guidance_of(result)
  local meta = result._meta
  if type(meta) ~= "table" then
    return nil
  end
  local text = meta[GUIDANCE_KEY]
  return type(text) == "string" and text or nil
end

--- The answer read as the two truths the slot can give. An option this plugin did
--- not offer, a result it cannot parse, an error instead of a result, and a frame
--- with neither are all a deny, which mirrors what `maki-acp` does with anything
--- it cannot read (WIRE 10.1).
local function verdict(answer)
  local result = type(answer.result) == "table" and answer.result or nil
  local outcome = result ~= nil and type(result.outcome) == "table" and result.outcome or nil
  if answer.error ~= nil or outcome == nil or outcome.outcome ~= SELECTED then
    return { decision = "deny" }
  end
  if outcome.optionId == ALLOW then
    return { decision = "allow" }
  end
  if outcome.optionId == REJECT then
    local guidance = guidance_of(result)
    if guidance == nil or guidance == "" then
      return { decision = "deny" }
    end
    return { decision = "deny", guidance = guidance }
  end
  return { decision = "deny" }
end

--- Withdraw one ask on every connection of it that had not answered, so their
--- dialogs close. A gateway reads this as a withdrawn ask and not as an error
--- (WIRE 10.2, 10.3).
local function withdraw(record, except)
  local frame = protocol.ok(record.id, CANCELLED)
  for _, entry in ipairs(record.live) do
    if entry ~= except then
      conns.send(entry, frame)
    end
  end
end

--- The layer for the `permission.prompt` slot, registered once at load.
function M.wrapper(prev, req, ctx)
  local record = conns.ask(METHOD, ask_params(req, ctx))
  if record == nil then
    -- No connection finished the handshake, so nothing would read this ask, and a
    -- frame nobody reads only parks the tool.
    return prev(req, ctx)
  end
  local waited = 0
  while record.answer == nil and waited < PERMISSION_WAIT_MS and #record.live > 0 do
    local called, why = pcall(maki.async.sleep, POLL_MS)
    if not called then
      maki.log.debug("gateway: permission wait stopped (" .. tostring(why) .. ")")
      break
    end
    waited = waited + POLL_MS
  end
  local answer = record.answer
  conns.settle(record)
  if answer == nil then
    -- Nobody answered. The ask is ended here first, so the terminal prompt is not
    -- shadowed by a dialog that still looks answerable, and then the call goes to
    -- that prompt (WIRE 10.2).
    withdraw(record, nil)
    return prev(req, ctx)
  end
  withdraw(record, answer.from)
  return verdict(answer)
end

return M
