-- Where the control socket lives, and how it is bound.
--
-- Paths follow docs/CONTRACTS.md section 1: $MAKI_GATEWAY_SOCK wins, else
-- <state dir>/maki/gateway/pid-<pid>.sock. The name carries the process id,
-- so a socket left by a dead maki never collides with a live one and a crash
-- cannot leak a name that blocks the next start. The host removes a socket it
-- bound, and only hands a path over to the same plugin, so /reload is covered
-- while a crashed maki is not: bind once, and when the name is taken remove it
-- and bind again.

local M = {}

local STATE_ROOT = { "maki", "gateway" }
local SOCK_PREFIX = "pid-"
local READABLE_FILE = 65536

-- The host leaves a bound socket to the umask. The gateway reaches it over this
-- user's own files, so owner-only rights match what the contract promises.
local MODE_SOCK = "600"
local MODE_DIR = "700"

local pid

local function readable_file(path)
  local meta = maki.fs.metadata(path)
  return meta ~= nil and not meta.is_dir and (meta.size or 0) < READABLE_FILE
end

--- Process id of the running maki, as a string. Cached.
-- /proc/self is the maki process itself, so a read of it names maki's pid in
-- field 1, and no child process is involved. Where /proc is missing (macOS)
-- a direct child reads its own parent, which is maki.
function M.pid()
  if pid then
    return pid
  end
  local stat = maki.fs.read("/proc/self/stat")
  if stat then
    pid = stat:match("^(%d+)")
    if pid then
      return pid
    end
  end
  local job, start_err = maki.fn.jobstart("readlink /proc/self/ppid || echo $PPID", { tail = 2 })
  if not job then
    maki.log.warn("gateway: cannot find the process id (" .. tostring(start_err) .. ")")
    return nil
  end
  local result, wait_err = maki.fn.jobwait(job, 5000)
  maki.fn.jobforget(job)
  if not result then
    maki.log.warn("gateway: cannot read the process id (" .. tostring(wait_err) .. ")")
    return nil
  end
  pid = tostring(result.stdout or ""):match("(%d+)")
  if not pid then
    maki.log.warn("gateway: the process id is not in the helper output")
  end
  return pid
end

local function chmod(path, mode)
  local job, err = maki.fn.jobstart({ "chmod", mode, path })
  if not job then
    maki.log.warn("gateway: chmod " .. mode .. " failed to start (" .. tostring(err) .. ")")
    return
  end
  local result = maki.fn.jobwait(job, 5000)
  maki.fn.jobforget(job)
  if not result or result.exit_code ~= 0 then
    maki.log.warn("gateway: chmod " .. mode .. " on " .. path .. " did not apply")
  end
end

--- Absolute socket path, with the directory created. Returns path, dir, err.
function M.path()
  local explicit = maki.uv.os_getenv("MAKI_GATEWAY_SOCK")
  if explicit and explicit ~= "" then
    if explicit:sub(1, 1) ~= "/" and explicit:sub(1, 1) ~= "~" then
      return nil, nil, "MAKI_GATEWAY_SOCK must be an absolute path: " .. explicit
    end
    local full = maki.fs.abspath(explicit)
    local dir = maki.fs.dirname(full)
    if not maki.fs.mkdir(dir, { parents = true }) then
      return nil, nil, "cannot create " .. tostring(dir)
    end
    return full, dir, nil
  end

  local root = maki.uv.os_getenv("XDG_STATE_HOME")
  if not root or root == "" then
    root = maki.uv.os_homedir()
    if not root then
      return nil, nil, "no home directory to put the socket in"
    end
    root = maki.fs.joinpath(root, ".local", "state")
  end
  local state = maki.fs.joinpath(root, STATE_ROOT[1], STATE_ROOT[2])
  if not maki.fs.mkdir(state, { parents = true }) then
    return nil, nil, "cannot create " .. state
  end
  local id = M.pid()
  if not id then
    return nil, nil, "cannot name the socket without a process id"
  end
  return maki.fs.joinpath(state, SOCK_PREFIX .. id .. ".sock"), state, nil
end

--- Bind the control socket. Returns a Listener, or nil plus an error.
-- Yields: it waits on the two chmod calls it makes.
function M.bind()
  local path, dir, err = M.path()
  if not path then
    return nil, err
  end
  local listener, bind_err = maki.net.listen_unix(path)
  if not listener then
    if not readable_file(path) then
      return nil, tostring(bind_err)
    end
    maki.log.warn("gateway: removing the stale socket " .. path)
    if not maki.fs.rm(path, { force = true }) then
      return nil, tostring(bind_err)
    end
    listener, bind_err = maki.net.listen_unix(path)
    if not listener then
      return nil, tostring(bind_err)
    end
  end
  chmod(path, MODE_SOCK)
  if dir and maki.fs.basename(dir) == STATE_ROOT[2] then
    chmod(dir, MODE_DIR)
  end
  return listener, nil, path
end

return M
