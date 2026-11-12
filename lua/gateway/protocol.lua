-- The JSON-RPC 2.0 codec of docs/WIRE.md 1 over newline-delimited JSON.
--
-- maki.json encodes a plain empty table as an object, so a list that can come
-- out empty carries the tag a decoded `[]` holds, and three answers on this wire
-- are empty objects that must stay objects.
--
-- An answer echoes the id of its request as it arrived (WIRE 1.4), so an id is
-- the decoded value and is never reformatted on the way back. A number and a
-- string both name a request.
--
-- One frame per line, and 8 MiB is the ceiling: an oversize frame costs the
-- whole connection (WIRE 1.2), so the size is checked before the write.

local M = {}

M.MAX_LINE_BYTES = 8 * 1024 * 1024
M.JSONRPC = "2.0"

-- ACP v1 is the protocol version on this wire. The attach build is a maki fact,
-- so it travels in `_meta` (WIRE 2.1, 2.2).
M.PROTOCOL_VERSION = 1
M.ATTACH_PROTO = 2
-- The marker of this plugin build. Maki answers no Lua call for its own
-- version, so `sh.maki/maki_version` comes from the host binary (host.lua).
M.PLUGIN_VERSION = "2.0.0"

-- The codes of WIRE 13.
M.E_HELD = -32000
M.E_NOT_LIVE = -32002
M.E_BAD_REQUEST = -32600
M.E_NO_METHOD = -32601
M.E_BAD_PARAMS = -32602
M.E_INTERNAL = -32603

-- The words WIRE 13.1 keeps verbatim: the gateway maps a message by substring,
-- and the fixed busy sentence is the one `maki acp` writes too.
M.NOT_LIVE = "session not live"
M.BUSY = "session is busy: a prompt is already running"
M.INIT_FIRST = "initialize first"
M.NO_METHOD = "method not found"
M.TEXT_REQUIRED = "text is required"

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

--- True for the two id values this wire allows (WIRE 1.4).
function M.is_id(id)
  return type(id) == "number" or type(id) == "string"
end

--- Text for an id, which a log line names. A whole number keeps the spelling it
--- arrived with rather than the `.0` a float would print.
function M.id_text(id)
  if type(id) == "number" then
    return string.format("%.0f", id)
  end
  return M.str(id)
end

--- The correlation key of an id. A number keeps its text spelling, so 5 and 5.0
--- are one id, and a string never collides with the number it reads like.
function M.id_key(id)
  if type(id) == "number" then
    return "#" .. string.format("%.0f", id)
  end
  return M.str(id)
end

--- One request frame. {params} defaults to the empty object a method that takes
--- nothing is sent with (WIRE 1.3).
function M.call(id, method, params)
  return { jsonrpc = M.JSONRPC, id = id, method = method, params = params or M.object() }
end

--- One notification frame: a method with no id, so it answers nothing.
function M.note(method, params)
  return { jsonrpc = M.JSONRPC, method = method, params = params or M.object() }
end

--- One answer that carries a result and cannot carry an error. `"result":null`
--- is a value, and a handler that answers nothing at all gets an empty object.
function M.ok(id, result)
  return { jsonrpc = M.JSONRPC, id = id, result = result == nil and M.object() or result }
end

--- One answer that carries an error and cannot carry a result. {data} is what
--- the plugin has to add, and the gateway logs it unparsed (WIRE 13).
function M.fail(id, code, message, data)
  local err = { code = code, message = message }
  if not M.is_null(data) then
    err.data = data
  end
  return { jsonrpc = M.JSONRPC, id = id, error = err }
end

--- The error value a handler hands back instead of a message string.
function M.err(code, message, data)
  return { code = code, message = message, data = data }
end

--- The code of WIRE 13 that fits a message the host wrote. The words stay as
--- they are: 13.1 maps them by substring, and a rewording turns a clean refusal
--- into a 500.
function M.code_for(message)
  if message:find(M.NOT_LIVE, 1, true)
    or message:find("expired", 1, true)
    or message:find("unknown", 1, true)
    or message:find("no such", 1, true) then
    return M.E_NOT_LIVE
  end
  if message:find("another", 1, true) or message:find("elsewhere", 1, true) then
    return M.E_HELD
  end
  return M.E_INTERNAL
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

--- Parse one frame line. Returns a table, or nil plus a message. A JSON array is
--- not one object (WIRE 1.2), and neither is a scalar.
function M.decode(line)
  local called, value, err = pcall(maki.json.decode, line)
  if not called then
    return nil, tostring(value)
  end
  if type(value) ~= "table" then
    return nil, tostring(err or "expected a JSON object")
  end
  if M.kind(value) == nil then
    return nil, "frame is not one JSON object"
  end
  return value, nil
end

--- What one decoded frame is: "request", "note", "answer", "broken" for an id
--- that carries neither result nor error, or "junk" for a frame that does none
--- of those. nil says the value is not an object at all, which ends the
--- connection (WIRE 1.2).
function M.kind(frame)
  if type(frame) ~= "table" then
    return nil
  end
  local first = next(frame)
  if type(first) == "number" then
    return nil
  end
  local has_id = frame.id ~= nil and frame.id ~= maki.NIL
  if type(frame.method) == "string" and frame.method ~= "" then
    if has_id then
      return "request"
    end
    return "note"
  end
  if has_id then
    if frame.result ~= nil or frame.error ~= nil then
      return "answer"
    end
    return "broken"
  end
  return "junk"
end

return M
