# WIRE: the v2 attach protocol (maki Lua plugin <-> gateway)

Status: frozen for M3. A change to any name, field, or rule here needs a new value in
`sh.maki/attach_proto`, and both sides keep working with the older value until the plugin
on the box is replaced.

This file is the whole contract for the unix socket between a maki TUI (through its
`gateway` Lua plugin) and maki-gateway. It replaces section 1 of `docs/CONTRACTS.md`,
which describes v1 and stays there as the record of what a running plugin speaks today.
Sections 2, 3, 4, and 5 of `docs/CONTRACTS.md` (the HTTP and SSE surface, the on-disk
shapes, the engine interface, the fakes) are not replaced by this file and do not change.

v2 is a subset of the Agent Client Protocol (ACP) version 1, the same subset
`docs/ACP_MIN.md` describes for the `maki acp` child process. One codec in this repository
reads and writes both: `internal/acpwire`. The plugin side is the ACP agent. The
gateway is the ACP client.

Terms used throughout:

- tab: one chat in a running TUI. ACP calls it a session, and `sessionId` names it.
- turn: one prompt and everything the model does before it stops.
- ask: a `session/request_permission` request the plugin sends and the gateway answers.
- extension method: a method named `_sh.maki/...`. The `_` prefix marks it as
  implementation-specific in ACP, and the reverse-DNS prefix `sh.maki/` marks the names
  inside `_meta` as maki's own.
- live session: a tab that a running TUI owns. A stored session is not live.

## 0. Method index

Direction C2A means gateway to plugin, A2C means plugin to gateway. `req` is a request
(it gets one answer), `note` is a notification (it gets no answer).

| method | dir | kind | answer |
|---|---|---|---|
| `initialize` | C2A | req | agent info, capabilities, and the `_meta` gate of section 2 |
| `session/list` | C2A | req | `{"sessions":[SessionInfo]}` |
| `session/load` | C2A | req | mode and config state, after the history replay of section 4 |
| `session/resume` | C2A | req | mode and config state, no replay |
| `session/prompt` | C2A | req | `{"stopReason":...}`, at end of turn |
| `session/cancel` | C2A | req | never: `-32601`, section 7.3 |
| `session/set_mode` | C2A | req | `{}` |
| `session/set_title` | C2A | req | `{}` |
| `_sh.maki/messages` | C2A | req | `{"messages":[Message]}`, native maki shape |
| `_sh.maki/queue` | C2A | req | `{"state":"started" \| "queued"}` |
| `_sh.maki/pop_queue` | C2A | req | `true` |
| `_sh.maki/notify` | C2A | req | `true` |
| `_sh.maki/plan` | C2A | req | `{"mode","path","ready","content"?}` |
| `_sh.maki/tasks` | C2A | req | the task rows, as an array |
| `_sh.maki/task_focus` | C2A | req | `true` |
| `_sh.maki/run_command` | C2A | req | `true` |
| `session/update` | A2C | note | one streamed change, section 11.1 |
| `_sh.maki/session_event` | A2C | note | one change ACP cannot carry, section 11.2 |
| `session/request_permission` | A2C | req | the answer of section 10 |

## 1. Transport

### 1.1 Socket path and discovery

Unchanged from v1. The plugin listens on `$MAKI_GATEWAY_SOCK` when that variable is set,
otherwise on `$XDG_STATE_HOME/maki/gateway/pid-<pid>.sock`, and the default state home is
`~/.local/state`. The socket is mode `0600`. The gateway scans that directory, connects to
every `pid-*.sock`, and runs the handshake of section 2 on each one. The gateway never
creates a socket and never writes to the socket directory.

One connection serves one TUI process for the life of that process. A plugin that accepts
a second connection serves it the same way, and both connections get the same
notifications. Section 10.3 says how a shared ask is answered.

### 1.2 Frames

Newline-delimited JSON. One JSON object per line, no length prefix, and no
`Content-Length` header. The rules for reading and writing are the v1 rules:

- Both sides keep a buffer, append each `read()` result to it, and cut frames on `\n`. A
  frame can arrive in several reads, and one read can carry several whole frames.
- A frame longer than 8 MiB, the newline included, ends the connection. The limit is
  enforced on the bytes before anything is parsed. A writer must not send a frame over the
  limit at all.
- An empty line is ignored.
- A line that is not one JSON object ends the connection. It is a peer that cannot frame a
  value, so nothing after it can be trusted.
- Writes are serialized. One frame reaches the socket whole, and frames leave in call
  order.

### 1.3 JSON-RPC 2.0 shapes

Every frame carries `"jsonrpc":"2.0"`. A reader does not test that field, and does not
reject a frame over it.

```json
{"jsonrpc":"2.0","id":1,"method":"session/list","params":{}}
{"jsonrpc":"2.0","id":1,"result":{"sessions":[]}}
{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01J8...","update":{}}}
```

- A request has `id` and `method`, and gets exactly one answer with the same `id`.
- A notification has `method` and no `id`. It never gets an answer. A sender must not put
  an `id` on a notification, because the receiver reads that as a request and waits to
  answer it.
- A response has `id` and exactly one of `result` or `error`. `"result":null` is a value
  and is a valid answer. A frame with an `id` and neither field answers nothing, so the
  receiver fails the waiting call and ends the connection. This is the v1 rule, and it
  holds in v2.
- `params` is optional on a request. A method that takes no argument is sent with
  `"params":{}`.

### 1.4 Ids

- An `id` is a JSON number or a JSON string. Both sides must accept either in a frame they
  read, and both key an open request on the id as it arrived: a number on its canonical
  decimal spelling, a string on its text. `"7"` and `7` are two ids, and a peer that
  re-spells `7` as `7.0` still reaches its caller (`idKey` in `internal/acpwire/frame.go`).
- The gateway numbers its own requests with the JSON strings `"g-1"`, `"g-2"`, and onward
  (`requestIDPrefix` in `internal/attach/conn.go`, `IDPrefix` in `internal/acpwire.Options`).
  A plugin numbers its own requests with JSON numbers from 1000 upward, keeps them numbers,
  and must not spend one below 1000 (`ASK_ID_FIRST` in `lua/gateway/conns.lua`). The split
  is the point. Both directions share one socket, so a number this side minted can be the
  number a plugin minted for its own ask, and the frame a plugin writes to withdraw that ask
  (section 10.2) is then read as the answer to an unrelated open call. One long-running TUI
  reached that state in 83 minutes of ordinary polling. Strings cannot collide with numbers.
- An answer echoes the `id` of its request as it arrived, byte for byte. A writer must not
  re-format a number or re-encode a string on the way back. A plugin that decodes JSON and
  re-encodes the value it read satisfies this for both shapes, which is what
  `protocol.lua` does.
- A response whose `id` matches no open request is logged, and the reader is told about it
  rather than left to guess, because one such frame is a protocol event and not noise: the
  plugin answering a request it sent itself (sections 10.2 and 10.3). `OnStrayResponse` in
  `internal/acpwire` is that report, and `internal/attach` closes the dialog it names. The
  connection stays open either way, because the other reason for a stray id is a caller that
  gave up on that call or a process that restarted.
- One request gets at most one answer that counts. A second answer for the same `id` is
  dropped.

### 1.5 Deadlines, and what keeps a connection alive

v2 has no ping method. Each side sets a deadline for every request it sends, and a
request that is not answered before its deadline is dropped: the caller gives up on the
id, and a late answer for it is ignored. Nothing on this wire can undo a request that
already left, so an answer that arrives after a deadline has no effect either way.

Liveness comes from two facts. A socket that dies reports it at once, because the reader
sees the end of the stream. A plugin that is stuck reports itself, because its answers
stop arriving. The gateway uses the `session/list` call it already makes on its scan clock
(3 seconds with the default timings, `scanEvery` in `defaultTimings` of
`internal/attach/attach.go`) as its liveness clock: two of those calls in a row that fail or
time out end the connection, and the tabs of that TUI leave the list. That is the same
treatment v1 gave a plugin that missed two pings.

A request that a plugin answers late is still a correct answer. A deadline is a caller's
choice, not a protocol event.

## 2. Handshake

### 2.1 `initialize`

One `initialize` per connection, and it is the first request on that connection. It
registers the protocol build and the identities of both sides.

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
  "protocolVersion": 1,
  "clientCapabilities": {}
}}
```

The gateway sends no capabilities. It reads no files for the agent, it opens no terminal,
and it has no form to answer an elicitation with, so `clientCapabilities` is an empty
object. It sends no `_meta`, because it has no maki datum of its own to declare.

The plugin answers:

```json
{"jsonrpc":"2.0","id":1,"result":{
  "protocolVersion": 1,
  "agentCapabilities": {
    "loadSession": true,
    "promptCapabilities": {},
    "sessionCapabilities": {"list": {}}
  },
  "_meta": {
    "sh.maki/pid": 4128,
    "sh.maki/cwd": "/home/gab/project",
    "sh.maki/attach_proto": 2,
    "sh.maki/maki_version": "0.7.1"
  }
}}
```

`loadSession: true` says the plugin answers `session/load` with the replay of section 4.
`sessionCapabilities.list` says it answers `session/list`. `promptCapabilities` is an
empty object, because a live tab takes text blocks only.

The four `sh.maki/` keys in `_meta` are required:

| key | type | meaning |
|---|---|---|
| `sh.maki/pid` | number | the process id of the TUI, which is also the name of its socket |
| `sh.maki/cwd` | string | the working directory of that TUI |
| `sh.maki/attach_proto` | number | the build of this document. v2 is `2` |
| `sh.maki/maki_version` | string | the maki version, as the host reports it |

`sh.maki/maki_version` is the one key of the four the plugin cannot ask the host for: maki
answers no Lua call with its own version. The plugin probes the binary of this very process,
with `readlink /proc/<pid>/exe` and `--version` on the file it names (`version_command` and
`probe_version` in `lua/gateway/host.lua`), which is the same shape the gateway's own health
route uses. The probe runs once, on a child task, because code that runs while the plugin
loads cannot park.

`unknown` is an allowed value, and a reader must take it: it is what the key reads before the
probe lands, on a system with no `/proc`, and for a host that prints no version. It is a fact
about this plugin rather than a number that might be wrong. The gateway writes the value into
its log lines and decides nothing from it. The gate of section 2.2 reads
`sh.maki/attach_proto`, which the plugin knows exactly.

A plugin can add more `sh.maki/` keys. One useful addition is `sh.maki/methods`, an array
of the extension method names that plugin answers, which lets a reader check a capability
instead of trying a call. The gateway ignores every `sh.maki/` key it does not know.

Before `initialize` is answered, the plugin answers every other request with `-32600` and
the message `initialize first`, and it pushes no notification. The gateway sends nothing
before `initialize` on a connection.

### 2.2 The version gate

The gateway must refuse an agent that does not declare `sh.maki/attach_proto` of `2` or
more. A missing key, a non-number, and a number below 2 all mean the same thing: this
plugin speaks v1 or older.

The refusal is loud, because a silent one is what made v1 confusing when an old plugin
met a new gateway:

1. The gateway closes that connection. It sends no further request on it.
2. The gateway reports the version it saw, the pid, and the socket path in one log line at
   error level.
3. The gateway publishes an SSE `notice` event (section 2 of `docs/CONTRACTS.md`) whose
   text names the pid and says the plugin is too old for this gateway.
4. The tabs of that TUI stay out of the session list. The gateway does not fall back to
   v1, and it does not guess at a v1 frame it cannot correlate.

An agent that cannot accept the client's `protocolVersion` answers `initialize` with
`-32600` and a message that names the version it was sent. This document fixes
`protocolVersion` at the ACP number `1`. The attach build number is a maki fact, so it
travels in `_meta` and not in that field.

### 2.3 `_meta` rules

Every maki-attach-specific datum goes in `_meta`, under a `sh.maki/` key. That applies to
requests, responses, and notifications, and to the objects nested in them that ACP gives
`_meta` room for.

- A reader must not fail a frame over an unknown `_meta` key, and must not fail a frame
  over a missing `sh.maki/` key other than the version gate of section 2.2.
- A writer must not put a maki name in a place ACP reserves, and must not rename an ACP
  field.
- A value that ACP can carry is not duplicated under a `sh.maki/` key, except where this
  document says so and gives the reason (`sh.maki/cwd` on a listed tab, section 3.1).
- ACP reads `_meta` keys as JSON, so a number stays a number and a boolean stays a
  boolean. A consumer that reads one as text must handle both, because a Lua encoder can
  write `true` and `"true"`.

## 3. `session/list`: the tabs of this TUI

The gateway calls it to build the list, and on its scan clock as its liveness clock (see
section 1.5 for the interval). It is the only way to learn that a tab opened or closed.

```json
{"jsonrpc":"2.0","id":2,"method":"session/list","params":{}}
{"jsonrpc":"2.0","id":2,"result":{"sessions":[
  {"sessionId":"01J8F6Z3H7Q0M2S4T6V8W9XABC",
   "cwd":"/home/gab/project",
   "title":"fix the flaky test",
   "updatedAt":"2026-11-12T09:14:03Z",
   "_meta":{
     "sh.maki/status":"working",
     "sh.maki/model":"anthropic/claude-opus-4.1",
     "sh.maki/cwd":"/home/gab/project",
     "sh.maki/mode":"build",
     "sh.maki/focused":true
   }}
]}}
```

The result is `SessionInfo` as ACP v1 defines it: `sessionId` and `cwd` are required,
`title` and `updatedAt` are optional, and `additionalDirectories` and `nextCursor` are not
used. The tab that no longer exists leaves the list, so the list is the whole truth about
what is live. The rows come in no defined order, and a caller must not read a meaning into
the order.

`updatedAt` is one format in v2: an ISO 8601 UTC timestamp, as ACP v1 defines it. v1 sent
epoch seconds here, and a live row showed `0` when the plugin knew nothing. v2 writes no
zero: a plugin that does not know the last activity of a tab omits `updatedAt`, and the
gateway reads an absent value as `0`, which is what it does today. The gateway converts
the timestamp to epoch seconds for `model.Session.updated_at`, because the registry and
the analytics route work in seconds.

### 3.1 The `sh.maki/` keys of a row

| key | type | meaning |
|---|---|---|
| `sh.maki/status` | string | `working`, `needs_input`, or `idle`, the three states a tab has |
| `sh.maki/model` | string | the model that tab runs now, or `""` when the host reports none |
| `sh.maki/cwd` | string | the working directory, the same string as the ACP `cwd` field |
| `sh.maki/mode` | string | `build` or `plan` |
| `sh.maki/focused` | boolean | the tab that the terminal screen shows, and that keys at the terminal type into |

`sh.maki/cwd` is present because the mapping of this protocol names it, and a reader must
not have to know which ACP field it duplicates. When both are present they hold the same
string, and the plugin must not let them differ. The gateway reads `sh.maki/cwd` first
and falls back to `cwd`, so a plugin that sends only the ACP field still gets a correct
row.

`sh.maki/status` has no ACP home. ACP v1 has no stable status vocabulary, and its
unstable `session/status` values are `open` and `saved`, which say whether a session is
in memory. That is a different question from "this tab is waiting for a person to answer
a prompt", so the three maki states travel as maki data. `docs/CONTRACTS.md` 2 keeps the
same three words in the HTTP shape.

A needs_input tab is one that waits on the person at the terminal: a permission ask the
terminal has, a `question` the terminal has, or plain input. Section 11.2 says how a
change reaches the gateway between list calls.

## 4. `session/load`: history, then the answer

The gateway calls `session/load` to read a tab's transcript as a stream of ACP updates.
This is the method a stock ACP client uses for the same job, and it is the only way to
get a tab's history in ACP shapes.

```json
{"jsonrpc":"2.0","id":3,"method":"session/load","params":{
  "sessionId":"01J8F6Z3H7Q0M2S4T6V8W9XABC",
  "cwd":"/home/gab/project",
  "mcpServers":[]
}}
```

`sessionId`, `cwd`, and `mcpServers` are all required by ACP v1. The gateway sends its
`sh.maki/cwd` for that tab and an empty `mcpServers`, and the plugin injects no MCP
servers.

The rule that makes this method: the plugin replays the stored history as `session/update`
notifications on the same connection, and answers the request only after the last one.

```json
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01J8...","update":
  {"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"fix the tests"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01J8...","update":
  {"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"the failing case is here"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01J8...","update":
  {"sessionUpdate":"tool_call","toolCallId":"toolu_01ABC","name":"read","title":"read file",
   "kind":"read","status":"pending","rawInput":{"path":"a.txt"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01J8...","update":
  {"sessionUpdate":"tool_call_update","toolCallId":"toolu_01ABC","status":"completed",
   "rawOutput":"one line"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01J8...","update":
  {"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}}}
{"jsonrpc":"2.0","id":3,"result":{
  "modes":{"currentModeId":"build","availableModes":[{"id":"build","name":"Build"},{"id":"plan","name":"Plan"}]},
  "configOptions":[{"id":"model","name":"Model","category":"model","type":"select",
    "currentValue":"anthropic/claude-opus-4.1",
    "options":[{"value":"anthropic/claude-opus-4.1","name":"anthropic/claude-opus-4.1"}]}]
}}
```

The update objects are the shapes `docs/ACP_MIN.md` 5 records for `maki acp`, and the fake
agent in `internal/acpengine/fake.go` writes the same ones. One `session/update` per
notification, tagged by `sessionUpdate`:

| `sessionUpdate` | used in a replay for |
|---|---|
| `user_message_chunk` | what the person sent. `session/prompt` text is not replayed by the plugin, only stored history is |
| `agent_message_chunk` | model output text |
| `agent_thought_chunk` | model thinking text |
| `tool_call` | the open of one tool card |
| `tool_call_update` | a change to one tool card, terminal status included |

`content` on a chunk is one `ContentBlock`, and a live tab sends
`{"type":"text","text":...}` only. The chunks of one message arrive in order, and a
reader joins a chunk to the one before it when they carry the same kind. There is no
frame that says "this message ended": a change of kind, or the answer to `session/load`,
ends the message. That is how ACP works, and it is what `maki acp` does.

The answer carries the mode and config state of the loaded tab, in the ACP shape, so one
reducer reads the same fields from `session/load` and `session/resume` here and from
`session/new` and `session/load` in `internal/acpengine`. The gateway keeps three things
from it: `modes.availableModes[].id`, so it can refuse a mode this tab never offered, and
`configOptions[]` where `id` is `model`, for the model name in the list. A plugin with no
modes to report omits `modes`, and one with no config option to report omits
`configOptions`.

`session/load` of an id that is not a live tab answers `-32002` with a message that holds
the words `session not live`, and section 13 says what the gateway makes of that. A load
of a tab the gateway already follows answers the replay again, because a load is a read
of history and not a subscription.

## 5. `session/resume`: follow a tab without the replay

`session/resume` attaches the connection to the update stream of a tab and pushes nothing
it has already stored.

```json
{"jsonrpc":"2.0","id":4,"method":"session/resume","params":{"sessionId":"01J8..."}}
{"jsonrpc":"2.0","id":4,"result":{
  "modes":{"currentModeId":"plan","availableModes":[{"id":"build","name":"Build"},{"id":"plan","name":"Plan"}]}
}}
```

`sessionId` is the only argument this protocol uses, and `cwd` and `mcpServers` are
optional here, so the gateway sends `sessionId` alone. A plugin that requires more must
say so in the error message rather than guess at a directory.

The line between the two methods: `session/load` replays and then answers, and
`session/resume` answers and replays nothing.

One subscription rule covers both, and it is the v1 rule: a plugin pushes every
`session/update` and every `_sh.maki/session_event` of every tab in its TUI to every
connection that finished the handshake. A connection does not have to load or resume a
tab to hear about it. So `session/resume` does not open a stream that was closed. Its job
is to confirm that the named tab is live and to report its mode and config state, which
is what a client does before it prompts a tab it did not load.

A resume of an id that is not live answers `-32002` with a message that holds
`session not live`.

## 6. `_sh.maki/messages`: the native transcript

The UI renders native maki `Message` JSON, the shape of section 3 of `docs/CONTRACTS.md`.
ACP has no method that reads a transcript back as native records, so this one cannot be
ACP. `session/load` gives ACP updates, and an update stream cannot rebuild what the store
holds: the `kind` of a message, the `display_text` the TUI shows, the exact block order,
and a block type ACP has no name for. So the payload travels verbatim under an extension
name.

```json
{"jsonrpc":"2.0","id":5,"method":"_sh.maki/messages","params":{"sessionId":"01J8...","last":200}}
{"jsonrpc":"2.0","id":5,"result":{"messages":[
  {"role":"user","content":[{"type":"text","text":"fix the tests"}],"created_at":1763000000000},
  {"role":"assistant","content":[
    {"type":"thinking","thinking":"the failing case is here"},
    {"type":"tool_use","id":"toolu_01ABC","name":"read","input":{"path":"a.txt"}},
    {"type":"tool_result","tool_use_id":"toolu_01ABC","content":"one line","is_error":false},
    {"type":"text","text":"done"}]}
]}}
```

- `sessionId` is required, and `last` is the number of records to read from the end. A
  missing or non-positive `last` means 200.
- `result.messages` is the `d` of the `{"t":"msg"}` lines, in the order the host holds
  them, with no field renamed, dropped, or added. A block type this document does not list
  stays in the array exactly as the host wrote it, and the browser shows its raw JSON in a
  details box.
- A live tab answers from memory. A tab that is not live answers `session not live`, and
  the gateway reads that transcript off disk through the store engine instead, which is
  what it does today.

## 7. Turns

### 7.1 `session/prompt`

The gateway sends the prompt as an ACP `ContentBlock` array, exactly as `maki acp` takes
it:

```json
{"jsonrpc":"2.0","id":6,"method":"session/prompt","params":{
  "sessionId":"01J8...",
  "prompt":[{"type":"text","text":"fix the tests"}]
}}
```

- The plugin joins the text blocks with `\n` and passes the result to the host as the
  input of a turn. A block of any other type answers `-32600` with a message that names
  the type it cannot take.
- The answer arrives at the end of the turn, not when the request is read. This is the ACP
  rule, and `docs/ACP_MIN.md` 3 records the same behavior for `maki acp`:

```json
{"jsonrpc":"2.0","id":6,"result":{"stopReason":"end_turn"}}
```

- `stopReason` is `end_turn`, `max_tokens`, `max_turn_requests`, `refusal`, or
  `cancelled`. A live tab reports `end_turn` for a normal stop, and `cancelled` for a turn
  the person at the terminal stopped. A gateway must read the stop reason as information
  and must not treat any of the five as a transport fault.
- A second `session/prompt` for a tab that is already running one answers `-32603`. The
  message must hold the word `busy`, and this is the fixed text: `session is busy: a
  prompt is already running`. It carries the ACP sentence that `maki acp` writes and the
  substring the gateway already maps, so one error string serves both engines.
- A prompt for a tab that is not live answers `-32002` with a message that holds `session
  not live: <sessionId>`.
- `session/prompt` means "start a turn now", and its answer is one object with one field.
  It cannot report a queue, and its `result` carries no maki `_meta`. A caller that wants
  to put text in front of a turn that is already running uses `_sh.maki/queue` (section
  7.2).
- The plugin must not answer the request before the turn ends. An early answer tells the
  gateway the turn is over, and the streamed frames that follow then belong to a turn
  nothing waits for.

### 7.2 `_sh.maki/queue`, and `_sh.maki/pop_queue`

Text steering keeps the real maki behavior: a person can type while the model works, and
the host either starts the turn as soon as it can or holds the text in the queue of the
tab. ACP has no vocabulary for a queued input, so this travels as maki data.

```json
{"jsonrpc":"2.0","id":7,"method":"_sh.maki/queue","params":{
  "sessionId":"01J8...","text":"use the table-driven form"
}}
{"jsonrpc":"2.0","id":7,"result":{"state":"queued"}}
```

- `result.state` is `started` when the host took the text and a turn is running, and
  `queued` when the host holds it for the end of the turn that is running. These are the
  two words the v1 `prompt` op returned, and they are the two words `POST
  /api/sessions/{id}/prompt` returns today.
- `text` is required, and an empty one answers `text is required`.
- A `sessionId` that is not a live tab answers a message that holds `session not live`.
- `_sh.maki/queue` with the text of a prompt is what the HTTP route calls for a live tab.
  The route reads `state` and answers 200 with it.

`_sh.maki/pop_queue` removes the first held input of the tab the terminal has focused,
which is the only queue the host can reach from a plugin.

```json
{"jsonrpc":"2.0","id":8,"method":"_sh.maki/pop_queue","params":{}}
{"jsonrpc":"2.0","id":8,"result":true}
```

It takes no argument. v1 took an optional `session` and answered
`only the focused queue can be popped: <id>` for a tab that was not the focused one. v2
drops the argument, so that error cannot happen and the caller reads the focused tab off
`session/list`.

### 7.3 `session/cancel` is deliberately unsupported

A live tab has no cancel. The TUI owns the terminal. A plugin that stops a turn from
outside leaves the person at that terminal looking at a screen that no longer matches the
state of the host. v2 therefore has no cancel, and this is a decision rather than an
omission.

The gateway still sends the standard method, and the plugin answers that it has none:

```json
{"jsonrpc":"2.0","id":9,"method":"session/cancel","params":{"sessionId":"01J8..."}}
{"jsonrpc":"2.0","id":9,"error":{"code":-32601,"message":"method not found"}}
```

- The gateway sends `session/cancel` as a request, so the plugin can answer. ACP defines
  it as a notification, and a stock ACP agent that receives one stays silent, which is
  also correct behavior. The gateway's deadline for the call ends the wait, and both paths
  mean the same thing to the caller.
- The gateway maps a `-32601` answer for this method to `501` on `POST
  /api/sessions/{id}/cancel`, which is what a live session answers today, and the same
  mapping for a call that gets no answer at all. A caller reads one outcome: a live tab
  cannot be cancelled over this socket.
- A person who wants a turn stopped stops it at the terminal. The gateway then sees the
  tab move from `working` to `idle` in `session/list`, and the turn's `stopReason` of
  `cancelled` when its own `session/prompt` is answered.

## 8. Tab settings

### 8.1 `session/set_mode`

```json
{"jsonrpc":"2.0","id":10,"method":"session/set_mode","params":{"sessionId":"01J8...","modeId":"plan"}}
{"jsonrpc":"2.0","id":10,"result":{}}
```

`modeId` is `build` or `plan`, the two ids the host uses. Any other value answers
`-32602` with the message `mode must be "build" or "plan"`, which is the v1 sentence. The
plugin pushes `session/update` with `current_mode_update` for the change too, because the
mode can also move at the terminal, where no request on this socket is involved (section
11.1).

A move at the terminal is found by diffing the list poll, and that sets its delay. The host
reports no mode event, so the plugin reads the mode of every row of `session/list`, keeps the
one it last announced for that tab, and pushes `current_mode_update` when the two differ
(`watch_mode` in `lua/gateway/ops.lua`, and the row's `sh.maki/mode` of section 3.1). The
gateway asks on the scan clock of section 1.5 (3 seconds by default), so a mode moved at the
screen reaches the socket up to one poll interval later. A plugin that could push on the move
itself is allowed, and keeps the diff as the backstop.

What the gateway does with the update is fixed here: it reads it, logs it, and publishes
nothing. `model.Session` has no mode field and the SSE envelope has no mode event
(`docs/CONTRACTS.md` 2), so the mode of a live tab is not on the browser's surface, and the
chip there shows the mode that browser last asked for. A plugin that wants the screen to show
the truth needs a field added to the frozen surface, which is a `docs/CONTRACTS.md` change and
not a change to this document.

### 8.2 `session/set_title`

```json
{"jsonrpc":"2.0","id":11,"method":"session/set_title","params":{"sessionId":"01J8...","title":"flaky test, fixed"}}
{"jsonrpc":"2.0","id":11,"result":{}}
```

`title` is a required string. An empty string clears the name, and the tab falls back to
the title the host generates, which is what a rename to nothing at the terminal does. v1
took `null` for a clear, and JSON has no empty `null` to echo, so one spelling is enough.

The answer is an empty object. ACP declares a `title` in the response of this method, and
a plugin can echo the title it set. The gateway reads nothing from the body: the title
reaches it as a notification (section 11.2), because the host can rename a tab on its
own.

ACP keeps `session/set_title` behind an unstable feature flag. This protocol fixes the
name and the params of the v1 schema, so a plugin that renames the method or the field
breaks v2, and so does a gateway that reads a result field.

## 9. The calls ACP has no name for

These six carry the payloads of today's `ops.lua` verbatim under `_sh.maki/` names. Only
the envelope changes: `{"id":N,"op":"name",...}` becomes a JSON-RPC request, and
`{"id":N,"ok":<value>}` becomes a JSON-RPC result. The argument names move to camelCase
where v1 used a snake_case one, and section 12 lists each rename.

### 9.1 `_sh.maki/notify`

```json
{"jsonrpc":"2.0","id":12,"method":"_sh.maki/notify","params":{"text":"gateway: session saved","sessionId":"01J8..."}}
{"jsonrpc":"2.0","id":12,"result":true}
```

`text` is required, and an empty one answers `text is required`. `sessionId` is optional:
with one, the notice goes to that tab, and without one it goes to the focused tab.

### 9.2 `_sh.maki/plan`

```json
{"jsonrpc":"2.0","id":13,"method":"_sh.maki/plan","params":{"sessionId":"01J8..."}}
{"jsonrpc":"2.0","id":13,"result":{"mode":"plan","path":"/home/gab/project/.maki/plan.md","ready":true,"content":"# Plan\n..."}}
```

The result is the v1 `plan` payload with no change: `mode`, `path`, `ready`, and `content`
only when the host returned one. `GET /api/plan/{id}` answers this object.

### 9.3 `_sh.maki/tasks`

```json
{"jsonrpc":"2.0","id":14,"method":"_sh.maki/tasks","params":{}}
{"jsonrpc":"2.0","id":14,"result":[
  {"id":"main","name":"main chat","focused":true},
  {"id":"01J8TASK","name":"research","focused":false,"status":"working","tool":"task"}
]}
```

The result is the array `maki.task.list()` returns, verbatim, and it is the array the v1
`tasks` op returned under `ok`. That includes every rule recorded in `docs/CONTRACTS.md`
1: the host lists the chats of the focused session only and takes no session argument,
row 1 is the main chat with `id:"main"` and no `status`, `status` is one of `working`,
`idle`, `done`, `error`, or `cancelled`, and an optional field stays away rather than
arrive as null.

The result is an array rather than an object, which JSON-RPC allows. The gateway keeps
reading `{"tasks":...}` as the shape its HTTP route returns, and it builds that wrapper
itself.

### 9.4 `_sh.maki/task_focus`

```json
{"jsonrpc":"2.0","id":15,"method":"_sh.maki/task_focus","params":{"taskId":"01J8TASK"}}
{"jsonrpc":"2.0","id":15,"result":true}
```

`taskId` is a row of `_sh.maki/tasks`, and `main` is the main chat. It shows one chat of
the tab the terminal already has focused, which is where keys typed at that terminal then
go. The host takes no session argument, so a chat of another tab is out of reach, and a
caller reads the focused tab off `session/list`.

v1 also took the same name in the frame's `id` field, because an old gateway sent it
there. v2 has no such field on a params object, so the fallback and its `id is required`
message leave the protocol with the `id` collision that made them necessary.

### 9.5 `_sh.maki/run_command`

```json
{"jsonrpc":"2.0","id":16,"method":"_sh.maki/run_command","params":{"cmdline":"/model zai/glm-4.6"}}
{"jsonrpc":"2.0","id":16,"result":true}
```

`cmdline` is required, and an empty one answers `cmdline is required`.

A `cmdline` that names `/reload` is refused. `/reload` rebuilds the plugin host, and that
takes this plugin, its listener, and every connection on this socket with it, so a
command that runs a model's request must never cause it. The answer is `cmdline not
allowed: /reload destroys the plugin host`. The check covers the literal prefix `/reload`
and the same word after a slash or white space in any case, which is what the plugin does
today. The rule is part of this protocol, not of one plugin build.

## 10. `session/request_permission`: the agent asks

This is the one request the plugin sends. The host slot for a permission prompt has two
answers, and a browser dialog has two buttons, so the two truths match exactly.

```json
{"jsonrpc":"2.0","id":1000,"method":"session/request_permission","params":{
  "sessionId":"01J8...",
  "toolCall":{"toolCallId":"toolu_01ABC","title":"bash: ls -1","name":"bash","rawInput":{"command":"ls -1"}},
  "options":[
    {"optionId":"allow_once","name":"Allow once","kind":"allow_once"},
    {"optionId":"reject_once","name":"Reject once","kind":"reject_once"}
  ]
}}
```

- `options` holds exactly these two entries, in this order. The host slot cannot say
  "remember this", so a plugin must not offer an option it cannot honor. A client that
  wants to allow always gets `allow_once`, and the tab asks again on the next call. That
  is what `docs/CONTRACTS.md` 2 records for `allow_always` on the attach bridge, and the
  option list now says it on the wire.
- `toolCall.title` is the line the terminal shows, built the way `maki acp` builds it:
  `<tool>: <scope>`. `name` is the programmatic tool name, and `rawInput` is the tool
  input when the host gives one. `kind` and `status` are optional here, and a plugin that
  reports no `kind` omits it.
- `toolCallId` ties the ask to the `tool_call` notification of the same call, so a browser
  can put the dialog on top of the card that is already on screen.

### 10.1 The answer

```json
{"jsonrpc":"2.0","id":1000,"result":{
  "outcome":{"outcome":"selected","optionId":"allow_once"},
  "_meta":{"sh.maki/guidance":"only in this directory"}
}}
```

`result.outcome` is the ACP `RequestPermissionOutcome`:
`{"outcome":"selected","optionId":...}` for a decision, and `{"outcome":"cancelled"}` for
a dialog that was withdrawn.

- `allow` answers `allow_once`. `allow_always` answers `allow_once`, because the slot has
  nothing else to give. `deny` answers `reject_once`.
- Guidance is free text the host can take with a deny, and ACP has no field for it, so it
  travels under `sh.maki/guidance` in the `_meta` of the `result` object. A deny without
  the key is a deny with no words, which is all the terminal prompt gives.
- A response with an `optionId` the plugin did not offer, a `result` the plugin cannot
  parse, an `error` instead of a `result`, or a frame with neither, is a deny. This
  mirrors `maki-acp`, which turns anything it cannot read into the safe answer. A deny
  from garbage carries no guidance.
- An answer for an `id` the plugin never asked is ignored, and the connection stays open.

### 10.2 The 55 second fallthrough

The bridge parks an ask, and the person at the terminal can answer the prompt in front of
them instead. At 55 seconds with no answer, the plugin calls the previous slot, which
shows the terminal prompt, and it ends its own ask:

```json
{"jsonrpc":"2.0","id":1000,"result":{"outcome":{"outcome":"cancelled"}}}
```

The plugin sends that frame itself, on the same connection, as the answer to the request
it sent. The gateway reads it as a withdrawn dialog: it clears the parked ask and publishes
the SSE `permission_cancel` event with the request id it had published for that ask. v1
reported the same fact with a `permission_cancel` event frame, and v2 has no need of it,
because ACP already has a word for "this ask is off".

How the gateway comes to see that frame is part of the contract, because the frame answers a
request the gateway never sent. Section 1.4 routes it: the codec finds no open call with that
id, and reports it to `OnStrayResponse` (`internal/acpwire`), and `internal/attach` reads
that report as the withdrawal of the ask whose plugin id it is (`withdrawn` in
`internal/attach/events.go`). An id that names no parked ask belongs to a call this side gave
up on, and changes nothing. A withdrawal that answers with an `error` rather than a `result`
closes the dialog the same way, because the plugin holds nothing either way. This replaces
the byte-sniffing tap the gateway needed before section 1.4 put stray replies in front of a
handler.

The plugin must not park an ask that a connection has already answered. If the answer
arrives first, there is nothing to cancel and no fallthrough to make.

### 10.3 Two gateways on one plugin

When more than one connection is open, the plugin sends the ask to each one and takes the
first valid answer. Every other answer for that `id` is dropped, because a request is
answered once. The plugin then pushes its own `cancelled` answer to the connections that
had not answered, so their dialogs close. A gateway that reads that frame must treat it
as a withdrawn ask and not as an error.

An ask with no answer at all parks the tool, and the tool waits. This is the one reason
the 55 second rule is part of the protocol rather than a detail of one plugin build.

### 10.4 The bridge is on while a client is connected

v1 had a `permissions` op, and the gateway turned the bridge on after `hello`. v2 has no
such method: a plugin raises `session/request_permission` for every host permission ask
while at least one connection has finished the handshake, and it does not park anything
with no connection. That is the effective behavior of v1, because the gateway was the
only client and it always turned the bridge on.

## 11. Notifications the plugin pushes

Both methods are notifications. A notification gets no answer, and section 12 says what
happens to a name the reader does not know.

### 11.1 `session/update`

The ACP stream, with the same inner shapes as section 4. Params are
`{"sessionId":...,"update":{...}}`, and the `update` object is tagged by `sessionUpdate`.
A live tab uses these five:

| `sessionUpdate` | pushed for |
|---|---|
| `agent_message_chunk` | streamed model text |
| `agent_thought_chunk` | streamed thinking text |
| `tool_call` | a tool call started |
| `tool_call_update` | a tool call changed, or ended |
| `current_mode_update` | the mode moved, after `session/set_mode` or a change at the terminal |

`tool_call` and `tool_call_update` carry what the host reports at `ToolStart` and
`ToolDone`. `docs/CONTRACTS.md` 1 lists those two events with their v1 fields, and this
table says where each field goes:

| v1 `tool_start` field | v2 field | v1 `tool_done` field | v2 field |
|---|---|---|---|
| `session` | `params.sessionId` | `session` | `params.sessionId` |
| `tool_id` | `update.toolCallId` | `tool_id` | `update.toolCallId` |
| `tool` | `update.title` and `update.name` | `tool` | `update.title` |
|  |  | `is_error` | `update.status`, which is `failed` when true and `completed` when false |
|  |  | `bytes` | `update._meta["sh.maki/bytes"]` |
|  |  | `duration_ms` | `update._meta["sh.maki/duration_ms"]`, omitted when the host has none |

```json
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01J8...","update":
  {"sessionUpdate":"tool_call","toolCallId":"toolu_01ABC","name":"bash","title":"bash: ls -1",
   "status":"pending"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01J8...","update":
  {"sessionUpdate":"tool_call_update","toolCallId":"toolu_01ABC","status":"completed",
   "_meta":{"sh.maki/bytes":212,"sh.maki/duration_ms":40}}}}
```

Three rules follow from the shapes, and both sides must hold them:

1. A `tool_call` opens a card, and every `tool_call_update` for that `toolCallId` changes
   the card that is open. A terminal status (`completed` or `failed`) ends it. An update
   for a card that was never opened is still a valid frame, and a reader treats it as an
   out-of-order start.
2. `ToolDone` carries no text, so a live tab reports no `rawOutput` and no `content`. A
   tool card on a live tab shows its name, its status, and its size in bytes. The full
   result of a call reaches the browser through `_sh.maki/messages` after a `dirty`
   refetch, which is what happens today.
3. A streamed turn of a live tab ends with the answer to its `session/prompt` when the
   gateway started the turn. When the person at the terminal started it, the end of the
   turn reaches the gateway as `usage_update` plus a status event of `idle` (section
   11.2). `usage_update` keeps its ACP shape:
   `{"sessionUpdate":"usage_update","used":53000,"size":200000,"cost":{"amount":0.02,"currency":"USD"}}`,
   and `cost` stays away for a model with no published price.

A plugin does not push `user_message_chunk` for a live turn. The text of a prompt the
gateway sent is already in its own record, and pushing it again paints the line twice.
Only a replay of stored history uses that variant (section 4).

A plugin pushes no `config_option_update` and no `session_info_update`. A model change
and a rename reach the gateway as events, so a second path for the same fact means a
second reducer to keep in step. Section 11.2 carries both.

### 11.2 `_sh.maki/session_event`

Everything ACP cannot push travels here. The params are flat: `kind`, `sessionId`, and the
fields of that kind, which are the v1 event payloads with their names moved to camelCase.

```json
{"jsonrpc":"2.0","method":"_sh.maki/session_event","params":{
  "kind":"status","sessionId":"01J8...","status":"needs_input","title":"flaky test","focused":false}}
```

| `kind` | fields | from v1 event | reaches the browser as |
|---|---|---|---|
| `status` | `status`, `title`, `focused` | `status` (SessionStatusChanged) | `status` with the fields present, and `dirty` with reason `status` |
| `title` | `title` | `title` (SessionTitleChanged) | `status` with `title` |
| `model` | `model` | `model` (ModelChanged) | `status` with `session` alone |
| `question` | none | `question` | `status` with `session` alone, which tells the browser that tab waits for a person |
| `tasks_changed` | none | `tasks_changed` | `tasks` |
| `task_focus` | `taskId` | `task_focus` | `task_focus` with `id` set to `taskId` |

The last column is ratified here, because the browser reducer was written against v1 and the
SSE surface is frozen (`docs/CONTRACTS.md` 2). Two of its rows need stating exactly:

- A `status` event publishes two hub events on the gateway side, in this order: the `status`
  event carrying the fields this document names, then a `dirty` event with reason `status`
  for the same tab (`sessionEvent` in `internal/attach/events.go`). The status event patches
  the row the browser holds, and the dirty event is the refetch nudge v1 called `turn_start`:
  a tab that starts or stops working changed its transcript.
- The `usage_update` of `session/update` publishes one `dirty` event with reason `turn_end`,
  and no token numbers, which is how the browser learns to refetch a turn the person at the
  terminal started (section 11.1 rule 3, `sessionUpdate` in `internal/attach/events.go`). A
  turn the gateway started ends with the answer to its own `session/prompt`, and publishes
  nothing extra.

- `kind` is required, and a reader must ignore an event whose `kind` it does not know. The
  list grows by addition, and that is the point of the wrapper.
- `sessionId` is required on every kind, and it names the tab the change belongs to. That
  is not always the focused tab, which is the trap `docs/CONTRACTS.md` 1 records for
  `tasks_changed`.
- A `status` event carries `title` and `focused` because the v1 event did, and a browser
  patches the fields it is given. `focused` is not part of `model.Session`, so it travels
  on this event and on `session/list` only.
- A `question` event is raised when a tab enters `needs_input` and the `question` tool is
  the reason. It is best effort, and it can name a plain input wait. There is no answer to
  a question on this socket: the terminal answers it. A future web form for a question
  uses ACP `elicitation/create`, which `internal/acpengine` already answers, and that
  needs no new event kind.
- A `task_focus` event names the row of `_sh.maki/tasks` now on the screen, and `main` is
  the main chat. v1 carried that name in the frame's `id` field, which is the request
  number on a response, and that collision is the reason v1 needed the note it carries. v2
  names it `taskId` inside `params`, and the gateway writes the v2 name into the SSE `id`
  field, so the browser sees the envelope of section 2 of `docs/CONTRACTS.md` unchanged.

`turn_start` and `turn_end` from v1 have no kind here, and that is not an omission. A
turn that the gateway started ends with the answer to its own `session/prompt`. A turn
that the terminal started is visible as the tab moving from `idle` to `working` and back,
which is a `status` event, plus the streamed `session/update` frames and the closing
`usage_update`. The v1 fields `reason` and `num_turns` have no v2 home, and no gateway
code reads them today. The v1 field `usage`, `cost`, and `context_size` become `used`,
`size`, and `cost` in `usage_update`, which is ACP's own shape for the same three
numbers.

## 12. Unknown methods, and names that move

Both sides must obey these three rules, because a protocol that both ends must upgrade at
the same instant cannot grow.

1. An unknown notification method is ignored. The reader logs it at debug level and keeps
   the connection. This holds in both directions, and it is what lets a plugin push a new
   kind to an older gateway.
2. An unknown request method is answered `-32601` with the message `method not found`. A
   request must never be left unanswered, because a silent answer parks the caller. The
   gateway maps a `-32601` for `session/cancel` to `501`, as section 7.3 says, and a
   `-32601` for any other method to a 500 that names the method.
3. An unknown `kind` inside `_sh.maki/session_event`, an unknown `sessionUpdate` inside
   `session/update`, and an unknown `sh.maki/` key inside `_meta` are each ignored the
   same way. A new value in a field the reader knows is not a protocol error.

A method name is a whole string, and a reader must not prefix-match one. `_sh.maki/tasks`
and `_sh.maki/task_focus` are two names.

Argument names that changed from v1, in one list:

| v1 argument | v2 argument |
|---|---|
| `session` on an op | `sessionId` everywhere, without exception |
| `task_id` on `task_focus` | `taskId` |
| `cmdline` | `cmdline`, unchanged |
| `text` on `prompt` | `prompt`, an array of one text block, on `session/prompt`, and `text` on `_sh.maki/queue` |
| `last` on `messages` | `last`, unchanged |
| `on` on `permissions` | gone, section 10.4 |
| `request_id` on `respond` | the JSON-RPC `id` of the ask, section 10 |

## 13. Errors

A JSON-RPC error object carries `code`, `message`, and optional `data`. `data` holds
whatever the plugin has to say about it, and the gateway logs it and does not parse it.

The codes v2 uses:

| code | used for |
|---|---|
| `-32601` | a method this build does not answer, `session/cancel` included |
| `-32603` | a busy tab: one `session/prompt` at a time, with a message that holds `busy` |
| `-32000` | a session this TUI cannot take: the claim is held elsewhere. The message must hold `another` or `elsewhere` |
| `-32602` | an argument that is present and wrong, such as a mode id that is not `build` or `plan` |
| `-32600` | a request that is malformed, or one that arrived before `initialize`, or a content block a live tab cannot take |
| `-32002` | a `sessionId` that is not a live tab. The message must hold `session not live` |

`-32000` for a held session is the same number `maki acp` uses for the same fact
(`docs/ACP_MIN.md` 9), which is why one gateway mapping covers both engines.

The code is not the mapping, and one row of section 13.1 has to be read before another. A tab
that closed while a turn was parked answers that turn's `session/prompt` with `-32000` and the
words `session not live: <sessionId> (the tab closed)` (`session_end` in
`lua/gateway/turns.lua`). `-32000` is the held-elsewhere row, so a reader that keyed on the
code alone would answer 409 to a person who asked about a tab that is gone. `MapError` reads
the message, so `session not live` wins, and the caller gets its 404. A writer must therefore
keep the words of section 13.1 in `error.message` even when the code of section 13 is already
fixed: the words carry the fact.

### 13.1 The verbatim strings

The plugin must keep these words in `error.message`. The gateway maps a message by
substring, exactly as it does today, through `MapError` in `internal/attach/attach.go`,
and a reworded message turns a clean refusal into a 500. A suffix after the fixed words
is allowed, and the id of the tab is a useful suffix.

| message holds | gateway reads | HTTP |
|---|---|---|
| `session not live` | `model.ErrNotFound` | 404 |
| `expired` | `model.ErrNotFound` | 404 |
| `unknown` | `model.ErrNotFound` | 404 |
| `no such` | `model.ErrNotFound` | 404 |
| `another` | `model.ErrOpenElsewhere` | 409 |
| `elsewhere` | `model.ErrOpenElsewhere` | 409 |
| `busy` | `model.ErrBusy` | 409 |
| anything else | the message itself | 500 |

`no interactive UI attached` is one of the "anything else" rows. The plugin must keep
that phrase exactly as it is, because it is what the host reports when there is no TUI,
and the message reaches a person who reads the log. It has no mapping today, and v2 does
not add one.

`expired` has a narrower job in v2 than in v1. v1 answered `expired` to a `respond` op
for a permission id that had gone away. v2 cannot: an answer for an ask that the plugin
already closed is a response frame with an id the plugin does not hold, and section 10.1
says it is ignored. The word stays in the table for two reasons. The gateway writes it
itself (`permission %d expired`) when the browser answers an ask this side dropped, and
the mapping must keep reading it in either direction.

### 13.2 Transport faults

A socket error, a frame over 8 MiB, a line that is not one JSON object, a response with
neither `result` nor `error`, and the end of the stream are not errors of one request.
They end the connection, every call that waits on it fails, the tabs of that TUI leave
the list, and the gateway publishes an SSE `notice` that names the pid. That is the v1
treatment, and it is what `internal/acpwire` implements.

## 14. What this buys, and what it costs

What v2 gains over v1:

- One codec for both engines, so the framing, id, deadline, and size rules are written
  once and tested once (`internal/acpwire`).
- Real ACP method names and shapes for tabs, history, turns, modes, and permission asks. A
  browser reducer that reads `session/update` from a web child reads the same frames from
  a live tab, and the plugin does not have to invent a name for anything ACP already has.
- An explicit version gate, so an old plugin is refused in one log line and one notice,
  and not passed over in silence.
- A written-down ceiling, so M3 can be judged against it.

What it costs:

- Every request carries a `method` and a `params` object rather than a flat op frame, so
  one call is a few dozen bytes bigger. On a local unix socket, that is not a cost anyone
  can measure, and the answer is worth it.
- Two methods that v1 had are gone: `new` and `permissions`. Section 10.4 covers the
  second, and Appendix B covers the first.
- `_sh.maki/messages` stays bespoke, because ACP has no method that reads a native
  transcript back.

## Appendix A: v1 to v2, one table

The left column is the v1 name from `docs/CONTRACTS.md` 1. The right column is what
carries the same job in v2.

| v1 op or event | v2 |
|---|---|
| `hello` | `initialize`, and its `_meta` of section 2.1. The `plugin:"1"` version string becomes `sh.maki/maki_version`, and the `sessions` count is `session/list` |
| `ping` | no method. The list poll is the liveness clock, section 1.5 |
| `list` | `session/list`, and each row is a `SessionInfo` with the maki fields in `_meta` |
| `messages` | `_sh.maki/messages`, same payload |
| `prompt` | `_sh.maki/queue` for the `started`/`queued` answer, and `session/prompt` for a turn whose answer arrives at end of turn |
| `new` | none. A stock client cannot open a tab, and neither can this gateway. Appendix B |
| `set_mode` | `session/set_mode`, with `mode` renamed to `modeId` |
| `set_title` | `session/set_title`, with `null` replaced by `""` |
| `notify` | `_sh.maki/notify`, same payload |
| `plan` | `_sh.maki/plan`, same payload |
| `tasks` | `_sh.maki/tasks`, same payload, still an array |
| `task_focus` (op) | `_sh.maki/task_focus`, with `task_id` renamed to `taskId` and the `id` fallback gone |
| `run_command` | `_sh.maki/run_command`, same payload and the same `/reload` refusal |
| `pop_queue` | `_sh.maki/pop_queue`, with no argument |
| `permissions` | none. The bridge is on while a client is connected, section 10.4 |
| `respond` | the JSON-RPC answer to the plugin's own `session/request_permission`, section 10.1 |
| `event: status` | `_sh.maki/session_event` with `kind:"status"` |
| `event: turn_start` | a `status` event to `working`, and the streamed `session/update` frames |
| `event: tool_start` | `session/update` with `tool_call` |
| `event: tool_done` | `session/update` with `tool_call_update` |
| `event: turn_end` | the answer to `session/prompt`, plus `usage_update` and a `status` event to `idle` |
| `event: title` | `_sh.maki/session_event` with `kind:"title"` |
| `event: model` | `_sh.maki/session_event` with `kind:"model"` |
| `event: question` | `_sh.maki/session_event` with `kind:"question"` |
| `event: permission` | `session/request_permission`, and its `request_id` is now minted by the gateway rather than the plugin |
| `event: permission_cancel` | the plugin's own `cancelled` answer to the ask, section 10.2 |
| `event: tasks_changed` | `_sh.maki/session_event` with `kind:"tasks_changed"` |
| `event: task_focus` | `_sh.maki/session_event` with `kind:"task_focus"` and `taskId` |
| `{"id":N,"ok":V}` | `{"jsonrpc":"2.0","id":N,"result":V}` |
| `{"id":N,"error":M}` | `{"jsonrpc":"2.0","id":N,"error":{"code":C,"message":M}}`, where C is the code of section 13 that fits the message and M keeps the same words |
| a frame with neither `ok` nor `error` | a frame with neither `result` nor `error`: a protocol violation that ends the connection |
| an event frame with a task name in `id` | `taskId` inside `params` of a `task_focus` event |

## Appendix B: what a stock ACP client still cannot do here

This socket speaks a subset of ACP v1 with maki extensions. A client that implements ACP
and nothing else can list tabs, follow them, read their history as updates, prompt them,
change their mode and title, and answer permission asks. It cannot do the following, and
each ceiling is a decision rather than an accident.

1. Open a session. There is no `session/new` on this socket. A live tab is opened by a
   person at the terminal with `maki`, and a plugin answers `session/new` with `-32601`.
   The v1 `new` op had the same limit in practice: it opened a background tab in the TUI
   that already had one, took a prompt, and refused a `cwd` that was not the current
   project, and no HTTP route reached it, because `POST /api/sessions` always asks the web
   engine. A client that must create a session creates it by spawning `maki acp`, which is
   what the gateway does.

   The gateway does not pretend otherwise, and it still asks once. `Engine.Create` in
   `internal/attach/attach.go` sends one `session/new` to the lowest socket path it holds,
   with `cwd` and an empty `mcpServers`, and sends it no second time to another socket. Its
   answers are fixed: `-32601` (no such method, what the plugin of section 12 answers) and
   `-32000` (a session this TUI will not take) both become `model.ErrUnsupported`, which
   `internal/httpapi` writes as 501, and no live socket at all is `model.ErrNotFound`, which
   is 404. A build that does answer `session/new` is read: its `sessionId` is routed to that
   connection, the browser is told the list changed, and the caller gets the id.
2. Discover the extensions. ACP gives a client no way to ask which extension methods an
   agent answers. A stock client cannot find `_sh.maki/messages`, and so cannot read the
   native transcript, and cannot see the queue of a tab. A plugin can advertise them under
   `sh.maki/methods` in its `initialize` `_meta` (section 2.1), and a stock client reads
   no `_meta` at all.
3. Read status without polling. `working`, `needs_input`, and `idle` are maki states in
   `_meta` of a `SessionInfo` row. ACP's stable vocabulary has none of them, and its
   unstable `session/status` answers whether a session is in memory. A stock client polls
   `session/list` to learn that a tab is waiting for a person.
4. See the focused tab, or move it. `sh.maki/focused` and `_sh.maki/task_focus` are maki
   facts. A stock client sees a flat list of tabs and has no notion of the screen a person
   is looking at.
5. Read a native transcript. `_sh.maki/messages` is the only method that returns maki
   `Message` JSON. The ACP replay of `session/load` cannot rebuild `kind`, `display_text`,
   an unknown block type, or the exact block order.
6. Steer or read the input queue. `_sh.maki/queue` and `_sh.maki/pop_queue` carry the two
   words `started` and `queued` and the queue of the focused tab. ACP has no queue.
7. Stop a turn. `session/cancel` is answered `-32601` on purpose (section 7.3), so a stock
   client's cancel does nothing and a live tab is stopped at the terminal.
8. See the output of a tool call on a live tab. `ToolDone` reports a status, a size in
   bytes, and a duration, and it carries no text, so `rawOutput` and `content` stay away
   from a live tab's tool cards. The result of a call reaches a browser through
   `_sh.maki/messages` after a `dirty` refetch.
9. Rename a plan, read a plan, or run a command. `_sh.maki/plan` and
   `_sh.maki/run_command` are maki-only, and `/reload` is refused by rule (section 9.5).
10. Answer a question or a plan approval in the web UI. The `question` kind says a tab is
    waiting, and the terminal is the only thing that can answer. A plugin that wants the
    web to answer one adds ACP `elicitation/create`, which needs no change to this
    document.
11. Ask for "always". `session/request_permission` offers two options here and no others,
    because the host slot has two answers (section 10).
12. Reach another TUI. One socket is one process, and a plugin can only see and change the
    tabs of the TUI that runs it. The gateway merges the lists of many sockets, which is a
    gateway job and not a protocol feature.
