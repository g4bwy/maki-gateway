-- One handler per op in CONTRACTS section 1, plus the shared host wrappers.
--
-- A handler returns `value, nil` for `{"id":N,"ok":value}` and
-- `nil, message` for `{"id":N,"error":message}`. It may park, since it runs
-- inside the connection's own task, and it never answers twice: the caller in
-- conns.lua wraps it and sends the one reply.
--
-- Session calls only work with a TUI attached. `host` keeps the error the host
-- gave, and `ui_only` turns the missing UI into the message the contract fixes,
-- so an op never reports a failure the plugin invented.

local protocol = require("gateway.protocol")
local transport = require("gateway.transport")
local permission = require("gateway.permission")

local M = {}

local NO_UI = "no interactive UI attached"
local NOT_LIVE = "session not live"
local DEFAULT_LAST = 200
local RELOAD_PREFIX = "/reload"
local MODES = { build = true, plan = true }
local DECISIONS = { allow = true, deny = true }

local H = {}

--- Every session call goes through here: the host gives (value, err) or raises.
local function host(fn, ...)
  local args = { ... }
  local called, value, err = pcall(function()
    return fn(table.unpack(args))
  end)
  if not called then
    return nil, tostring(value)
  end
  if value == nil then
    return nil, protocol.str(err, "the host refused the call")
  end
  return value
end

local function no_ui(err)
  return tostring(err):find(NO_UI, 1, true) ~= nil
end

--- Whether this maki has a TUI. Probed once and kept.
local attached
local function ui_attached()
  if attached == nil then
    local _, err = host(maki.session.live)
    attached = not no_ui(err)
  end
  return attached
end

--- An error that names the missing UI rather than a raw host message.
local function ui_only(err)
  if no_ui(err) then
    return nil, NO_UI
  end
  return nil, tostring(err)
end

-- ---------------------------------------------------------------- list

--- cwd and model are not on `session.live()`; `session.read` is metadata only,
--- so one call per tab fills them without needing a content permission.
local function fill(id)
  local snap = host(maki.session.read, { session = id })
  return {
    cwd = snap and protocol.str(snap.cwd) or "",
    model = snap and protocol.model(snap.model) or "",
  }
end

function H.list(_req)
  local live, err = host(maki.session.live)
  if not live then
    return ui_only(err)
  end
  local focused_id = protocol.str(maki.session.current())
  local out = {}
  for _, s in ipairs(live) do
    local id = protocol.str(s.id)
    local focused = s.focused
    if focused == nil then
      focused = id == focused_id
    end
    local extra = fill(id)
    out[#out + 1] = {
      id = id,
      title = protocol.str(s.title),
      status = protocol.str(s.status),
      cwd = extra.cwd,
      model = extra.model,
      focused = focused and true or false,
      updated_at = tonumber(s.updated_at) or 0,
    }
  end
  return protocol.array(out)
end

-- ------------------------------------------------------------- messages

--- `session` and `last` are always passed: a live session then answers from
--- memory on its cheap path, and a stored one is read off disk.
function H.messages(req)
  local id = protocol.str(req.session)
  if id == "" then
    id = protocol.str(maki.session.current())
    if id == "" then
      return nil, "session is required"
    end
  end
  local last = DEFAULT_LAST
  if type(req.last) == "number" and req.last > 0 then
    last = math.floor(req.last)
  end
  local messages, err = host(maki.session.messages, { session = id, last = last })
  if not messages then
    return ui_only(err)
  end
  return { messages = protocol.array(messages) }
end

-- --------------------------------------------------------------- prompt

function H.prompt(req)
  local text = protocol.str(req.text)
  if text == "" then
    return nil, "text is required"
  end
  local id = protocol.str(req.session)
  if id == "" then
    return nil, "session is required"
  end
  local state, err = host(maki.session.prompt, text, { session = id })
  if not state then
    if no_ui(err) then
      return nil, NO_UI
    end
    if tostring(err):find(NOT_LIVE, 1, true) then
      return nil, NOT_LIVE .. ": " .. id
    end
    return nil, tostring(err)
  end
  return state
end

-- ------------------------------------------------------------------ new

--- `cwd` cannot be honoured: a new tab inherits the focused tab's project, and
--- the only cwd a document carries is text the model is told, which would
--- mislead it. Refusing is better than opening in the wrong directory.
function H.new(req)
  local want_cwd = protocol.str(req.cwd)
  if want_cwd ~= "" then
    local snap = host(maki.session.read)
    local here = snap and protocol.str(snap.cwd) or ""
    if here ~= "" and here ~= want_cwd then
      return nil, "cannot open a session in another directory: " .. want_cwd
    end
  end
  -- A tab with no prompt can only open in the foreground, and this op is
  -- defined as a background one, so a missing prompt is refused.
  local text = protocol.str(req.prompt)
  if text == "" then
    return nil, "prompt is required to open a background session"
  end
  local id, err = host(maki.session.new, { prompt = text })
  if not id then
    return ui_only(err)
  end
  return { session = protocol.str(id) }
end

-- ------------------------------------------------------------- set_mode

function H.set_mode(req)
  local mode = protocol.str(req.mode)
  if not MODES[mode] then
    return nil, 'mode must be "build" or "plan"'
  end
  local id = protocol.str(req.session)
  local opts = id ~= "" and { session = id } or {}
  local done, err = host(maki.session.set_mode, mode, opts)
  if not done then
    return ui_only(err)
  end
  return true
end

-- ------------------------------------------------------------ set_title

--- A null or empty title clears it, and the session falls back to its
--- generated title, like a rename to nothing in the TUI.
function H.set_title(req)
  local id = protocol.str(req.session)
  if id == "" then
    return nil, "session is required"
  end
  local title = protocol.is_null(req.title) and "" or protocol.str(req.title)
  local done, err = host(maki.session.set_title, { id = id, title = title })
  if not done then
    return ui_only(err)
  end
  return true
end

-- --------------------------------------------------------------- notify

function H.notify(req)
  local text = protocol.str(req.text)
  if text == "" then
    return nil, "text is required"
  end
  local id = protocol.str(req.session)
  local opts = id ~= "" and { session = id } or {}
  local done, err = host(maki.session.notify, text, opts)
  if not done then
    return ui_only(err)
  end
  return true
end

-- ----------------------------------------------------------------- plan

function H.plan(req)
  local id = protocol.str(req.session)
  local opts = id ~= "" and { session = id } or {}
  local plan, err = host(maki.plan.read, opts)
  if not plan then
    return ui_only(err)
  end
  local out = {
    mode = protocol.str(plan.mode),
    path = protocol.str(plan.path),
    ready = plan.ready and true or false,
  }
  if not protocol.is_null(plan.content) then
    out.content = protocol.str(plan.content)
  end
  return out
end

-- ---------------------------------------------------------------- tasks

function H.tasks(_req)
  local tasks, err = host(maki.task.list)
  if not tasks then
    return ui_only(err)
  end
  return protocol.array(tasks)
end

-- ----------------------------------------------------------- run_command

--- /reload rebuilds the plugin host, which would take this plugin, its
--- listener and its connections with it. Nothing on this socket may ask for it.
local function refused_reload(cmdline)
  local head = cmdline:match("^/%s*(%w+)") or cmdline:match("^%s*(%w+)")
  return head ~= nil and head:lower() == "reload"
end

function H.run_command(req)
  local cmdline = protocol.str(req.cmdline)
  if cmdline == "" then
    return nil, "cmdline is required"
  end
  if cmdline:sub(1, #RELOAD_PREFIX) == RELOAD_PREFIX or refused_reload(cmdline) then
    return nil, "cmdline not allowed: /reload destroys the plugin host"
  end
  local done, err = host(maki.api.run_command, cmdline)
  if not done then
    return nil, tostring(err)
  end
  return true
end

-- ------------------------------------------------------------ pop_queue

--- The host pops the queue of the tab on screen and has no call for another
--- one, so a session that is not focused is refused rather than ignored.
function H.pop_queue(req)
  local id = protocol.str(req.session)
  if id ~= "" then
    local current = protocol.str(maki.session.current())
    if current ~= "" and current ~= id then
      return nil, "only the focused queue can be popped: " .. id
    end
  end
  if not ui_attached() then
    return nil, NO_UI
  end
  local _, action_err = host(maki.ui.action, "pop_queue")
  if action_err then
    return nil, tostring(action_err)
  end
  return true
end

-- ----------------------------------------------------------- permissions

function H.permissions(req)
  return permission.set_enabled(req.on ~= nil and req.on ~= false)
end

function H.respond(req)
  local decision = protocol.str(req.decision)
  if not DECISIONS[decision] then
    return nil, 'decision must be "allow" or "deny"'
  end
  local guidance = protocol.is_null(req.guidance) and nil or protocol.str(req.guidance)
  local done, err = permission.respond(req.request_id, decision, guidance)
  if not done then
    return nil, tostring(err)
  end
  return true
end

-- ------------------------------------------------------- hello and ping

function H.hello(_req, entry)
  entry.subscribed = true
  local live = host(maki.session.live)
  local cwd, cwd_err = maki.uv.cwd()
  if not cwd then
    maki.log.warn("gateway: cannot read the working directory (" .. tostring(cwd_err) .. ")")
  end
  return {
    pid = tonumber(transport.pid() or "") or 0,
    plugin = protocol.PLUGIN,
    cwd = protocol.str(cwd),
    sessions = live and #live or 0,
  }
end

function H.ping(_req)
  return "pong"
end

--- The op table conns.lua routes to. Every handler takes (req, entry) and
--- answers (value, nil) or (nil, message): the success value is never nil, so
--- the missing first value means an error to conns.handle_line.
M.handlers = H

return M
