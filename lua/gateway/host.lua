-- What this TUI is, and the shared shape of a host call.
--
-- Session calls need a TUI attached. `call` keeps the error the host wrote, so
-- no handler reports a failure the plugin invented, and `ui_only` turns a missing
-- UI into the words WIRE 13.1 fixes.
--
-- Maki answers no Lua call for its own version, so `sh.maki/maki_version` of
-- WIRE 2.1 comes from one probe of the host binary, in the same shape the
-- gateway's own health route uses. Until the probe lands the key reads `unknown`,
-- which is a fact about this plugin rather than a number that might be wrong.

local protocol = require("gateway.protocol")
local transport = require("gateway.transport")

local M = {}

-- The words the host writes when there is no TUI. They reach a person who reads
-- the log, so they stay exactly as the host reports them (WIRE 13.1).
local NO_UI = "no interactive UI attached"
local VERSION_UNKNOWN = "unknown"
-- `maki --version` answers "maki 0.7.1". The running binary is named, not the one
-- that happens to be first on `$PATH`, and the shell is only there for `readlink`.
local VERSION_PATTERN = "(%d+%.%d+%.%d+[%w%.%-%+]*)"
local VERSION_PROBE_MS = 5000
-- The two modes the host uses, named the way WIRE 8.1 names them.
local MODE_BUILD = "build"
local MODE_PLAN = "plan"

--- The shell command that prints the version of this very process's binary.
local function version_command()
  local pid = transport.pid()
  if pid == nil then
    return "command -v maki >/dev/null 2>&1 && maki --version"
  end
  return string.format(
    'p=$(readlink /proc/%s/exe 2>/dev/null || command -v maki); "$p" --version', pid
  )
end

--- Every host call goes through here: the host answers (value, err) or raises.
function M.call(fn, ...)
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

function M.no_ui(err)
  return tostring(err):find(NO_UI, 1, true) ~= nil
end

--- The refusal for a call that needs a terminal this maki does not have. Answers
--- one value, so a caller can write `return nil, M.ui_only(err)` and keep it.
function M.ui_only(err)
  if M.no_ui(err) then
    return protocol.err(protocol.E_INTERNAL, NO_UI)
  end
  return protocol.str(err, "the host refused the call")
end

--- The live tabs of this TUI, as the host reports them: id, title, status,
--- updated_at, focused.
function M.live()
  local live, err = M.call(maki.session.live)
  if not live then
    return nil, M.ui_only(err)
  end
  return live
end

--- What the host knows of one tab beyond the live row: cwd, model, mode. Answers
--- an empty table when the host says nothing, so a row is still complete.
function M.details(id)
  local snap = M.call(maki.session.read, { session = id })
  return type(snap) == "table" and snap or {}
end

--- The tab the terminal shows, or "" when the host answers nothing.
function M.focused()
  return protocol.str(M.call(maki.session.current))
end

--- One ISO 8601 UTC stamp from Unix seconds, the only timestamp format WIRE 3
--- allows. Nothing known gives nil, and the row then omits `updatedAt`.
function M.iso(epoch)
  local stamp = tonumber(epoch)
  if not stamp or stamp <= 0 then
    return nil
  end
  return os.date("!%Y-%m-%dT%H:%M:%SZ", math.floor(stamp))
end

--- Ask the host binary what version it is. Yields on the child process, so call
--- it from a task, once, at start.
function M.probe_version()
  local job, start_err = maki.fn.jobstart({ "sh", "-c", version_command() }, { tail = 2 })
  if not job then
    maki.log.warn("gateway: cannot ask the host its version (" .. tostring(start_err) .. ")")
    return
  end
  local result, wait_err = maki.fn.jobwait(job, VERSION_PROBE_MS)
  maki.fn.jobforget(job)
  if not result then
    maki.log.warn("gateway: no version from the host (" .. tostring(wait_err) .. ")")
    return
  end
  probed = protocol.str(result.stdout):match(VERSION_PATTERN)
  if probed == nil then
    maki.log.debug("gateway: the host reported no version, _meta says unknown")
    return
  end
  maki.log.info("gateway: host version " .. probed)
end

--- The version string for `_meta`. Never yields, so any handler can read it.
function M.version()
  return probed or VERSION_UNKNOWN
end

--- The `sh.maki/` keys of the `initialize` answer (WIRE 2.1), plus
--- `sh.maki/methods`, the extension names this build answers, which lets a
--- reader check a capability instead of trying a call.
function M.identity(methods)
  local cwd, cwd_err = maki.uv.cwd()
  if not cwd then
    maki.log.warn("gateway: cannot read the working directory (" .. tostring(cwd_err) .. ")")
  end
  local meta = {
    ["sh.maki/pid"] = tonumber(transport.pid() or "") or 0,
    ["sh.maki/cwd"] = protocol.str(cwd),
    ["sh.maki/attach_proto"] = protocol.ATTACH_PROTO,
    ["sh.maki/maki_version"] = M.version(),
  }
  if methods ~= nil then
    meta["sh.maki/methods"] = protocol.array(methods)
  end
  return meta
end

--- The two modes the host uses, in the ACP shape of WIRE 4.
function M.modes(current)
  return {
    currentModeId = current == MODE_PLAN and MODE_PLAN or MODE_BUILD,
    availableModes = { { id = MODE_BUILD, name = "Build" }, { id = MODE_PLAN, name = "Plan" } },
  }
end

--- The one config option this protocol reports, the model of the tab, or nil when
--- the host reports no model. WIRE 4 says a plugin with nothing to report omits
--- `configOptions`.
function M.config_option(snap)
  local spec = protocol.model(snap and snap.model)
  if spec == "" then
    return nil
  end
  return {
    {
      id = "model",
      name = "Model",
      category = "model",
      type = "select",
      currentValue = spec,
      options = { { value = spec, name = spec } },
    },
  }
end

return M
