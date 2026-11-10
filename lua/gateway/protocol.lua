-- JSON values that stay valid on one line, plus the outbound queue.
--
-- The wire is newline-delimited JSON, so every value written must hold one
-- line, and the 8 MiB cap in CONTRACTS section 1 must be checked before the
-- write, because an oversize line costs the whole connection.
--
-- maki.json.encode turns a plain empty table into an object. An empty list has
-- to stay a list on the wire, so tables that answer ops are decoded from JSON
-- first, which tags them as arrays.

local M = {}

M.MAX_LINE_BYTES = 8 * 1024 * 1024
-- The `plugin` field of a hello answer, per CONTRACTS section 1.
M.PLUGIN = "1"

local empty_array = maki.json.decode("[]")
local empty_object = maki.json.decode("{}")

--- A table that encodes to [] whatever it holds.
function M.array(items)
  if items == nil or #items == 0 then
    return empty_array
  end
  return items
end

--- A table that encodes to {} when it holds nothing.
function M.object(value)
  if value == nil then
    return empty_object
  end
  return value
end

--- Text for a JSON string field. Absent, null and non-text become {fallback}.
function M.str(value, fallback)
  if value == nil or value == maki.NIL then
    return fallback or ""
  end
  if type(value) == "table" then
    local ok, encoded = pcall(maki.json.encode, value)
    if ok and encoded then
      return encoded
    end
    return fallback or ""
  end
  return tostring(value)
end

--- Model text: a spec string, or the spec of the table maki.model.get returns.
function M.model(value)
  if type(value) == "table" then
    local spec = M.str(value.spec)
    return spec ~= "" and spec or M.str(value.id)
  end
  return M.str(value)
end

--- True for JSON null and for nothing.
function M.is_null(value)
  return value == nil or value == maki.NIL
end

--- One JSON line, without its break. Returns line, err.
function M.encode(value)
  local called, line, err = pcall(maki.json.encode, value)
  if not called then
    return nil, tostring(line)
  end
  if not line then
    return nil, tostring(err or "cannot encode")
  end
  if line:find("\n", 1, true) then
    return nil, "encoded JSON holds a line break"
  end
  return line, nil
end

--- Parse one request line. Returns a table, or nil plus a message.
function M.decode(line)
  local called, value, err = pcall(maki.json.decode, line)
  if not called then
    return nil, tostring(value)
  end
  if type(value) ~= "table" then
    return nil, tostring(err or "expected a JSON object")
  end
  return value, nil
end

-- Encoded event lines wait here, so an event handler never yields. The conn
-- writer drains the queue (conns.start_flusher).
local outbox = {}
local OUTBOX_LIMIT = 4096

--- Hand over the queued lines and start an empty queue.
function M.take_events()
  local batch = outbox
  outbox = {}
  return batch
end

--- Queue one async event for every subscribed connection.
-- Field names follow CONTRACTS section 1. Never yields.
function M.emit(name, fields)
  if #outbox >= OUTBOX_LIMIT then
    maki.log.warn("gateway: event queue is full, dropping " .. name)
    return false
  end
  local event = { event = name }
  for key, value in pairs(fields or {}) do
    if value ~= nil then
      event[key] = value
    end
  end
  local line, err = M.encode(event)
  if not line then
    maki.log.warn("gateway: cannot encode event " .. name .. " (" .. err .. ")")
    return false
  end
  outbox[#outbox + 1] = line
  return true
end

return M
