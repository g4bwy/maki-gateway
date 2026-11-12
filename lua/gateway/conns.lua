-- Connections, the handshake gate, the frame router, and the one writer.
--
-- Three host facts shape the reader. `Conn:read` answers up to 64 KiB with no
-- line framing and (nil, nil) at the end of the stream, so a reader owns a
-- buffer and cuts frames out of it itself. Only one read per connection can be
-- in flight, so one task per connection owns it from accept to close. And a
-- garbage collected handle closes its connection, so every handle and its reader
-- live in the module-level `conns` while they are open.
--
-- A request handler runs in a task of its own, because `session/prompt` answers
-- at the end of a turn (WIRE 7.1) while the reader keeps taking the
-- `session/list` poll that is the liveness clock (WIRE 1.5).
--
-- Writes go the other way. A handler that answers a request, or replays history
-- to its own connection, writes here and can park. A host event handler must not
-- park, so it only hands a line to the queue, and the one flusher writes it.
-- `Conn:write` waits its turn, so a line leaves whole and in call order.

local protocol = require("gateway.protocol")

local M = {}

local IDLE_SLEEP_MS = 20
local RETRY_SLEEP_MS = 200
local OUTBOX_LIMIT = 4096
-- A plugin numbers its own requests from 1000 upward, so the two directions
-- cannot collide (WIRE 1.4).
local ASK_ID_FIRST = 1000
local FRAME_OVER = "frame over 8 MiB"
local TOO_BIG = "the answer does not fit one frame"

--- conn handle -> entry { conn, initialized, closed }. Module level on purpose:
--- a collected handle closes its socket.
local conns = {}
local outbox = {}
-- The answers this plugin waits for, keyed by the id text of WIRE 1.4.
local asks = {}
local ask_id = ASK_ID_FIRST - 1

-- ------------------------------------------------------------------ delivery

local function close(entry)
  if entry.closed then
    return
  end
  entry.closed = true
  pcall(function()
    entry.conn:close()
  end)
  conns[entry.conn] = nil
  -- A closed connection can answer no ask and hold no prompt answer.
  for _, record in pairs(asks) do
    for i, held in ipairs(record.live) do
      if held == entry then
        table.remove(record.live, i)
        break
      end
    end
  end
  if M.on_close ~= nil then
    M.on_close(entry)
  end
end

M.close = close

--- Write one encoded line. Yields: `Conn:write` waits for its turn on the socket.
--- A failed write ends the connection, because the peer would read the next line
--- as the rest of the cut one.
local function deliver(entry, line)
  if entry.closed or line == nil then
    return false, "connection closed"
  end
  local called, ok, err = pcall(function()
    return entry.conn:write(line .. "\n")
  end)
  if not called then
    ok, err = nil, tostring(ok)
  end
  if not ok then
    maki.log.debug("gateway: write failed (" .. tostring(err or ok) .. ")")
    close(entry)
    return false, protocol.str(err, "write failed")
  end
  return true
end

--- Write one frame to one connection. A frame over the ceiling is never written:
--- it would cost the whole connection (WIRE 1.2).
function M.send(entry, frame)
  local line, err = protocol.encode(frame)
  if not line then
    return false, err
  end
  if #line + 1 > protocol.MAX_LINE_BYTES then
    return false, FRAME_OVER
  end
  return deliver(entry, line)
end

--- Hand one encoded line to the fan-out queue. Never yields, so a host event
--- handler can call it.
local function queue(line)
  if #outbox >= OUTBOX_LIMIT then
    maki.log.warn("gateway: the write queue is full, dropping a frame")
    return false
  end
  outbox[#outbox + 1] = line
  return true
end

--- Queue one frame, encoded here once whatever number of connections read it.
local function enqueue(frame)
  local line, err = protocol.encode(frame)
  if not line then
    maki.log.warn("gateway: cannot encode a frame (" .. err .. ")")
    return false
  end
  if #line + 1 > protocol.MAX_LINE_BYTES then
    maki.log.warn("gateway: " .. FRAME_OVER .. ", dropped")
    return false
  end
  return queue(line)
end

--- One `session/update` notification of WIRE 11.1, queued for every initialized
--- connection. Never yields, so a host event handler calls it. A tab that is not
--- named is not pushed: `sessionId` is required on both notification kinds.
function M.update(session, update)
  if session == nil or session == "" then
    return false
  end
  return enqueue(protocol.note("session/update", { sessionId = session, update = update }))
end

--- One `session/update` written to one connection at once, in the order the
--- caller asked for. The history replay of WIRE 4 needs this rather than the
--- queue, because its answer must be the frame after the last update.
function M.update_now(entry, session, update)
  return M.send(entry, protocol.note("session/update", { sessionId = session, update = update }))
end


--- One `_sh.maki/session_event` notification: the change ACP cannot carry, of
--- WIRE 11.2. {fields} holds the keys of that kind, and a nil stays away.
function M.event(session, kind, fields)
  if session == nil or session == "" then
    return false
  end
  local params = { kind = kind, sessionId = session }
  for key, value in pairs(fields or {}) do
    if value ~= nil then
      params[key] = value
    end
  end
  return enqueue(protocol.note("_sh.maki/session_event", params))
end

--- One answer that carries a result and no error. An answer that would not fit
--- one frame becomes an error with the same id: a caller left waiting is worse
--- than an answer that names what went wrong.
---
--- The queue is written out first. WIRE 1.2 puts frames on the socket in call
--- order, and WIRE 7.1 makes an answer that overtakes the frames of its own turn
--- a lie about the turn being over.
function M.reply(entry, id, value)
  M.drain()
  local line, err = protocol.encode(protocol.ok(id, value))
  if line ~= nil and #line + 1 > protocol.MAX_LINE_BYTES then
    line, err = nil, TOO_BIG
  end
  if line == nil then
    maki.log.warn("gateway: " .. tostring(err))
    local why = tostring(err or "cannot encode the answer")
    line = protocol.encode(protocol.fail(id, protocol.E_INTERNAL, why))
  end
  return deliver(entry, line)
end

--- One answer that carries an error object and no result (WIRE 1.3).
function M.reject(entry, id, code, message, data)
  M.drain()
  return M.send(entry, protocol.fail(id, code, message, data))
end

-- ---------------------------------------------------------------- requests out

--- Send one request to every initialized connection under one id, and answer the
--- record a caller parks on. nil says no connection finished the handshake, so
--- nothing would read the frame (WIRE 10.4).
function M.ask(method, params)
  local record = { id = ask_id + 1, method = method, live = {}, answered = {} }
  ask_id = record.id
  local n = 0
  for _, entry in pairs(conns) do
    if M.is_ready(entry) then
      n = n + 1
      record.live[n] = entry
    end
  end
  if n == 0 then
    return nil
  end
  asks[protocol.id_key(record.id)] = record
  local frame = protocol.call(record.id, method, params)
  -- Backwards, because a write that fails closes that connection and takes its
  -- slot out of the list this loop is reading.
  for i = #record.live, 1, -1 do
    local entry = record.live[i]
    if not entry.closed then
      M.send(entry, frame)
    end
  end
  return record
end

--- Stop waiting for one request. A later answer for its id is then dropped,
--- which is what WIRE 1.4 says of an id nobody holds.
function M.settle(record)
  if record ~= nil then
    asks[protocol.id_key(record.id)] = nil
  end
end

--- The answer of one request, or nil. {record.answer} carries the raw `result`
--- and `error` of the frame and the connection it came from, and the first one
--- to land owns the answer (WIRE 10.3).
local function take_answer(entry, frame)
  local record = asks[protocol.id_key(frame.id)]
  if record == nil then
    local key = protocol.id_key(frame.id)
    maki.log.debug("gateway: answer for an id nothing waits on (" .. key .. ")")
    return
  end
  if record.answer ~= nil or record.answered[entry] then
    maki.log.debug("gateway: second answer for one request, dropped")
    return
  end
  record.answered[entry] = true
  record.answer = { result = frame.result, error = frame.error, from = entry }
end

-- ----------------------------------------------------------------- dispatching

--- The methods this build answers, set by init.lua from ops.lua.
M.handlers = nil

--- Whether one connection finished the handshake of WIRE 2.1.
function M.is_ready(entry)
  return entry ~= nil and entry.initialized == true and not entry.closed
end

--- Answer one request in a task of its own, and answer it exactly once.
local function run_handler(entry, id, method, params)
  maki.async.spawn(function()
    local ok, value, failure = pcall(M.handlers[method], params, entry)
    if not ok then
      failure = protocol.err(protocol.E_INTERNAL, "plugin error: " .. tostring(value))
      value = nil
    end
    if entry.closed then
      return
    end
    if value ~= nil then
      M.reply(entry, id, value)
      return
    end
    if type(failure) ~= "table" or type(failure.code) ~= "number" then
      local message = protocol.str(failure, "failed")
      failure = protocol.err(protocol.code_for(message), message)
    end
    M.reject(entry, id, failure.code, failure.message, failure.data)
  end)
end

--- One decoded frame. Never raises: an unwrapped error would drop a live
--- connection mid-conversation.
function M.handle_line(entry, frame)
  local kind = protocol.kind(frame)
  if kind == "answer" then
    take_answer(entry, frame)
    return
  end
  if kind == "broken" then
    -- A frame with an id that carries neither result nor error answers nothing,
    -- and nothing after it can be trusted (WIRE 1.3, 13.2).
    maki.log.warn("gateway: a frame answered nothing, ending the connection")
    close(entry)
    return
  end
  if kind == "note" then
    -- Unknown notifications are ignored, and that is what lets a newer plugin
    -- push a new kind to an older gateway (WIRE 12.1).
    maki.log.debug("gateway: ignored notification " .. tostring(frame.method))
    return
  end
  if kind ~= "request" then
    maki.log.debug("gateway: ignored a frame with no method and no id")
    return
  end
  if not protocol.is_id(frame.id) then
    maki.log.warn("gateway: request " .. tostring(frame.method) .. " names no usable id")
    return
  end
  if frame.params ~= nil and type(frame.params) ~= "table" then
    M.reject(entry, frame.id, protocol.E_BAD_REQUEST, "params is not one JSON object")
    return
  end
  local name = tostring(frame.method)
  if not entry.initialized and name ~= "initialize" then
    -- Before the handshake is answered the plugin answers every other request
    -- this way and pushes no notification (WIRE 2.1).
    M.reject(entry, frame.id, protocol.E_BAD_REQUEST, protocol.INIT_FIRST)
    return
  end
  if M.handlers == nil or M.handlers[name] == nil then
    -- A request is never left unanswered: a missing answer parks the caller
    -- (WIRE 12.2).
    M.reject(entry, frame.id, protocol.E_NO_METHOD, protocol.NO_METHOD)
    return
  end
  run_handler(entry, frame.id, name, frame.params or protocol.object())
end

--- One buffered line. Answers false when the connection must end.
local function take_frame(entry, line)
  if #line + 1 > protocol.MAX_LINE_BYTES then
    maki.log.warn("gateway: " .. FRAME_OVER .. ", ending the connection")
    return false
  end
  local frame, why = protocol.decode(line)
  if not frame then
    maki.log.warn("gateway: " .. tostring(why) .. ", ending the connection")
    return false
  end
  M.handle_line(entry, frame)
  return true
end

--- Read, frame, and route one connection until it ends.
function M.serve(entry)
  maki.async.spawn(function()
    local buffer = ""
    local open = true
    while open and not entry.closed do
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
      -- The ceiling is read off the bytes before anything is parsed (WIRE 1.2).
      if #buffer + #chunk > protocol.MAX_LINE_BYTES then
        maki.log.warn("gateway: " .. FRAME_OVER .. ", ending the connection")
        break
      end
      buffer = buffer .. chunk
      local cut = buffer:find("\n", 1, true)
      while cut do
        local line = buffer:sub(1, cut - 1)
        buffer = buffer:sub(cut + 1)
        if line ~= "" and not take_frame(entry, line) then
          open = false
          break
        end
        cut = buffer:find("\n", 1, true)
      end
    end
    close(entry)
  end)
end

--- Track a fresh connection and start its reader. Returns the entry.
function M.add(conn)
  local entry = { conn = conn, initialized = false, closed = false }
  conns[conn] = entry
  M.serve(entry)
  return entry
end

--- Write everything the queue holds, in order, and answer how many lines went
--- out. The flusher calls this, so one place owns the fan-out rule: every
--- connection that finished the handshake hears every line.
function M.drain()
  local batch = outbox
  outbox = {}
  for _, line in ipairs(batch) do
    for _, entry in pairs(conns) do
      if entry.initialized and not entry.closed then
        deliver(entry, line)
      end
    end
  end
  return #batch
end

--- Drain the fan-out queue until it is empty. One long-lived task.
function M.start_flusher()
  maki.async.spawn(function()
    while true do
      if M.drain() == 0 then
        maki.async.sleep(IDLE_SLEEP_MS)
      end
    end
  end)
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
