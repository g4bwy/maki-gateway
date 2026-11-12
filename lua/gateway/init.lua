-- Gateway control socket for a running maki TUI.
--
-- An external gateway connects to a unix socket and drives the tabs of this
-- process over newline-delimited JSON-RPC 2.0: requests in, one answer each, plus
-- the ACP update stream and the maki events this plugin adds on top of it. The
-- wire is docs/WIRE.md, the v2 attach protocol, and this plugin is the agent side
-- of that document. Nothing here talks to a browser or to the model.
--
-- Install: add `maki.pack.add({ "https://host/maki-gateway" })` to
-- ~/.config/maki/init.lua and restart maki. The pack manifest is the plugin.toml
-- at the repository root, and plugin/gateway.lua is the entry point that runs
-- this module. README.md covers it, including the MAKI_GATEWAY_SOCK override and
-- how to try the socket with socat.

local conns = require("gateway.conns")
local host = require("gateway.host")
local ops = require("gateway.ops")
local permission = require("gateway.permission")
local protocol = require("gateway.protocol")
local transport = require("gateway.transport")
local turns = require("gateway.turns")

local M = {}

local bound_path
local STATUS_WORKING = "working"
local STATUS_IDLE = "idle"
local STATUS_NEEDS_INPUT = "needs_input"
local QUESTION_TOOL = "question"
-- The one currency `maki acp` reports a turn's cost in, so both engines fill the
-- ACP field the same way (WIRE 11.1).
local CURRENCY = "USD"

-- Tool calls that started and have not finished, by tab. The question event reads
-- them to tell a question tool from any other wait for a person.
local outstanding = {}

--- Which tab an event is about. The host names it on every event but
--- `FileIndexReady` and `ModeChanged`, and WIRE 11.2 requires `sessionId` on every
--- kind, so an event that names no tab is left alone rather than guessed at:
--- `maki.session.current()` answers across a UI round trip, and a host event
--- handler must not park.
local function session_of(data)
  local session = protocol.str((data or {}).session_id)
  if session == "" then
    maki.log.debug("gateway: a host event named no tab, so nothing was pushed")
  end
  return session
end

-- ------------------------------------------------------------------ the ACP stream

--- A tool call started, so one card opens. The host reports a running call, and a
--- card ends at a terminal status (WIRE 11.1 rule 1), which `tool_done` sends.
local function tool_start(ev)
  local data = (ev or {}).data or {}
  local session = session_of(data)
  if session == "" then
    return
  end
  local tool = protocol.str(data.tool)
  local call = protocol.str(data.tool_id, "0")
  local open = outstanding[session]
  if open == nil then
    open = {}
    outstanding[session] = open
  end
  open[call] = tool
  conns.update(session, {
    sessionUpdate = "tool_call",
    toolCallId = call,
    name = tool,
    title = tool,
    status = "in_progress",
  })
end

--- A tool call ended. The host reports a status, a size in bytes, and a duration,
--- and it reports no text, so a live card carries no output and no content: the
--- result of a call reaches a browser through `_sh.maki/messages` after a refetch
--- (WIRE 11.1 rule 2).
local function tool_done(ev)
  local data = (ev or {}).data or {}
  local session = session_of(data)
  if session == "" then
    return
  end
  local call = protocol.str(data.tool_id, "0")
  local open = outstanding[session]
  if open ~= nil then
    open[call] = nil
  end
  local meta = { ["sh.maki/bytes"] = tonumber(data.bytes) or 0 }
  local ms = tonumber(data.duration_ms)
  if ms ~= nil then
    meta["sh.maki/duration_ms"] = ms
  end
  local tool = protocol.str(data.tool)
  conns.update(session, {
    sessionUpdate = "tool_call_update",
    toolCallId = call,
    -- WIRE 11.1 puts the tool name of a finished call in `title`, and the reducer
    -- reads the name from there, so `name` stays as the start frame set it.
    title = tool,
    status = data.is_error == true and "failed" or "completed",
    _meta = meta,
  })
end

--- The context gauge and the running bill of a finished turn, in ACP's own shape
--- (WIRE 11.2). A model with no published price reports no cost.
local function usage_update(data)
  local update = {
    sessionUpdate = "usage_update",
    used = tonumber(data.context_size) or 0,
    size = tonumber(data.context_window) or 0,
  }
  local cost = tonumber(data.cost)
  if cost ~= nil then
    update.cost = { amount = cost, currency = CURRENCY }
  end
  return update
end

--- One status change, which is how a turn the person at the terminal started
--- reaches the gateway: the tab moving is the event (WIRE 11.2). `title` and
--- `focused` ride along when the host reported them, and stay away when it did
--- not, because a browser patches the fields it is given.
local function status_event(session, status, title, focused)
  if session == "" then
    return
  end
  conns.event(session, "status", { status = status, title = title, focused = focused })
end

-- ------------------------------------------------------------------ host callbacks

--- A turn began. The tab is working, and a prompt this plugin queued for it is
--- now the turn someone is waiting on.
local function on_turn_start(ev)
  local data = (ev or {}).data or {}
  local session = session_of(data)
  turns.turn_start(session, protocol.is_null(data.text) and nil or protocol.str(data.text))
  status_event(session, STATUS_WORKING)
end

--- A turn ended: the numbers of its run, the tab at rest, and the answer to the
--- prompt that started it. Nothing here parks, so the queued frames above land
--- before the waiting handler of `session/prompt` writes its answer.
local function on_turn_end(ev)
  local data = (ev or {}).data or {}
  local session = session_of(data)
  conns.update(session, usage_update(data))
  status_event(session, STATUS_IDLE)
  -- Last, so the two frames above are already queued when the answer to the prompt
  -- of this turn is queued behind them.
  turns.turn_end(session, protocol.str(data.reason))
end

--- The turn died on an error. The prompt that waited on it is answered, and the
--- tab is at rest. The host put its own words on the screen and in the log.
local function on_turn_error(ev)
  local data = (ev or {}).data or {}
  local session = session_of(data)
  turns.turn_error(session)
  status_event(session, STATUS_IDLE)
end

--- A tab that waits for a person. An open question tool names itself as the cause.
--- The `question` kind of WIRE 11.2 carries nothing else, and the answer belongs
--- to the terminal.
local function on_status_changed(ev)
  local data = (ev or {}).data or {}
  local session = session_of(data)
  if session == "" then
    return
  end
  local status = protocol.str(data.status, STATUS_IDLE)
  status_event(session, status, protocol.str(data.title), data.focused == true)
  if status ~= STATUS_NEEDS_INPUT then
    return
  end
  for _, tool in pairs(outstanding[session] or {}) do
    if tool == QUESTION_TOOL then
      conns.event(session, "question")
      return
    end
  end
end

local function on_title_changed(ev)
  local data = (ev or {}).data or {}
  local session = session_of(data)
  if session ~= "" then
    conns.event(session, "title", { title = protocol.str(data.title) })
  end
end

local function on_model_changed(ev)
  local data = (ev or {}).data or {}
  local session = session_of(data)
  if session ~= "" then
    conns.event(session, "model", { model = protocol.model(data.model) })
  end
end

--- A task started, moved, or stopped. One kind covers all of them and names no
--- task: the gateway reads the rows with `_sh.maki/tasks`, so a browser that hears
--- this refetches them (WIRE 11.2). The tab it names is the one the task belongs
--- to, which is not always the tab on screen.
local function on_tasks_changed(ev)
  local session = session_of((ev or {}).data)
  if session ~= "" then
    conns.event(session, "tasks_changed")
  end
end

--- The person at the terminal cycled to another chat of this tab. The name of that
--- chat is `taskId` inside the params: v1 carried it in the frame's `id`, which is
--- the request number on a response, and WIRE 11.2 ended that collision.
local function on_task_focus(ev)
  local data = (ev or {}).data or {}
  local session = session_of(data)
  if session ~= "" then
    conns.event(session, "task_focus", { taskId = protocol.str(data.id) })
  end
end

--- A tab closed. Its prompt can never end, its open tool calls are gone, and its
--- mode is no longer worth watching.
local function on_session_end(ev)
  local session = session_of((ev or {}).data)
  if session == "" then
    return
  end
  turns.session_end(session)
  outstanding[session] = nil
  ops.forget(session)
end

-- What each host event pushes out. Every builder here only queues lines and reads
-- event data, so it never parks and cannot hold up the dispatch that fired it.
local EVENTS = {
  { host = "TurnStart", at = on_turn_start },
  { host = "TurnEnd", at = on_turn_end },
  { host = "TurnError", at = on_turn_error },
  { host = "ToolStart", at = tool_start },
  { host = "ToolDone", at = tool_done },
  { host = "SessionStatusChanged", at = on_status_changed },
  { host = "SessionTitleChanged", at = on_title_changed },
  { host = "ModelChanged", at = on_model_changed },
  { host = "TaskStatusChanged", at = on_tasks_changed },
  { host = "TaskStopped", at = on_tasks_changed },
  { host = "TaskFocusChanged", at = on_task_focus },
  { host = "SessionEnd", at = on_session_end },
  { host = "SessionReset", at = on_session_end },
}

--- One connection went away: it can hold no prompt answer any more. Its parked
--- asks drop themselves inside conns.lua, which holds the list of them.
local function on_close(entry)
  turns.drop_conn(entry)
end

local function start()
  maki.api.set_slot("permission.prompt", function(prev, req, ctx)
    return permission.wrapper(prev, req, ctx)
  end)
  for _, spec in ipairs(EVENTS) do
    maki.api.create_autocmd(spec.host, { callback = spec.at })
  end
  conns.handlers = ops.handlers
  conns.on_close = on_close
  conns.start_flusher()
  -- Bound in a task: the bind waits on two chmod calls, the version probe waits
  -- on a child process, and code that runs while the plugin loads cannot park:
  -- the UI answers no round trip until the load is done.
  maki.async.spawn(function()
    host.probe_version()
    local listener, err, path = transport.bind()
    if not listener then
      maki.log.error("gateway: cannot bind the control socket: " .. tostring(err))
      return
    end
    bound_path = path
    maki.log.info(
      "gateway: control socket at " .. tostring(path)
        .. " (plugin " .. protocol.PLUGIN_VERSION .. ", attach " .. protocol.ATTACH_PROTO .. ")"
    )
    conns.accept_loop(listener)
  end)
end

--- Where the socket is, or nil before the bind finished.
function M.path()
  return bound_path
end

start()

return M
