# maki-gateway wire contracts (v1)

Binding spec for all components. If you must deviate, keep the field names and change nothing
the other side parses. Three surfaces: (1) socket protocol between the Lua plugin and the
gateway, (2) HTTP/SSE API between gateway and browser, (3) on-disk reads. Shared Go types live
in `internal/model/types.go`; do not change its signatures silently.

## 1. Attach socket protocol (maki Lua plugin <-> gateway)

Transport: unix socket, newline-delimited JSON. No other framing. Plugin path:
`$MAKI_GATEWAY_SOCK` if set, else `$XDG_STATE_HOME/maki/gateway/pid-<pid>.sock` (default state
home `~/.local/state/maki`). Socket mode 0600. Gateway discovery: scan that dir, connect each,
send `hello`. Both sides buffer partial `read()` chunks and split on `\n`; max line 8 MiB,
oversize closes the connection.

Every request: `{"id": <int>, "op": "<name>", ...}`. Exactly one response per id:
`{"id": N, "ok": <value>}` or `{"id": N, "error": "msg"}`. A response MUST carry exactly one of
`ok` / `error`. A frame with neither (a bare `{"id": N}`) is a protocol violation: the gateway
fails that request with an error that names the protocol and closes the connection. It never reads
such a frame as a success with an empty payload, so a handler has no success answer that carries no
value. A connection that sent `hello` is subscribed to async events for the plugin's lifetime.
Events: `{"event": "<name>", ...}`, never carry `id`.

### ops

| op | args | ok value | notes |
|---|---|---|---|
| hello | — | `{"pid":int,"plugin":"1","cwd":str,"sessions":int}` | also first call; resubscribes |
| ping | — | `"pong"` | liveness, 15 s interval from gateway |
| list | — | `[{"id":str,"title":str,"status":"working"\|"needs_input"\|"idle","cwd":str,"model":str,"focused":bool}]` | live TUI tabs only |
| messages | `session?`, `last:int` | `{"messages":[Message]}` | Message = native maki JSON (see 3); always pass session+last |
| prompt | `session`, `text` | `"started"` \| `"queued"` | error `"session not live: <id>"` |
| new | `prompt?`, `cwd?`, `yolo?` | `{"session":id}` | opens background TUI tab, omit focus. The gateway copies `yolo:true` from a yolo create, and the plugin ignores it: a tab of a running TUI cannot skip the prompts its process was started with |
| set_mode | `session`, `mode:"build"\|"plan"` | `true` | |
| set_title | `session`, `title:\|null` | `true` | null clears |
| notify | `text`, `session?` | `true` | |
| plan | `session` | `{"mode":str,"path":str,"ready":bool,"content":str?}` | |
| tasks | — | `[{"id":str,"session":id,"name":str,"focused":bool,"status"?:str,...}]` | passthrough of `maki.task.list()`. The host lists the chats of the focused session only and takes no session argument, so the rows are whatever that TUI has on screen. Row 1 is the main chat, with `id:"main"` and no `status`. `status` is one of `"working"`, `"idle"`, `"done"`, `"error"`, `"cancelled"`. The optional fields stay away rather than arrive null: `tool` for the tool whose call runs (`"task"` or `"bash"`), `parent` for the call it runs under, `part`:bool, `wrapper`:bool, `plugin` for a task a plugin opened, `started_at` and `ended_at` in Unix seconds, `cost` in dollars |
| run_command | `cmdline` | `<result>` | plugin MUST reject cmdline starting with `/reload` |
| pop_queue | `session?` | `true` | removes first queued input of focused tab |
| permissions | `on:bool` | `true` | toggles the permission slot bridge |
| respond | `request_id:int`, `decision:"allow"\|"deny"`, `guidance?` | `true` | answers a `permission` event. The two verdicts are all the slot wrapper can give, so the gateway answers `allow_always` as `allow` (see 2) |

### events (plugin -> gateway)

| event | fields | source |
|---|---|---|
| status | `session,status,title,focused` | SessionStatusChanged |
| turn_start | `session,text` | TurnStart |
| tool_start | `session,tool_id,tool` | ToolStart |
| tool_done | `session,tool_id,tool,is_error,bytes,duration_ms?` | ToolDone |
| turn_end | `session,reason,usage,cost,context_size,num_turns` | TurnEnd |
| title | `session,title` | SessionTitleChanged |
| model | `session,model` | ModelChanged |
| question | `session` | raised when a tab enters needs_input and the question tool is the cause (best effort; may be a plain input wait) |
| permission | `request_id,session,tool,input` | only while permissions bridge on |
| permission_cancel | `request_id` | bridge parked >=55 s and fell through to the terminal prompt |
| tasks_changed | `session` | TaskStatusChanged, TaskStopped. One event per task that started, moved or stopped. It names no task, because the browser reads the rows with the `tasks` op. `session` is the tab the task belongs to, which is not always the focused one the `tasks` op answers |

Permission bridge semantics: the `permission.prompt` slot wrapper pushes a `permission` event to
all connections, then polls a pending-answers table every 500 ms (sleep yields, never holds the
VM). Answer `allow` -> `{decision="allow"}`, `deny` -> `{decision="deny",guidance=...}`. At 55 s
with no answer, call `prev(req, ctx)` (fall through to the human prompt) and emit
`permission_cancel`. `respond` for an unknown/expired id replies `error:"expired"`. The host slot
has no "remember this" answer, so an `allow_always` decision from the browser arrives here as
`allow` and the live tab asks again on the next call.

## 2. HTTP API (gateway <-> browser)

All JSON. Base = the bind addr. Static UI served from `/` (files in `web/`).

A token bind (a non-loopback address with `MAKI_GATEWAY_TOKEN` set) accepts three credentials:
`?token=`, `Authorization: Bearer`, and the `maki_gw` cookie. Static assets authenticate by that
cookie, because the browser carries neither of the other two forms on the `/app.js` and
`/style.css` requests the page makes. The gateway sets the cookie (`HttpOnly`, `SameSite=Strict`,
`Path=/`) on the first request that passes on a URL or header credential, so loading the document
seeds the session. A loopback bind needs no credential and never sets a cookie.

| method path | request | response |
|---|---|---|
| GET /api/health | — | `{"version":str,"maki":str,"sessions":int}` |
| GET /api/sessions | — | `[Session]` (model.Session JSON, below) |
| POST /api/sessions | `{"cwd":str,"model"?:str,"prompt"?:str,"yolo"?:bool}` | 201 `{"session":id}` — spawns `maki acp`. Missing or empty `cwd` is 400 `{"error":"cwd is required"}`: the gateway refuses to spawn in its own directory. `yolo:true` spawns `maki acp --yolo`, so the session answers every permission prompt without asking. The flag belongs to the `acp` subcommand, and maki reads the global flag of the same name on other run paths only, so it goes after `acp`. A live session cannot be yolo: the bridge reports the request to the plugin and the plugin ignores it |
| DELETE /api/sessions/{id} | — | `{"killed":bool}` — kills a **web** process only; 400 for live/stored |
| GET /api/sessions/{id}/messages?last=200 | — | `{"source":"acp"\|"attach"\|"file","messages":[Message]}` |
| POST /api/sessions/{id}/prompt | `{"text":str}` | `{"state":"started"\|"queued"}`; 409 `{"error":"busy"}` when a web session is mid-prompt; 409 `"open elsewhere"` on claim clash |
| POST /api/sessions/{id}/cancel | — | `{"cancelled":bool}`; 501 for live sessions (no plugin API exists yet), 404 for stored ones (nothing runs to stop) |
| POST /api/sessions/{id}/mode | `{"mode":"build"\|"plan"}` | `{"ok":true}` |
| POST /api/sessions/{id}/yolo | `{"on":bool}` | `{"yolo":bool}`, the answer now in force. Only kind `web` can change it. maki reads `--yolo` once, when the child starts, so no running child can change its mind: the gateway stops that child, starts a new one with or without the flag, and makes it load the same session id, which replays the stored history and continues the chat. A row that reads `stored` because its child died is revived the same way, as long as this gateway still holds the session. 409 `{"error":"busy"}` while a prompt is in flight: the toggle refuses, and that turn keeps running. 404 for an id no engine reports. Kind `live` and kind `stored` answer 501 `{"error":"yolo is a terminal-side toggle for attached sessions"}`. A body with no `"on"` bool is 400. What the toggle costs: every permission dialog the browser still has open is withdrawn (`permission_cancel`), any background work the old child ran dies with it, and the new child starts with the model the session was created with, so a model change made afterwards is undone. The row's `yolo` field and its `status` reach the browser through the `sessions` event, so no new SSE type exists for this. |
| POST /api/sessions/{id}/title | `{"title":str\|null}` | `{"ok":true}`. Rename is live-only: kind `web` answers 501 (`maki-acp` dispatches no `session/set_title`), kind `stored` answers 405 |
| POST /api/permissions/{engine}/{request_id} | `{"decision":"allow"\|"allow_always"\|"deny","guidance"?:str}` | `{"ok":true}`, 404 expired, 400 for any other decision. `{engine}` is `"acp"` or `"attach"`, and request ids are per-engine counters. `allow` answers `allow_once` on the ACP wire, `allow_always` answers `allow_always`, and an agent that offers no "always" option gets `allow_once`. The attach bridge has no "always" verdict to give, so `allow_always` arrives as `allow` and only the web engine remembers the answer |
| GET /api/sessions/{id}/tasks | — | `{"tasks":[TaskInfo]}`, the rows of the socket `tasks` op with the field names of section 1. 501 for kind `web` and kind `stored`, since only a running TUI has tasks. 404 for an id no engine reports |
| GET /api/plan/{id} | — | plan object as in socket `plan` op. 501 for web sessions and for stored sessions, since only the attach bridge can read a plan |
| GET /api/search?q= | — | `{"hits":[{"session":id,"snippet":str,"title":str}]}` (substring scan of jsonl, cap 50) |
| GET /api/analytics?days=30 | — | `{"daily":[{"date":"YYYY-MM-DD","sessions":int,"input":int,"output":int,"cache_read":int,"cache_creation":int,"cost":num}],"per_session":[{"session":id,"title":str,"model":str,"cost":num,"updated_at":int}]}` |
| GET /api/events | — | SSE stream, see below |

### SSE envelope

`event: maki` + `data: <json>`, one JSON object: `{"type":str, ...}`.

| type | fields | meaning |
|---|---|---|
| sessions | — | list changed, refetch GET /api/sessions. Only a visible change counts: a row whose `updated_at` moved and nothing else raises nothing |
| update | `session,engine:"acp",event:<raw ACP session/update params>` | web session live stream (chunks, tool cards, usage) |
| dirty | `session,reason:"turn_end"\|"tool_done"\|"tool_start"\|"status"` | attach session changed: refetch its messages (cheap; no delta events exist in maki) |
| status | `session` plus any of `status?`, `title?`, `focused?`, `engine?` | one session changed. Every field but `session` is optional and appears only when the source knew it: a plugin tab reports `status,title,focused`, a rename sends `title` alone, a model change sends `session` alone, and a web session sends `status,title,engine` with no `focused`. Patch the fields that are present and refetch the list |
| permission | `request_id,session,tool,input,engine:"acp"\|"attach"` | show dialog; answer via POST /api/permissions |
| permission_cancel | `request_id` | dialog withdrawn (fell through / answered elsewhere) |
| tasks | `session` | a task of a live session started, moved or stopped: refetch GET /api/sessions/{id}/tasks |
| notice | `text` | toast (process exit, plugin connect/disconnect) |
| exit | `session,code:int\|null` | web process terminated |

Browser never opens a WebSocket. POST + SSE only (proxy-safe, mobile-safe).

### Search limitations (known in v1)

`GET /api/search` reads the stored engine, and that engine scans raw jsonl lines. Two results
follow from that. A hit can come from a record no transcript view shows, such as a `frame` or
`meta` line or a tool result the UI folds away, so a snippet can hold JSON the browser never
renders. And one hit per session means the 50 hit cap fills in log-name order rather than by
recency: a query that matches more than 50 logs answers the same 50 sessions every time, and a
session created after the last of those names can be missing. There is no offset or scope
parameter. Only when no stored engine is registered does the gateway fall back to a transcript
scan (message text only, last 200 messages per session).

### model.Session JSON

`{"id":str,"kind":"web"|"live"|"stored","title":str,"status":"working"|"needs_input"|"idle"|"exited","cwd":str,"model":str,"updated_at":int,"plugin_pid":int?,"yolo":bool?}`

`live` entries exist only while the plugin socket lives. A session id present in both stored and
live lists appears once, `kind:"live"`. Web sessions are also stored (maki persists them); while
the process lives they appear as `kind:"web"`, and they stay `kind:"web"` for the moment a yolo
toggle replaces that process.

`yolo` is the permission answer the web child was started with. Only the acp engine reports it,
so the field is absent on every other row: absent means false, and an attached tab never tells
the gateway what mode its terminal chose.

## 3. Message JSON (native maki shape; what UI renders)

One line of `~/.local/state/maki/sessions/<id>.jsonl` record `{"t":"msg","d":<Message>}`.
`GET .../messages` returns the `d` objects:

`{"role":"user"|"assistant","content":[Block],"display_text"?:str,"created_at"?:int(ms),"kind"?:str}`

Blocks (tagged by `type`): `{"type":"text","text"}`, `{"type":"tool_use","id","name","input"}`,
`{"type":"tool_result","tool_use_id","content","is_error"}`, `{"type":"thinking","thinking"}`,
`{"type":"image",...}` (render placeholder). Unknown block types: show raw JSON in a details box.

File rules (hard-won, do not violate): read-only, take NO lock; consume only `\n`-terminated
lines; a rewrite can shrink the file so never seek by stored offset across reads; header line 1
is FLAT `{"t":"header","v":..,"id":..,"model":..,"cwd":..,"created_at":..}` (no `d` wrapper —
only msg/out/sub_msg/frame use `d`; readers accept both shapes); trailing `meta` record is FLAT
too: `{"t":"meta","title":..,"token_usage":{..},"updated_at":..,"usage_by_model":{model:{input,
output,cache_creation,cache_read,cost}},...}` latest-wins, analytics source (tail window 32 KiB,
grow to 1 MiB when no complete meta found); `maki session list --json` / `export <id>` /
`delete <id> --force` are the CLI surface (list writes a scan cache side effect, cwd filter is
exact-string).

Cost is optional in the shape above, and maki 0.7.1 leaves it out more often than the shape
suggests. `usage_by_model` only appears once a turn has settled, so a session that never finished
one carries `token_usage` alone. Each `cost` inside `usage_by_model` is written only for a model
with a published price: a local or self-hosted model has none, so the key stays away and analytics
reports its tokens with a zero cost. Treat the `token_usage` fallback as the normal path, not as
the legacy one. A future maki that prices more models fills both fields, and the same reader code
covers it.

### Attach error substrings (the Lua plugin MUST keep these words)

`"session not live"`, `"expired"`, `"unknown"`, `"no such"` → 404 · `"another"` / `"elsewhere"`
→ 409 open elsewhere · `"busy"` → 409 busy. The `notLive`, `expired` and `openElsewhere` consts in
`internal/attach/attach.go` hold the same words, and `MapError` does the mapping. The attach engine
sends `{"op":"permissions", "on":true}` automatically after `hello`. `list` rows MAY carry
`updated_at` (int seconds; the engine decodes it when present; live rows otherwise show 0, and the
gateway does not treat that field as a list change). `focused` is not part of model.Session; it
travels only on `status` events.

ACP update passthrough (`engine:"acp"`) uses ACP shapes: `sessionUpdate` one of
`agent_message_chunk|agent_thought_chunk|tool_call|tool_call_update|usage_update|current_mode_update|config_option_update`,
content in `content`/`toolCallId`/`rawInput`/`status` fields. The browser keeps its own reducer
from ACP updates + Message shape; see `web/app.js` header comment.

## 4. Engine -> gateway internal contract (Go)

`internal/model.Engine` interface in types.go. Events reach the hub via
`model.Hub.Publish(model.Event)`. The ACP engine caches a normalized transcript per session
(built from its own prompts + streamed updates) so `source:"acp"` messages work after refresh.
The attach engine fans requests to the right plugin by `plugin_pid`. The store engine answers
`stored` sessions and analytics. All three report through `List()`, and
`internal/registry` merges + dedupes and emits `sessions` SSE events on change.

## 5. Testing fakes

- `maki acp` is fakeable: tests use `-maki <path>` pointing at a script/binary that speaks the
  subset in appendix A of `docs/ACP_MIN.md` (owned by the acpengine package).
- The socket protocol is fakeable with a Go test listener per section 1.
- Never hand-edit `maki.d.luau` or maki sources. The plugin is a maki pack: the
  manifest is the `plugin.toml` at the repository root, `plugin/gateway.lua` is the
  entry point maki auto-runs, and the module files stay in `lua/gateway/`.
