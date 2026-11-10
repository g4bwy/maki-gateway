# ACP_MIN: the ACP subset `internal/acpengine` speaks

Appendix A of CONTRACTS 5. This file is the oracle for the fake in
`internal/acpengine/fake.go` and the reference for anyone changing either side.

Every JSON block here is copied from the source named above it, or assembled
from the `serde` attributes on the types that source builds. `maki-acp` is the
Rust crate at `/home/gab/test/maki/maki-acp`, `src/cli.rs` is the maki CLI at
`/home/gab/test/maki/src/cli.rs`, and `schema` is
`agent-client-protocol-schema 1.16.0`, the version in maki's `Cargo.lock`, and
its paths are relative to `src/`.

## 0. Transport

Newline-delimited JSON objects on the child's stdio. One frame per line, no
length prefix, no `Content-Length`, no LSP header.

The agent writes one `serde_json` value plus `\n` per frame
(`maki-acp/src/server.rs:119-128`):

```rust
while let Ok(msg) = out_rx.recv_async().await {
    let mut handle = stdout.lock();
    if serde_json::to_writer(&mut handle, &msg).is_ok() {
        let _ = handle.write_all(b"\n");
```

It reads with `read_line` on a `BufReader` over stdin (`server.rs:170-181`), so
a frame has no length limit. The gateway decodes with `json.Decoder` for the same
reason: one tool result can pass 64 KiB.

Every agent frame carries `"jsonrpc": "2.0"`, added by `JsonRpcMessage::wrap`
(`schema/rpc.rs:39-60`). The agent ignores it on input, because dispatch reads
only `id`, `method`, `params`, `result`, and `error` (`server.rs:227-254`). The
gateway sends it anyway.

Requests this side sends get integer ids starting at 1. Agent requests get
integer ids starting at 1000 (`server.rs:47`):

```rust
const FIRST_OUTGOING_REQUEST_ID: i64 = 1000;
```

## 1. `initialize` (once per process, before any session)

Client → agent. Required params `protocolVersion`, `clientCapabilities`
(`schema/v1/agent.rs:37-64`, `rename_all = "camelCase"`). `protocolVersion` is a
number, and version 1 is the number `1` (`schema/version.rs:12,21`).

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
  "protocolVersion": 1,
  "clientCapabilities": {
    "fs": {"readTextFile": false, "writeTextFile": false},
    "terminal": false,
    "elicitation": {"form": {}}
  },
  "clientInfo": {"name": "maki-gateway", "version": "1"}
}}
```

`elicitation.form` is the only capability that changes agent behaviour here.
`supports_form` requires an object, not a boolean
(`maki-acp/src/elicitation.rs:38-40`), and its own test spells the wire shape
(`elicitation.rs:265-268`):

```rust
let caps: ClientCapabilities = serde_json::from_value(serde_json::json!({
    "elicitation": { "form": {} }
}))
.unwrap();
assert!(supports_form(&caps));
```

Ask for it and the `question` tool is shadowed by the ACP one, so a question
reaches this side as `elicitation/create`. Do not ask, and maki drops the tool
from the model's list entirely and the agent asks in plain text instead
(`server.rs:465-472`). The gateway never answers `fs/*` or `terminal/*`, so both
are `false`.

Agent → client, built at `maki-acp/src/methods.rs:16-29`:

```json
{"jsonrpc":"2.0","id":1,"result":{
  "protocolVersion": 1,
  "agentCapabilities": {
    "loadSession": true,
    "promptCapabilities": {"image": true, "embeddedContext": true},
    "mcpCapabilities": {"http": true},
    "sessionCapabilities": {"list": {}}
  },
  "authMethods": [],
  "agentInfo": {"name": "maki", "version": "0.0.0"}
}}
```

The gateway reads nothing from this reply. It exists to register the
capabilities and to prove the child speaks ACP before `session/new`.

## 2. `session/new`

Params `cwd` (absolute) and `mcpServers` are both required fields. The rest have
defaults (`schema/v1/agent.rs:1224-1246`). The gateway injects no MCP servers:

```json
{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/repo","mcpServers":[]}}
```

Result, built at `maki-acp/src/methods.rs:31-43` and `:49-62`, with the session
id chosen by maki (`NewSessionResponse`, `schema/v1/agent.rs:1315-1342`):

```json
{"jsonrpc":"2.0","id":2,"result":{
  "sessionId": "01J8F6Z3H7Q0M2S4T6V8W9XABC",
  "modes": {
    "currentModeId": "build",
    "availableModes": [{"id": "build", "name": "Build"}, {"id": "plan", "name": "Plan"}]
  },
  "configOptions": [{
    "id": "model",
    "name": "Model",
    "category": "model",
    "type": "select",
    "currentValue": "anthropic/claude-opus-4-5",
    "options": [{"value": "anthropic/claude-opus-4-5", "name": "anthropic/claude-opus-4-5"}]
  }]
}}
```

The kind payload is flattened next to `id` and `name`
(`schema/v1/agent.rs:3399-3416`, `SessionConfigSelect` at `:3312-3317`), which is
why `currentValue` sits at the top of the option object.

The gateway keeps three things: `sessionId` (the id every later frame names, and
the session id in `GET /api/sessions`), `modes.availableModes[].id` (so
`SetMode` can refuse a mode this agent never offered), and
`configOptions[]` where `id == "model"` (the model name shown in the list).

One server holds one session (`server.rs:101`). A second `session/new` closes
the first (`new_session` → `close_session`, `server.rs:289-319`), so the gateway
spawns one child per web session and never reuses a child for a second one.

## 3. `session/prompt`, and when it is answered

```json
{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{
  "sessionId": "01J8F6Z3H7Q0M2S4T6V8W9XABC",
  "prompt": [{"type": "text", "text": "fix the tests"}]
}}
```

`prompt` is a `[ContentBlock]` array, tagged by `type`
(`schema/v1/agent.rs:4223-4239`, `schema/v1/content.rs:27-56`). The agent joins
the text blocks with `\n` and reads `image` blocks too
(`server.rs:903-934`). The gateway sends one text block.

`handle_prompt` stores the request id and returns without replying, so no
response arrives when the server handles the request (`server.rs:756-783`):

```rust
let mut pending = session.pending.lock().unwrap();
if pending.prompt.is_some() {
    return Err(AcpError::new(-32603, "a prompt is already running"));
}
session.handle.input_tx.send(input).map_err(|_| AcpError::new(-32603, "session ended"))?;
pending.prompt = Some(id.clone());
Ok(())
```

`handle_request` replies only when the handler returned `Ok`/`Err`, and
`session/prompt` answers `Ok(()) => return` (`server.rs:278-286`). The reply is
written at end of turn by the event pump, when the agent reaches `Done`
(`server.rs:1059-1076`, `finish_turn` at `:1106-1110`):

```rust
AgentEvent::Done { reason, cost: ledger, .. } => {
    if let Some(id) = finish_turn(&pending) {
        let resp = PromptResponse::new(translate::map_done_reason(reason));
        send(&out_tx, Response::<AgentResponse>::new(id, Ok(AgentResponse::PromptResponse(resp))));
    }
```

`stopReason` values: `end_turn | max_tokens | max_turn_requests | refusal |
cancelled` (`schema/v1/agent.rs:4353-4372`, mapped at `translate.rs:361-374`).

```json
{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}
```

A turn that dies on a provider error answers with an error instead
(`server.rs:1078-1098`): `-32000` (`auth_required`) when the provider rejected
the credentials, and `-32603` otherwise. Both carry the message as `data`.

Consequence for the gateway: `Prompt` writes the frame, returns `"started"` to
HTTP, and a background goroutine waits for the reply. Two details follow from
this, both in `engine.go`:

- One turn per session is tracked locally, so a second `POST /prompt` gets
  `model.ErrBusy` (409) without waiting for the agent's `-32603`.
- `Prompt` then waits up to 400 ms (`promptAdmission`) for a reply that can only
  be a rejection. This is a deviation from "return as soon as it is sent": a
  rejection that arrives later than that window reaches the browser as a
  `notice` event instead of a 409. CONTRACTS 2 asks for a 409 on a busy web
  session, and a rejection the server answers in one step is the only case worth
  reporting as a status code.

## 4. `session/cancel` (a notification: no id, no reply)

`server.rs:837-849`:

```rust
match method {
    "session/cancel" => {
        if let Some(session) = &srv.session {
            let _ = session.handle.cancel_tx.try_send(());
        }
    }
    _ => debug!(method, "unknown notification"),
}
```

```json
{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"01J8F6Z3H7Q0M2S4T6V8W9XABC"}}
```

The turn then ends with `Done{reason: Cancelled}`, so the open `session/prompt`
is answered with `stopReason: "cancelled"`. Cancel does not withdraw pending
permission asks (`server.rs:840-842`), so the gateway answers them itself with
`cancelled`, which is what ACP asks of a client that cancels a turn
(`schema/v1/client.rs:2440-2447`).

## 5. `session/update`: the one notification stream

Params are `SessionNotification { sessionId, update }`
(`schema/v1/client.rs:55-70`). The `update` object is tagged by `sessionUpdate`
with snake_case variant names (`schema/v1/client.rs:103-105`). Quoted verbatim
from the schema's own serialization test (`schema/rpc.rs:239-256`):

```json
{
    "jsonrpc": "2.0",
    "method": "sessionUpdate",
    "params": {
        "sessionId": "test-456",
        "update": {
            "sessionUpdate": "agent_message_chunk",
            "content": {
                "type": "text",
                "text": "Hello"
            }
        }
    }
}
```

Note the method name in that schema test is the sample's own choice. What maki
sends is `session/update` (`server.rs:1118-1128`).

The gateway forwards the inner `update` object, unwrapped, as
`model.Event{Type:"update", Engine:"acp", Session:<sessionId>, ACP:<update>}`.
CONTRACTS 2 describes the field as "raw ACP session/update params", so this is a
deviation, and a small one: `session` and `event` together carry what `params`
did. The gateway picks the inner object for two reasons. The browser reducer
consumes it directly, because `web/app.js:336` passes `ev.event` to
`applyUpdate`, which switches on `ev.sessionUpdate` (`web/acp.js:80-110`), and
`sessionId` inside the event repeats the `session` field the envelope already
has.

What maki puts on the wire, per variant:

| `sessionUpdate` | built at | shape |
|---|---|---|
| `agent_message_chunk` | `translate.rs:76-80` | `{"sessionUpdate","content":{"type":"text","text":"..."}}` |
| `agent_thought_chunk` | `translate.rs:96-100` | same, thought text |
| `tool_call` | `translate.rs:102-109` (pending), `:85-94` (notice card) | `{"sessionUpdate","toolCallId","title","kind","status","rawInput"?,"locations"?,"content"?}` |
| `tool_call_update` | `translate.rs:111-129` (start), `:301-309` (output), `:311-359` (done) | `{"sessionUpdate","toolCallId", ...flattened fields}` |
| `usage_update` | `translate.rs:379-388` | `{"sessionUpdate","used":N,"size":N,"cost":{"amount":F,"currency":"USD"}?}` |
| `current_mode_update` | `server.rs:796-800` | `{"sessionUpdate","currentModeId":"plan"}` |
| `config_option_update` | `server.rs:220-224` | `{"sessionUpdate","configOptions":[...]}` |

`ToolCallUpdate` flattens its fields next to `toolCallId`
(`schema/v1/tool_call.rs:208-224`, fields at `:261-305`), so `status`, `title`,
`name`, `kind`, `rawInput`, `rawOutput`, `content` and `locations` all sit at the
top of the update object. `status` is one of `pending | in_progress | completed
| failed` (`schema/v1/tool_call.rs:514-525`). A `tool_call` carries the tool name
in `title`, because `translate.rs:102-109` builds it as
`ToolCall::new(id, name)`. The `tool_call_update` that follows replaces `title`
with the summary line maki shows.

`usage_update`, quoted from the schema test (`schema/v1/client.rs:5390-5403`):

```json
{
    "sessionUpdate": "usage_update",
    "used": 53000,
    "size": 200000
}
```

`start_event_pump` never emits `user_message_chunk`: that variant comes from the
history replay of `session/load` (`translate.rs:408-418`), which this engine
never calls. It is still forwarded, because the gateway forwards any
`session/update` it does not know rather than dropping it. The reducer ignores
it, so a replay cannot double a message the gateway appended itself.

## 6. Agent → client requests on the same stdout

Both arrive on the agent's stdout with an `id` this side must echo
(`server.rs:530-547`). A response this side never writes parks a tool forever.

### 6.1 `session/request_permission`

```json
{"jsonrpc":"2.0","id":1000,"method":"session/request_permission","params":{
  "sessionId": "01J8F6Z3H7Q0M2S4T6V8W9XABC",
  "toolCall": {
    "toolCallId": "toolu_01ABC",
    "title": "bash: ls -1",
    "kind": "execute",
    "status": "pending",
    "rawInput": {"command": "ls -1"},
    "locations": [{"path": "/repo/x.sh"}]
  },
  "options": [
    {"optionId": "allow_once",    "name": "Allow once",    "kind": "allow_once"},
    {"optionId": "allow_always",  "name": "Allow always",  "kind": "allow_always"},
    {"optionId": "reject_once",   "name": "Reject once",   "kind": "reject_once"},
    {"optionId": "reject_always", "name": "Reject always", "kind": "reject_always"}
  ]
}}
```

`title` is `"<tool>: <scope(s)>"`, or `"<reason> (<tool>: <scopes>)"` when a
plugin escalated the call (`server.rs:983-1017`). `rawInput`, `locations` and `content` are present
only when maki cached the tool input for this call (`translate.rs:136-160`). The
title always exists. `locations` is optional and can be absent. The four option
ids are fixed (`maki-acp/src/permissions.rs:6-9,17-45`), and only the
`reject_always` label moves with folder trust.

The gateway publishes `Event{Type:"permission", Engine:"acp", Session,
RequestID, Tool:<title>, Input:<toolCall.rawInput>}` and writes, when the user
answers:

```json
{"jsonrpc":"2.0","id":1000,"result":{"outcome":{"outcome":"selected","optionId":"allow_once"}}}
```

Quoted verbatim from the agent's own test of this exact reply
(`server.rs:1369-1374`):

```rust
fn allow_once(id: i64) -> Value {
    serde_json::json!({
        "id": id,
        "result": { "outcome": { "outcome": "selected", "optionId": "allow_once" } },
    })
}
```

The extra `outcome` level is required: `RequestPermissionResponse` has one field
named `outcome` whose value is an enum tagged `outcome`
(`schema/v1/client.rs:2395-2399`, `:2433-2451`). Cancelling writes
`{"outcome":{"outcome":"cancelled"}}` (`server.rs:1377`).

`decision` maps to an option id, and `guidance` is dropped: ACP has no field for
it. `allow` picks `allow_once`, `deny` picks `reject_once`, each falling back to
any option whose `kind` has the same shape. The ids this side offers the browser
(`allow`/`deny`) are the contract's, not the agent's, and `allow_always` and
`reject_always` stay unreachable from the web UI on purpose, because each one
writes a rule nobody asked for.

Any reply the agent cannot read becomes a deny (`server.rs:893-901`), including
an error response, so a written-but-malformed answer fails closed.

`Event.RequestID` is a gateway counter, not the agent's number. CONTRACTS 2
specifies per-engine counters, and two children both start at 1000
(`server.rs:47,59`), so two sessions answer the same number, and echoing it lets
either session answer the dialog of the other. The raw id is kept with the parked ask and echoed on the
wire, which is all the agent matches on (`ask_id`, `server.rs:880-889`).

### 6.2 `elicitation/create`

Built from the `question` tool's input (`server.rs:555-590`,
`maki-acp/src/elicitation.rs:84-111`). `mode` is flattened into the params, so
`mode`, `sessionId`, `toolCallId` and `requestedSchema` sit side by side
(`schema/v1/elicitation.rs:1585-1603`, `:1809-1815`):

```json
{"jsonrpc":"2.0","id":1001,"method":"elicitation/create","params":{
  "mode": "form",
  "sessionId": "01J8F6Z3H7Q0M2S4T6V8W9XABC",
  "toolCallId": "toolu_01ABC",
  "message": "Pick a framework",
  "requestedSchema": {
    "type": "object",
    "properties": {
      "q1": {"type": "string", "title": "Pick a framework",
             "oneOf": [{"const": "axum", "title": "axum - tokio based"}]}
    }
  }
}}
```

`toolCallId` is absent when the ask is not tied to a call. Keys are positional
`q1`, `q2`, ... (`elicitation.rs:78-82`).

MVP: no web form exists, so the gateway answers the cancellation shape and
publishes a notice.

```json
{"jsonrpc":"2.0","id":1001,"result":{"action":"cancel"}}
```

`action` is a flattened tag with variants `accept` / `decline` / `cancel`
(`schema/v1/elicitation.rs:1868-1872`, `:1915-1919`), and the agent turns
anything but `accept` into the same text the Lua tool returns on a dismissal
(`elicitation.rs:126-132`, `DISMISSED` at `:17`):

```rust
if response["action"] != "accept" {
    return DISMISSED.to_owned();
}
```

So `{"action":"cancel"}` ends the park without the model inventing an answer.
The published event is `Event{Type:"notice", Engine:"acp", Session,
Text:"question asked; answer in terminal"}`: the human can still answer in the
child's own terminal, where the TUI-less process has none, so the text says
where the question went. The turn keeps running: the tool returns `DISMISSED`.

### 6.3 Anything else

`fs/read_text_file`, `fs/write_text_file`, and `terminal/*` are answered
`-32601 method not found`, and the agent advertised no client fs capability
beyond `false`, so maki never sends them. An unknown method gets the same reply
rather than silence, because silence parks the caller.

## 7. `session/set_mode`

`server.rs:785-804`, and the params are `SetSessionModeRequest { sessionId,
modeId }`
(`schema/v1/agent.rs:3036-3043`).

```json
{"jsonrpc":"2.0","id":4,"method":"session/set_mode","params":{"sessionId":"01J8F6Z3H7Q0M2S4T6V8W9XABC","modeId":"plan"}}
```

```json
{"jsonrpc":"2.0","id":4,"result":{}}
```

`modeId` is `build` or `plan`. Anything else answers `-32602` with
`unknown mode: <x>` (`server.rs:788-789`). The agent also pushes a
`current_mode_update` (§5) before the reply. The gateway refuses a mode the
agent did not list in `session/new` rather than waiting for `-32602`.

ACP names a `session/set_title` method (`schema/v1/agent.rs:6310`), but maki
dispatches no such thing (`server.rs:267-285`), so `SetTitle` answers
`model.ErrUnsupported` (501) and a title only ever arrives from the agent as a
`session_info_update`, which this maki does not send either.

## 8. `session/set_config_option` (model switch)

`server.rs:806-835`, and `SetSessionConfigOptionRequest { sessionId, configId }`
plus a flattened `value` (`schema/v1/agent.rs:3593-3606`), which serializes as a
plain `value` string for a select option
(`SessionConfigOptionValue::ValueId`, `schema/v1/agent.rs:3512-3532`).

```json
{"jsonrpc":"2.0","id":5,"method":"session/set_config_option","params":{
  "sessionId":"01J8F6Z3H7Q0M2S4T6V8W9XABC","configId":"model","value":"zai/glm-4.6"
}}
```

Result echoes the option list (`schema/v1/agent.rs:3654-3658`):
`{"configOptions":[{"id":"model","name":"Model","type":"select","currentValue":"zai/glm-4.6","options":[...]}]}`.

Unknown `configId` or a value the policy rejects answers `-32602` with the
reason as `data`. CONTRACTS 2 has no route for a model change yet, so this is
`Engine.SetModel`, not part of `model.Engine`.

## 9. Error codes

From `schema/v1/error.rs:202-213` and the maki sites.

| code | name | maki sends it for | gateway reads it as |
|---|---|---|---|
| -32700 | parse error | bad JSON on stdin (`server.rs:237`, id `null`) | logged |
| -32600 | invalid request | `no active session` (`server.rs:1130-1132`) | `model.ErrNotFound` |
| -32601 | method not found | any method outside §1-8 (`server.rs:284`) | the wire error, 500 |
| -32602 | invalid params | unknown mode, bad config option (`server.rs:789,808-819`) | the wire error, 500 |
| -32603 | internal error | `a prompt is already running` (`server.rs:774`), `session ended` (`:780,826`), a failed turn (`:1092`) | `model.ErrBusy` when the message contains `already running`, else 500 |
| -32000 | `SESSION_BUSY_CODE` (`server.rs:55`) = the schema's `auth_required` | a session another maki holds, from `load_error` (`server.rs:736-745`) | `model.ErrOpenElsewhere` |
| -32002 | resource not found | `session/load` of an id with no file (`server.rs:331,742`) | the wire error |

`SESSION_BUSY_CODE` is the value CONTRACTS 2 needs for the 409 "open elsewhere"
reply. It collides with `auth_required`, which only a turn answer uses. §3 says
how the gateway handles that.

`codeGone` is not a wire code. It labels only the reply `process.go` builds
itself for a request whose child died before it could answer. It maps to
`model.ErrNotFound`, and `Prompt` reads that same marker as "the frame went out,
and the child died after it", so it still answers 200 and lets the `exit` event
report the death.

## 10. Process lifecycle, and what the fake must copy

- Spawn: `<makiPath> acp`, plus `-m <model>` when a model was asked for, and
  never `--yolo`. The web UI answers permission prompts, and auto-allow takes
  that choice away from the user (`src/cli.rs:428-435` names both flags).
- The child starts with no session. `initialize` then `session/new` run once, in
  that order, inside `Create`, so `Create` returns a usable session id.
- stdin stays open for the process's life. `Kill` closes it, waits 3 s, then
  sends `SIGTERM`. maki exits on stdin EOF (`server.rs:145-163`), so the close is
  the graceful path and the signal is the backstop.
- stdout EOF or a decode failure is death: every open request is failed, parked
  asks are dropped with `permission_cancel`, and `exit` is published with the
  wait status. There is no ping, no restart, and no liveness probe.
- `Kill` withdraws the parked asks the same way `Cancel` does, with a
  `cancelled` answer and a `permission_cancel` event, before it closes stdin. A
  dialog left open on a killed session has no answer that can still arrive.
- stderr is forwarded to `slog` at debug, one line per call, prefixed with pid
  and session.

## 11. Transcript reducer, and its limits

`transcript.go` folds the same stream the browser folds, so
`GET /api/sessions/{id}/messages?source=acp` can answer after a refresh without a
second process. It is UI fuel, not a history: maki still writes the real record
to `~/.local/state/maki/sessions/<id>.jsonl`, and the store engine reads that.

- `Prompt` appends the user message the gateway is sending, as
  `{"role":"user","content":[{"type":"text","text":...}]}`. ACP never echoes
  `user_message_chunk` back, so nothing else can add it.
- `agent_message_chunk` / `agent_thought_chunk` append to the last text /
  thinking block of the current assistant message, or open that block, or open
  the assistant message. So chunks of one type join, and a type change splits.
- `tool_call` opens a `{"type":"tool_use","id","name","input"}` block, `name`
  from `name` then `title`, `input` from `rawInput`. A `tool_call_update` naming
  the same id refreshes `name` when it carries a `name` or `title`, matching
  `web/acp.js:33-48`.
- A `tool_call_update` with `status` `completed` or `failed` appends
  `{"type":"tool_result","tool_use_id","content","is_error"}` once per call id.
  `content` is `rawOutput` when it is a string, `rawOutput` as JSON text when it
  is anything else, and the update's text content blocks when `rawOutput` is
  absent.
- Everything else passes through as an `update` event and is not reduced.

Known limits, none of them fixable from this side:

1. The gateway never sees the prompt the model saw, only what the web UI sent,
   so a turn started in the terminal of the same session is missing from the
   cache. ACP gives no way to read history back, because `session/load` needs a
   fresh child, and a fresh child cannot share the session this one holds.
2. The agent's `sessionId` is maki's own id, so the cached transcript lines up
   with the stored file but is not it. Two views of one session can differ for
   as long as the process lives: web rows win in the merge (CONTRACTS 2).
3. Text blocks have no timestamps, `display_text` or `kind`, so a refetch after
   a restart returns less than the file holds. The store engine answers those.
4. `usage_update`, `current_mode_update` and `config_option_update` reach the
   browser only as events, and a notice card keeps no body. A refresh loses them,
   as it does for the browser's own copy.
5. The cache is per process and dies with `Kill`, which is the only way it is
   freed. `Messages` on a dead session with an empty cache answers
   `model.ErrNotFound`, so the caller falls back to the file.
