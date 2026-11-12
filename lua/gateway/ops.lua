-- One handler per method of docs/WIRE.md, plus the shared host wrappers.
--
-- A handler answers `value, nil` for a result and `nil, err` for an error, where
-- {err} is `protocol.err(code, message)` or a plain message whose code then
-- follows WIRE 13 from its own words. The caller in conns.lua turns one of those
-- into exactly one answer, so a handler never writes to the wire itself and never
-- answers twice. A handler runs in a task of its own and can park, because the answer to
-- a prompt waits for its turn.

local protocol = require("gateway.protocol")
local conns = require("gateway.conns")
local host = require("gateway.host")
local turns = require("gateway.turns")

local M = {}
local H = {}

local STATUS_WORKING = "working"
local MODE_BUILD = "build"
local MODE_PLAN = "plan"
local KIND_TURN = "turn"
local DEFAULT_LAST = 200
local RELOAD_WORD = "reload"
local RELOAD_REFUSAL = "cmdline not allowed: /reload destroys the plugin host"
local MODE_REFUSAL = 'mode must be "build" or "plan"'
-- The longest text one `session/update` can carry. A frame is escaped JSON and an
-- escape is at most six bytes per byte of text, so a piece this size cannot reach
-- the ceiling of WIRE 1.2.
local TEXT_PIECE_BYTES = math.floor(protocol.MAX_LINE_BYTES / 8)
-- The extension methods this build answers, advertised under `sh.maki/methods`
-- because ACP gives a client no other way to find them (WIRE 2.1).
M.EXTENSIONS = {
  "_sh.maki/messages",
  "_sh.maki/queue",
  "_sh.maki/pop_queue",
  "_sh.maki/notify",
  "_sh.maki/plan",
  "_sh.maki/tasks",
  "_sh.maki/task_focus",
  "_sh.maki/run_command",
}

-- ------------------------------------------------------------------ host shapes

--- The refusal for a required argument the request did not carry.
local function missing(name)
  return protocol.err(protocol.E_BAD_REQUEST, name .. " is required")
end

--- The refusal WIRE 13 gives a `sessionId` that is not a tab of this TUI.
local function not_live(id)
  return protocol.err(protocol.E_NOT_LIVE, protocol.NOT_LIVE .. ": " .. id)
end

--- The live row of one tab, or the WIRE 13 refusal for it. Every method that
--- names a tab starts here, so an unknown id, a session only on disk, and a tab
--- that closed all answer the same words.
local function live_row(id)
  if id == "" then
    return nil, missing("sessionId")
  end
  local live, err = host.live()
  if not live then
    return nil, err
  end
  for _, row in ipairs(live) do
    if protocol.str(row.id) == id then
      return row
    end
  end
  return nil, not_live(id)
end

--- The host's own words, with the code WIRE 13 gives them. Nothing is reworded:
--- 13.1 maps a message by substring, and a reworded refusal turns into a 500.
local function passthrough(err)
  local message = protocol.str(err, "the host refused the call")
  return nil, protocol.err(protocol.code_for(message), message)
end

-- ---------------------------------------------------------------- the mode watch

-- session id -> mode, as the plugin last reported it.
local modes = {}

--- Announce one mode now (WIRE 8.1).
local function push_mode(id, mode)
  modes[id] = mode
  return conns.update(id, { sessionUpdate = "current_mode_update", currentModeId = mode })
end

--- The mode also moves at the terminal, where no request on this socket is
--- involved, and no host event reports it. The list poll of WIRE 3 is the only
--- clock that sees it, so a move since the last list is announced there. The
--- first sight of a tab only records what it is.
local function watch_mode(id, mode)
  if modes[id] ~= nil and modes[id] ~= mode then
    push_mode(id, mode)
    return
  end
  modes[id] = mode
end

--- Stop watching a tab that closed.
function M.forget(id)
  modes[id] = nil
end

-- ------------------------------------------------------------------- initialize

--- One `initialize` per connection, and the same answer again for a connection
--- that repeats it: the handshake registers the build and the identities, and a
--- second call changes nothing (WIRE 2.1).
H.initialize = function(params, entry)
  local want = params.protocolVersion
  if want ~= nil and type(want) ~= "number" then
    return nil, protocol.err(protocol.E_BAD_REQUEST, "protocolVersion is not a number")
  end
  if want ~= nil and want ~= protocol.PROTOCOL_VERSION then
    local why = "cannot answer protocolVersion " .. protocol.id_text(want)
    return nil, protocol.err(protocol.E_BAD_REQUEST, why)
  end
  entry.initialized = true
  return {
    protocolVersion = protocol.PROTOCOL_VERSION,
    agentCapabilities = {
      loadSession = true,
      promptCapabilities = protocol.object(),
      sessionCapabilities = { list = protocol.object() },
    },
    _meta = host.identity(M.EXTENSIONS),
  }
end

-- ------------------------------------------------------------------ session/list

--- The tabs of this TUI, as `SessionInfo` rows of WIRE 3. `cwd` and the
--- `sh.maki/cwd` of `_meta` are one string read once, so they cannot differ (3.1).
--- A tab with no known clock leaves `updatedAt` out rather than send a zero.
H["session/list"] = function()
  local live, err = host.live()
  if not live then
    return nil, err
  end
  local focused_id = host.focused()
  local rows = {}
  for _, tab in ipairs(live) do
    local id = protocol.str(tab.id)
    local extra = host.details(id)
    local cwd = protocol.str(extra.cwd)
    local mode = protocol.str(extra.mode, MODE_BUILD)
    watch_mode(id, mode)
    rows[#rows + 1] = {
      sessionId = id,
      cwd = cwd,
      title = protocol.str(tab.title),
      updatedAt = host.iso(tab.updated_at),
      _meta = {
        ["sh.maki/status"] = protocol.str(tab.status, "idle"),
        ["sh.maki/model"] = protocol.model(extra.model),
        ["sh.maki/cwd"] = cwd,
        ["sh.maki/mode"] = mode,
        ["sh.maki/focused"] = tab.focused == true or id == focused_id,
      },
    }
  end
  return { sessions = protocol.array(rows) }
end

-- ----------------------------------------------------------------- session/load

--- The text blocks of one user message, joined the way a prompt is joined.
local function message_text(content)
  local parts = {}
  for _, block in ipairs(content) do
    if protocol.str(block.type) == "text" then
      parts[#parts + 1] = protocol.str(block.text)
    end
  end
  return table.concat(parts, "\n")
end

-- The first byte of a UTF-8 sequence, so a piece never ends in half a character.
local UTF8_CONTINUATION = 128
local UTF8_LEAD = 192
--- The largest cut at or before {want} that leaves every UTF-8 sequence whole.
--- A trailing byte belongs to a sequence that started behind it, so the walk ends
--- at that sequence's lead byte, which says how wide the sequence is.
local function utf8_cut(text, want)
  local stop = want
  local byte = text:byte(stop) or 0
  while stop > 1 and byte >= UTF8_CONTINUATION and byte < UTF8_LEAD do
    stop = stop - 1
    byte = text:byte(stop) or 0
  end
  if byte < UTF8_CONTINUATION then
    return want
  end
  local width = byte >= 240 and 4 or (byte >= 224 and 3 or 2)
  if stop + width - 1 <= want then
    return want
  end
  if stop <= 1 then
    return 1
  end
  return stop - 1
end

--- One text block as the chunks WIRE 4 sends in order, split so every frame stays
--- under the ceiling. A reader joins chunks that carry the same kind, so a long
--- block arriving in pieces still reads as one message.
local function text_chunks(kind, text)
  local out = {}
  local n = 0
  while #text > TEXT_PIECE_BYTES do
    local stop = utf8_cut(text, TEXT_PIECE_BYTES)
    n = n + 1
    out[n] = { sessionUpdate = kind, content = { type = "text", text = text:sub(1, stop) } }
    text = text:sub(stop + 1)
  end
  n = n + 1
  out[n] = { sessionUpdate = kind, content = { type = "text", text = text } }
  return out
end

local function push_text(push, kind, text)
  if text == "" then
    return
  end
  for _, update in ipairs(text_chunks(kind, text)) do
    push(update)
  end
end

--- What one stored message becomes in the replay of WIRE 4: what the person sent
--- is a `user_message_chunk`, model text and thinking are the two agent chunks, a
--- tool call opens a card and its result closes it under the same id.
local function replay_message(msg, push)
  local kind = protocol.str(msg.kind, KIND_TURN)
  if kind ~= KIND_TURN then
    -- A report the host made to the model and a context update are not what a
    -- person sent. WIRE 6 carries them to a browser verbatim, and WIRE 4 has no
    -- update for them.
    return
  end
  local content = type(msg.content) == "table" and msg.content or {}
  if protocol.str(msg.role) == "user" then
    local shown = protocol.is_null(msg.display_text) and nil or protocol.str(msg.display_text)
    if shown ~= nil and shown ~= "" then
      push_text(push, "user_message_chunk", shown)
      return
    end
    push_text(push, "user_message_chunk", message_text(content))
    return
  end
  for _, block in ipairs(content) do
    local btype = protocol.str(block.type)
    if btype == "text" then
      push_text(push, "agent_message_chunk", protocol.str(block.text))
    elseif btype == "thinking" then
      push_text(push, "agent_thought_chunk", protocol.str(block.thinking))
    elseif btype == "tool_use" then
      push({
        sessionUpdate = "tool_call",
        toolCallId = protocol.str(block.id),
        name = protocol.str(block.name),
        title = protocol.str(block.name),
        status = "pending",
        rawInput = protocol.is_null(block.input) and nil or block.input,
      })
    elseif btype == "tool_result" then
      push({
        sessionUpdate = "tool_call_update",
        toolCallId = protocol.str(block.tool_use_id),
        status = block.is_error == true and "failed" or "completed",
        rawOutput = protocol.is_null(block.content) and nil or block.content,
      })
    end
  end
end

--- History, then the answer: the updates of WIRE 4 go to this connection and no
--- other, and the answer is the frame after the last one. The mode and config
--- state of the tab ride on that answer, which is what a client reads before it
--- prompts a tab it did not open.
H["session/load"] = function(params, entry)
  local id = protocol.str(params.sessionId)
  local tab, err = live_row(id)
  if not tab then
    return nil, err
  end
  local messages, merr = host.call(maki.session.messages, { session = id })
  if not messages then
    return passthrough(merr)
  end
  local dropped = 0
  local function push(update)
    if conns.update_now(entry, id, update) then
      return
    end
    -- A card whose payload will not fit one line still needs its status, or the
    -- reader waits on a tool that finished. The payload goes, the card closes, and
    -- `_sh.maki/messages` of WIRE 6 holds what was cut.
    if update.toolCallId ~= nil then
      local bare = {
        sessionUpdate = update.sessionUpdate,
        toolCallId = update.toolCallId,
        title = update.title,
        name = update.name,
        status = update.status,
      }
      if conns.update_now(entry, id, bare) then
        return
      end
    end
    dropped = dropped + 1
  end
  for _, msg in ipairs(messages) do
    replay_message(msg, push)
  end
  if dropped > 0 then
    maki.log.warn("gateway: dropped " .. dropped .. " replay frames that would not fit one line")
  end
  local extra = host.details(id)
  return {
    modes = host.modes(extra.mode),
    configOptions = host.config_option(extra),
  }
end

-- --------------------------------------------------------------- session/resume

--- A resume confirms the tab is live and reports its mode. It replays nothing:
--- that is the line between it and a load (WIRE 5). Every update of every tab
--- already reaches every connection that finished the handshake, so this opens no
--- stream that was closed.
H["session/resume"] = function(params)
  local id = protocol.str(params.sessionId)
  local tab, err = live_row(id)
  if not tab then
    return nil, err
  end
  local extra = host.details(id)
  return {
    modes = host.modes(extra.mode),
    configOptions = host.config_option(extra),
  }
end

-- ---------------------------------------------------------------- session/prompt

--- The text of a prompt, or the refusal of a block a live tab cannot take.
local function prompt_text(prompt)
  if prompt == nil then
    return nil, missing("prompt")
  end
  if type(prompt) ~= "table" then
    return nil, protocol.err(protocol.E_BAD_REQUEST, "prompt is not an array of content blocks")
  end
  local parts = {}
  for _, block in ipairs(prompt) do
    local btype = protocol.str(block.type)
    if btype ~= "text" then
      return nil, protocol.err(
        protocol.E_BAD_REQUEST,
        "a live tab takes text blocks only, not type " .. (btype == "" and "?" or btype)
      )
    end
    parts[#parts + 1] = protocol.str(block.text)
  end
  local text = table.concat(parts, "\n")
  if text == "" then
    return nil, protocol.err(protocol.E_BAD_PARAMS, protocol.TEXT_REQUIRED)
  end
  return text
end

--- Start a turn now, and answer when it ends.
---
--- Two rules meet here. WIRE 7.1 wants one prompt at a time, with a second
--- refused rather than held, while the host queues whatever it is given. So this
--- reads the status first and refuses a working tab without touching the queue:
--- text in front of a running turn is `_sh.maki/queue` of WIRE 7.2. The claim in
--- turns.lua is taken before the host call, so the end of this turn cannot arrive
--- unwatched, and that claim is the only wake-up: no clock bounds a turn, because
--- a long turn is legal.
H["session/prompt"] = function(params, entry)
  local id = protocol.str(params.sessionId)
  local text, terr = prompt_text(params.prompt)
  if not text then
    return nil, terr
  end
  local tab, lerr = live_row(id)
  if not tab then
    return nil, lerr
  end
  if turns.parked(id) or tab.status == STATUS_WORKING then
    return nil, protocol.err(protocol.E_INTERNAL, protocol.BUSY)
  end
  local claim = turns.claim(id, text, entry)
  if claim == nil then
    return nil, protocol.err(protocol.E_INTERNAL, protocol.BUSY)
  end
  local state, perr = host.call(maki.session.prompt, text, { session = id })
  if not state then
    turns.release(claim)
    return passthrough(perr)
  end
  turns.arm(claim, state)
  local stop, werr = turns.wait(claim)
  if not stop then
    return nil, werr
  end
  return { stopReason = stop }
end

--- A live tab has no cancel over this socket, and that is a decision rather than
--- an omission (WIRE 7.3). The gateway reads this answer as its own 501.
H["session/cancel"] = function()
  return nil, protocol.err(protocol.E_NO_METHOD, protocol.NO_METHOD)
end

-- ----------------------------------------------------------------- tab settings

H["session/set_mode"] = function(params)
  local mode = protocol.str(params.modeId)
  if mode ~= MODE_BUILD and mode ~= MODE_PLAN then
    return nil, protocol.err(protocol.E_BAD_PARAMS, MODE_REFUSAL)
  end
  local id = protocol.str(params.sessionId)
  local tab, err = live_row(id)
  if not tab then
    return nil, err
  end
  local done, derr = host.call(maki.session.set_mode, mode, { session = id })
  if not done then
    return passthrough(derr)
  end
  push_mode(id, mode)
  return protocol.object()
end

--- An empty title clears the name, and the tab falls back to the title the host
--- generated (WIRE 8.2). The answer carries nothing: the host renames a tab on its
--- own too, so the title reaches the gateway as an event.
H["session/set_title"] = function(params)
  local id = protocol.str(params.sessionId)
  local tab, err = live_row(id)
  if not tab then
    return nil, err
  end
  if type(params.title) ~= "string" then
    return nil, missing("title")
  end
  local done, derr = host.call(maki.session.set_title, { id = id, title = params.title })
  if not done then
    return passthrough(derr)
  end
  return protocol.object()
end

-- ----------------------------------------------------------- native transcript

--- The native `Message` records of one live tab (WIRE 6), with no field renamed,
--- dropped, or added. A tab that is not live is refused rather than read off
--- disk: the gateway owns that path through the store engine.
H["_sh.maki/messages"] = function(params)
  local id = protocol.str(params.sessionId)
  local tab, err = live_row(id)
  if not tab then
    return nil, err
  end
  local last = DEFAULT_LAST
  if type(params.last) == "number" and params.last > 0 then
    last = math.floor(params.last)
  end
  local messages, merr = host.call(maki.session.messages, { session = id, last = last })
  if not messages then
    return passthrough(merr)
  end
  return { messages = protocol.array(messages) }
end

-- -------------------------------------------------------------------- text steering

--- Text with the host's own answer: the tab takes it now or holds it for the end
--- of the turn that is running. This is the method that holds text, which is what
--- `session/prompt` of WIRE 7.1 must not do.
H["_sh.maki/queue"] = function(params)
  local text = protocol.str(params.text)
  if text == "" then
    return nil, protocol.err(protocol.E_BAD_PARAMS, protocol.TEXT_REQUIRED)
  end
  local id = protocol.str(params.sessionId)
  if id == "" then
    return nil, missing("sessionId")
  end
  local state, err = host.call(maki.session.prompt, text, { session = id })
  if not state then
    return passthrough(err)
  end
  return { state = state }
end

--- The queue of the tab on screen, because that is the only queue the host can
--- reach from a plugin (WIRE 7.2). WIRE 12 dropped v1's session argument, so the
--- refusal v1 needed for another tab cannot happen here.
H["_sh.maki/pop_queue"] = function()
  local done, err = host.call(maki.ui.action, "pop_queue")
  if not done then
    return passthrough(err)
  end
  return true
end

-- --------------------------------------------------------- the other four calls

--- A report for the tab's next agent run. With no `sessionId` it goes to the tab
--- on screen (WIRE 9.1).
H["_sh.maki/notify"] = function(params)
  local text = protocol.str(params.text)
  if text == "" then
    return nil, protocol.err(protocol.E_BAD_PARAMS, protocol.TEXT_REQUIRED)
  end
  local id = protocol.str(params.sessionId)
  local opts = id ~= "" and { session = id } or {}
  local done, err = host.call(maki.session.notify, text, opts)
  if not done then
    return passthrough(err)
  end
  return true
end

--- The plan of one tab, in the v1 payload with no change (WIRE 9.2): `content`
--- only when the host returned one.
H["_sh.maki/plan"] = function(params)
  local id = protocol.str(params.sessionId)
  local opts = id ~= "" and { session = id } or {}
  local plan, err = host.call(maki.plan.read, opts)
  if not plan then
    return passthrough(err)
  end
  local out = {
    mode = protocol.str(plan.mode),
    path = protocol.str(plan.path),
    ready = plan.ready == true,
  }
  if not protocol.is_null(plan.content) then
    out.content = protocol.str(plan.content)
  end
  return out
end

--- The task rows, as the bare array `maki.task.list()` answers (WIRE 9.3). The
--- host lists the chats of the focused tab only and takes no session argument, and
--- an optional field stays away rather than arrive as null.
H["_sh.maki/tasks"] = function()
  local tasks, err = host.call(maki.task.list)
  if not tasks then
    return passthrough(err)
  end
  return protocol.array(tasks)
end

--- Show one chat of the tab the terminal already has focused (WIRE 9.4).
--- `taskId` is a row of `_sh.maki/tasks`, and `main` is the main chat. WIRE 12
--- says a renamed argument is not aliased, so v1's `id` fallback left with the
--- collision that made it necessary.
H["_sh.maki/task_focus"] = function(params)
  local task = params.taskId
  if task == nil then
    return nil, missing("taskId")
  end
  if type(task) ~= "string" or task == "" then
    return nil, protocol.err(protocol.E_BAD_PARAMS, "taskId must name a task row of this tab")
  end
  local done, err = host.call(maki.task.focus, task)
  if not done then
    return passthrough(err)
  end
  return true
end

--- A slash command, with the one refusal that belongs to the protocol rather than
--- to one plugin build (WIRE 9.5): `/reload` rebuilds the plugin host and would
--- take this listener and every connection on it with it.
local function names_reload(cmdline)
  if cmdline:lower():sub(1, #RELOAD_WORD + 1) == "/" .. RELOAD_WORD then
    return true
  end
  local head = cmdline:match("^/%s*(%w+)") or cmdline:match("^%s*(%w+)")
  return head ~= nil and head:lower() == RELOAD_WORD
end

H["_sh.maki/run_command"] = function(params)
  local cmdline = protocol.str(params.cmdline)
  if cmdline == "" then
    return nil, protocol.err(protocol.E_BAD_PARAMS, "cmdline is required")
  end
  if names_reload(cmdline) then
    return nil, protocol.err(protocol.E_BAD_REQUEST, RELOAD_REFUSAL)
  end
  local done, err = host.call(maki.api.run_command, cmdline)
  if not done then
    return passthrough(err)
  end
  return true
end

--- The method table conns.lua routes requests to. A method name is a whole
--- string, and a reader must not prefix-match one (WIRE 12).
M.handlers = H

return M