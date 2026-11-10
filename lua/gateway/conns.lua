-- Open connections, request framing, and the fan-out writer.
--
-- Three host facts shape this file. `Conn:read` returns up to 64 KiB with no
-- line framing and (nil, nil) at the end of the stream, so a reader owns a
-- buffer and cuts lines out of it itself. Only one read per connection may be
-- in flight, so one task per connection owns it from accept to close. A
-- garbage collected handle closes its connection, so every handle and its
-- reader live in the module-level `conns` table for as long as they are open.
--
-- Writes go the other way. One queue and one writer, so an event never blocks
-- the code that raised it and the one place that writes events is the place
-- that decides a peer is gone. `Conn:write` waits its turn, so lines stay
-- whole and in call order.

local protocol = require("gateway.protocol")

local M = {}

local IDLE_SLEEP_MS = 20
local RETRY_SLEEP_MS = 200
local LINE_OVER = "line over 8 MiB"

--- conn handle -> entry { conn, subscribed, closed }. Module level on purpose.
local conns = {}

local function close(entry)
  if entry.closed then
    return
  end
  entry.closed = true
  pcall(function()
    entry.conn:close()
  end)
  conns[entry.conn] = nil
end

M.close = close

--- Write one finished line. A failed write closes the connection.
local function deliver(entry, line)
  if entry.closed then
    return false
  end
  local called, ok, err = pcall(function()
    return entry.conn:write(line .. "\n")
  end)
  if not called then
    maki.log.warn("gateway: write failed (" .. tostring(ok) .. ")")
    close(entry)
    return false
  end
  if not ok then
    maki.log.warn("gateway: write failed (" .. tostring(err) .. ")")
    close(entry)
    return false
  end
  return true
end

local function id_text(id)
  if type(id) == "number" then
    return string.format("%.0f", id)
  end
  return "0"
end

--- One response line. A result that cannot be encoded, or that would not fit
--- one line, becomes an error with the same id: a conn that gets no answer at
--- all is worse than an answer that names what went wrong, and an oversize
--- line would cost the connection.
local function send(entry, payload)
  local line, err = protocol.encode(payload)
  if line and #line > protocol.MAX_LINE_BYTES then
    line = nil
    err = LINE_OVER .. ", send less"
  end
  if not line then
    line = '{"id":' .. id_text(payload.id) .. ',"error":"reply rejected: '
      .. tostring(err):gsub("[%c\"\\]", "") .. '"}'
  end
  return deliver(entry, line)
end

function M.ok(entry, id, value)
  return send(entry, { id = id, ok = value })
end

function M.err(entry, id, message)
  return send(entry, { id = id, error = message })
end

function M.count()
  local n = 0
  for _ in pairs(conns) do
    n = n + 1
  end
  return n
end

--- Drain the event queue to every subscribed connection. One long-lived task.
function M.start_flusher()
  maki.async.spawn(function()
    while true do
      local batch = protocol.take_events()
      if #batch == 0 then
        maki.async.sleep(IDLE_SLEEP_MS)
      else
        for _, line in ipairs(batch) do
          for _, entry in pairs(conns) do
            if entry.subscribed then
              deliver(entry, line)
            end
          end
        end
      end
    end
  end)
end

--- Read, frame, and answer one connection until it ends.
function M.serve(entry)
  maki.async.spawn(function()
    local buffer = ""
    while not entry.closed do
      local called, chunk, err = pcall(function()
        return entry.conn:read()
      end)
      if not called then
        maki.log.debug("gateway: read failed (" .. tostring(chunk) .. ")")
        break
      end
      if chunk == nil then
        if err then
          maki.log.debug("gateway: read ended (" .. tostring(err) .. ")")
        end
        break
      end
      if #buffer + #chunk > protocol.MAX_LINE_BYTES then
        M.err(entry, 0, LINE_OVER)
        break
      end
      buffer = buffer .. chunk
      local cut = buffer:find("\n", 1, true)
      while cut do
        local line = buffer:sub(1, cut - 1)
        buffer = buffer:sub(cut + 1)
        if line ~= "" then
          M.handle_line(entry, line)
        end
        cut = buffer:find("\n", 1, true)
      end
    end
    close(entry)
  end)
end

--- One request line. Never raises: an unwrapped error would drop a live conn.
function M.handle_line(entry, line)
  if #line > protocol.MAX_LINE_BYTES then
    M.err(entry, 0, LINE_OVER)
    return
  end
  local req, err = protocol.decode(line)
  if not req then
    M.err(entry, 0, "bad json: " .. tostring(err))
    return
  end
  local id = req.id
  if type(id) ~= "number" then
    id = 0
  end
  local name = protocol.str(req.op)
  local handlers = M.handlers
  local handler = handlers and handlers[name]
  if not handler then
    M.err(entry, id, "unknown op: " .. name)
    return
  end
  -- Handlers answer (value, nil) or (nil, message), so the first value decides
  -- the outcome. A raised error is the plugin's own fault and says so.
  local called, value, err = pcall(handler, req, entry)
  if not called then
    M.err(entry, id, "plugin error: " .. tostring(value))
  elseif value == nil then
    M.err(entry, id, tostring(err or "failed"))
  else
    M.ok(entry, id, value)
  end
end

--- Track a fresh connection and start its reader. Returns the entry.
function M.add(conn)
  local entry = { conn = conn, subscribed = false, closed = false }
  conns[conn] = entry
  M.serve(entry)
  return entry
end

--- Accept until the listener closes. Keeps to one parked accept at a time.
function M.accept_loop(listener)
  maki.async.spawn(function()
    while true do
      local called, conn, err = pcall(function()
        return listener:accept()
      end)
      if not called then
        maki.log.warn("gateway: accept failed (" .. tostring(conn) .. ")")
        maki.async.sleep(RETRY_SLEEP_MS)
      elseif conn then
        M.add(conn)
      else
        if err then
          maki.log.warn("gateway: accept failed (" .. tostring(err) .. ")")
        else
          maki.log.info("gateway: listener closed")
        end
        return
      end
    end
  end)
end

return M
