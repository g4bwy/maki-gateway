-- Gateway control socket for a running maki TUI.
--
-- An external gateway connects to a unix socket and drives the tabs of this
-- process over newline-delimited JSON: requests in, one answer each, plus async
-- events for every connection that sent `hello`. The wire is
-- maki-gateway/docs/CONTRACTS.md section 1, and this plugin is the maki side of
-- it. Nothing here talks to a browser or to the model.
--
-- Install: add `maki.pack.add({ "https://host/maki-gateway" })` to
-- ~/.config/maki/init.lua and restart maki. The pack manifest is the plugin.toml
-- at the repository root, and plugin/gateway.lua is the entry point that runs
-- this module. README.md covers it, including the MAKI_GATEWAY_SOCK override and
-- how to try the socket with socat.

local conns = require("gateway.conns")
local ops = require("gateway.ops")
local permission = require("gateway.permission")
local protocol = require("gateway.protocol")
local transport = require("gateway.transport")

local M = {}

local bound_path

-- A tool call that started and has not finished, by session id. The question
-- event uses it to tell a question tool from any other wait for input.
local outstanding = {}

--- Which session an event is about. Host payloads say session_id, some do not.
local function session_of(data)
  local id = protocol.str(data and data.session_id)
  if id == "" then
    id = protocol.str(maki.session.current())
  end
  return id
end

-- What each host event adds, in the field names CONTRACTS section 1 fixes.
-- A builder answers the event body; the session id is filled in by the caller.
-- Tool rows also feed `outstanding`, which is how the question event knows a
-- question tool is still open.
local EVENTS = {
  {
    host = "SessionStatusChanged",
    wire = "status",
    build = function(d)
      return {
        status = protocol.str(d.status),
        title = protocol.str(d.title),
        focused = d.focused == true,
      }
    end,
  },
  {
    host = "TurnStart",
    wire = "turn_start",
    build = function(d)
      return { text = protocol.str(d.text) }
    end,
  },
  {
    host = "TurnEnd",
    wire = "turn_end",
    build = function(d)
      return {
        reason = protocol.str(d.reason),
        usage = protocol.object(d.usage),
        cost = tonumber(d.cost) or 0,
        context_size = tonumber(d.context_size) or 0,
        num_turns = tonumber(d.num_turns) or 0,
      }
    end,
  },
  {
    host = "ToolStart",
    wire = "tool_start",
    track = "open",
    build = function(d)
      return { tool_id = protocol.str(d.tool_id), tool = protocol.str(d.tool) }
    end,
  },
  {
    host = "ToolDone",
    wire = "tool_done",
    track = "close",
    build = function(d)
      local out = {
        tool_id = protocol.str(d.tool_id),
        tool = protocol.str(d.tool),
        is_error = d.is_error == true,
        bytes = tonumber(d.bytes) or 0,
      }
      -- Absent on a call that never ran, like a cancelled one.
      if not protocol.is_null(d.duration_ms) then
        out.duration_ms = tonumber(d.duration_ms)
      end
      return out
    end,
  },
  {
    host = "SessionTitleChanged",
    wire = "title",
    build = function(d)
      return { title = protocol.str(d.title) }
    end,
  },
  {
    host = "ModelChanged",
    wire = "model",
    build = function(d)
      return { model = protocol.model(d.model) }
    end,
  },
  -- A task is not a tab, so a subagent starting, finishing or being stopped
  -- moves no other event. One wire event covers both host events, and it names
  -- no task: the gateway reads task rows with the `tasks` op, so the browser
  -- that hears this refetches them.
  {
    host = "TaskStatusChanged",
    wire = "tasks_changed",
    build = function()
      return {}
    end,
  },
  {
    host = "TaskStopped",
    wire = "tasks_changed",
    build = function()
      return {}
    end,
  },
  -- The terminal user changed followed task with the chat cycling keys. The host
  -- payload carries no session, so `session` is the tab on screen, and `id` is
  -- the row of the `tasks` op now focused ("main" for the main chat).
  {
    host = "TaskFocusChanged",
    wire = "task_focus",
    build = function(d)
      return { id = protocol.str(d.id) }
    end,
  },
}

--- A host event as one fan-out call. Handlers only queue a line, so they never
--- yield and cannot hold up the dispatch that fired them.
local function event_handler(spec)
  return function(ev)
    local data = ev.data or {}
    local session = session_of(data)
    local body = spec.build(data)
    body.session = session
    if spec.track then
      local calls = outstanding[session]
      if not calls then
        calls = {}
        outstanding[session] = calls
      end
      local id = protocol.str(data.tool_id, "0")
      if spec.track == "open" then
        calls[id] = protocol.str(data.tool)
      else
        calls[id] = nil
      end
    end
    protocol.emit(spec.wire, body)
  end
end

--- A tab that waits for a person. An open question tool names itself as the
--- cause, and any other wait is reported as a plain input wait.
local function question_handler(ev)
  local data = ev.data or {}
  if data.status ~= "needs_input" then
    return
  end
  local session = session_of(data)
  local why = "input"
  for _, tool in pairs(outstanding[session] or {}) do
    if tool == "question" then
      why = "question"
    end
  end
  protocol.emit("question", { session = session, reason = why })
end

local function start()
  maki.api.set_slot("permission.prompt", function(prev, req, ctx)
    return permission.wrapper(prev, req, ctx)
  end)
  for _, spec in ipairs(EVENTS) do
    maki.api.create_autocmd(spec.host, { callback = event_handler(spec) })
  end
  maki.api.create_autocmd("SessionStatusChanged", { callback = question_handler })
  -- A closed tab drops its open tool calls, so the table stays one row per tab.
  maki.api.create_autocmd("SessionEnd", {
    callback = function(ev)
      outstanding[session_of((ev or {}).data)] = nil
    end,
  })
  conns.handlers = ops.handlers
  conns.start_flusher()
  -- Bound in a task: the bind waits on chmod, and code at load time may not
  -- park. UI round trips do not answer while the plugin is still loading.
  maki.async.spawn(function()
    local listener, err, path = transport.bind()
    if not listener then
      maki.log.error("gateway: cannot bind the control socket: " .. tostring(err))
      return
    end
    bound_path = path
    maki.log.info("gateway: control socket at " .. tostring(path))
    conns.accept_loop(listener)
  end)
end

--- Where the socket is, or nil before the bind finished.
function M.path()
  return bound_path
end

start()

return M
