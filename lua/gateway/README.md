# gateway

A maki Lua plugin that lets an external gateway attach to a maki TUI that is already
running. It opens a unix socket and speaks newline-delimited JSON-RPC 2.0 on it.

The wire format is [docs/WIRE.md](../../docs/WIRE.md), the v2 attach protocol. That
file is the binding spec: method names, params, result shapes, error codes, the
fixed message words, and the version gate. This plugin is the ACP agent side of it,
and maki-gateway is the client. The plugin answers the ACP subset of WIRE plus the
maki extension methods below, and it pushes the notifications of WIRE 11.

Through the socket a gateway can list the tabs, read a transcript as ACP updates or
as native maki records, start a turn and wait for it, change mode and title, steer
the input queue, read the plan, list and follow tasks, run a slash command, and
answer tool permission prompts.

## Install

Add one line to `~/.config/maki/init.lua`, then restart maki:

```lua
maki.pack.add({ "file:///path/to/maki-gateway" })   -- or the pushed https URL
```

Maki clones the repository, asks you twice at startup, and loads the plugin:

```
Install these packages?
  maki-gateway from file:///path/to/maki-gateway
Allow package maki-gateway these permissions: run, fs_read, fs_write, env,
agent, session_read, session_write, session_settings?
```

Answer `y` to both. The second answer is what grants the permissions in
`plugin.toml`: a fetched pack gets the manifest request cut down by what you
approved, so nothing runs before you say yes. Approvals are stored, so the
prompts come back only when the pack asks for something new or you clear them.

`maki.pack.add` works only from the global `~/.config/maki/init.lua`, and only at
startup. Both prompts need a real terminal, so run `maki` yourself in one. Under
`maki -p` the pack is skipped with a note that installation needs confirmation.

The pack is the whole repository. Maki reads only `plugin.toml`, `plugin/`, and
`lua/` from it, so the Go source, the web files, and the docs cost nothing but
clone time. `plugin/gateway.lua` is the entry point maki auto-runs, and it is one
`require("gateway")`, which finds `lua/gateway/init.lua`. The comments in
`plugin.toml` say which call needs which grant. The checkout lands under
`~/.local/share/maki/site/pack/core/maki-gateway/<rev>/`, and your answer to the
second prompt is stored in `~/.local/share/maki/site/pack-approvals.json`.

To pin a release, or to rename the directory it installs under, pass a table
instead of a string: `maki.pack.add({ { src = "...", version = "v1.0.0", name =
"maki-gateway" } })`.

After the restart, or after `/reload` in a session that is already up, the plugin
logs one line when the socket is open:

```bash
grep 'control socket' ~/.local/logs/maki/*.log
```

Power users who would rather not have maki manage a checkout can copy this
directory to `~/.config/maki/lua/gateway`, add `require("gateway")` to
`~/.config/maki/init.lua`, and copy the `[permissions]` table from `plugin.toml`
into `~/.config/maki/plugin.toml`. Maki reads grants for a hand-placed plugin
from the manifest of the config dir that loads it, and merging into a manifest you
already have keeps your other grants. For a project-only install use
`<project>/.maki/` in place of `~/.config/maki/` in every step, and trust that
folder so its plugins load. With no manifest at all every gated call is denied. A
call without its grant fails with `permission denied: '<name>' not granted for
this plugin`, and the plugin logs it.

## Socket path

WIRE 1.1 fixes this, and it did not change from v1:

- `MAKI_GATEWAY_SOCK`, when set, is the path. Use an absolute path. One fixed name
  shared by two maki processes means the later one takes it over.
- Otherwise `XDG_STATE_HOME` if set, else `~/.local/state`, then
  `maki/gateway/pid-<pid>.sock`.

The pid in the name keeps one socket per maki process. The gateway finds every
running maki by listing that directory:

```bash
ls ~/.local/state/maki/gateway/
# pid-41123.sock
```

The plugin removes a stale socket file left by a crashed maki before it binds, and
asks for mode 0600 on the socket and 0700 on that directory.

## Methods

ACP names carry tabs, history, turns, mode, title, and permission asks. The maki
extensions below carry what ACP has no name for. The envelope is always a
JSON-RPC 2.0 request, and answers use `result` or `error`, never both.

| method | params | answer | WIRE |
|---|---|---|---|
| `_sh.maki/messages` | `sessionId`, `last`? | `{"messages":[Message]}`, the native record as the host holds it | 6 |
| `_sh.maki/queue` | `sessionId`, `text` | `{"state":"started"\|"queued"}` | 7.2 |
| `_sh.maki/pop_queue` | none | `true` | 7.2 |
| `_sh.maki/notify` | `text`, `sessionId`? | `true` | 9.1 |
| `_sh.maki/plan` | `sessionId`? | `{"mode","path","ready","content"?}` | 9.2 |
| `_sh.maki/tasks` | none | the task rows, as a bare array | 9.3 |
| `_sh.maki/task_focus` | `taskId` | `true` | 9.4 |
| `_sh.maki/run_command` | `cmdline` | `true` | 9.5 |

The plugin also answers `initialize`, `session/list`, `session/load`,
`session/resume`, `session/prompt`, `session/set_mode`, and `session/set_title`,
and refuses `session/cancel` with `-32601` on purpose (WIRE 7.3). It advertises
the extension names above under `sh.maki/methods` in its `initialize` `_meta`,
because ACP gives a client no other way to find them.

## Try it by hand

With `socat`. Every line is one request, and answers and notifications arrive on
the same connection. `initialize` must be the first request, because every other
method answers `-32600` `initialize first` until the handshake is done.

```bash
socat - UNIX-CONNECT:"$HOME/.local/state/maki/gateway/pid-41123.sock"
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}
{"jsonrpc":"2.0","id":2,"method":"session/list","params":{}}
{"jsonrpc":"2.0","id":3,"method":"_sh.maki/messages","params":{"sessionId":"<id>","last":20}}
{"jsonrpc":"2.0","id":4,"method":"_sh.maki/queue","params":{"sessionId":"<id>","text":"use table-driven tests"}}
```

Type slowly. A prompt answer arrives when its turn ends, which can be minutes, and
notifications for every tab arrive while you think.

With Go, saved as `attach.go` and run as `go run attach.go <socket-path>`. It uses
the same framing the gateway uses, so you can watch the handshake and one list:

```go
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
)

func main() {
	conn, err := net.Dial("unix", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/list","params":{}}`,
	}
	for _, line := range lines {
		fmt.Fprintln(conn, line)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 8<<20), 8<<20)
	for i := 0; i < 6 && scanner.Scan(); i++ {
		fmt.Println(scanner.Text())
	}
}
```

## A host with no terminal

The plugin needs a TUI. Under `maki -p`, or under `maki acp`, session calls answer
`no interactive UI attached`, and the methods that need a tab fail. A gateway that
starts maki itself, over ssh or from a service manager, must give it a real pseudo
terminal. `tmux` is the smallest way:

```bash
tmux new-session -d -s maki-attach 'cd ~/src/project && exec maki'
# The socket appears a moment later. Take the pid from the file name:
sock=$(ls -t ~/.local/state/maki/gateway/*.sock | head -1)
socat - "UNIX-CONNECT:$sock"
```

`tmux attach -t maki-attach` shows the same screen the plugin is driving. Keep one
session per project, because tabs belong to the maki process that opened them.

## Notifications out

The plugin pushes both kinds of notification to every connection that finished the
handshake, for every tab of this process. A connection does not have to load or
resume a tab to hear about it.

- `session/update` (WIRE 11.1): `tool_call` when a tool starts, `tool_call_update`
  when it ends, `usage_update` and `current_mode_update` as they move.
- `_sh.maki/session_event` (WIRE 11.2): `kind` `status`, `title`, `model`,
  `question`, `tasks_changed`, and `task_focus`. A `task_focus` event names the row
  of `_sh.maki/tasks` now on the screen in `taskId`. `sessionId` is present on every
  kind, and it is not always the tab on screen.
- `session/request_permission` (WIRE 10) as a request this side sends, when a tool
  call would show a permission prompt.

Maki reports no streamed model text to plugins, so a live tab pushes no
`agent_message_chunk` and no `agent_thought_chunk`. Those two variants appear only
in the replay of `session/load`. A turn shows up as tool cards, the closing
`usage_update`, the tab moving to `idle`, and the answer to its own prompt.

## Permission asks

The bridge is on while at least one connection has finished the handshake, and the
plugin parks nothing when there is no connection (WIRE 10.4). The ask offers
exactly `allow_once` and `reject_once`, because the host slot has those two answers
and no "remember this". `allow` and `allow_always` from a browser both arrive as
`allow_once`, and the tab asks again on the next call.

A `deny` answer can carry free text under `result._meta["sh.maki/guidance"]`, and
the plugin passes it to the model as guidance. An `optionId` the plugin did not
offer, an `error`, a result it cannot read, and a frame with neither are all a deny
with no words. A `cancelled` outcome is a deny too, which is what `maki acp` does
with the same answer.

After 55 seconds with no answer the plugin answers its own ask with `cancelled` on
every connection, so each dialog closes, and then the terminal prompt takes the
call. With two gateways on one socket the first valid answer wins, and the
connections that had not answered get the same `cancelled` answer (WIRE 10.3).

## Notes on behaviour

- One request gets one answer, and that answer carries exactly one of `result` and
  `error`. A frame with an `id` and neither, a line that is not one JSON object, and
  a frame over 8 MiB each end the connection (WIRE 1.2, 1.3).
- An answer that would not fit one line comes back as an error with the same `id`,
  so a caller reads a refusal rather than waiting. Lower `last` on
  `_sh.maki/messages` when that happens.
- `session/prompt` answers at the end of the turn, and it has no timeout, because a
  long turn is legal. A second prompt for a tab that is already `working` answers
  `-32603` with the fixed busy sentence, and the text never reaches the host queue.
  Use `_sh.maki/queue` to put text in front of a running turn (WIRE 7.1, 7.2).
- `session/load` replays stored history to the connection that asked, then answers.
  A block longer than one frame arrives in chunks, and a reader joins chunks that
  carry the same kind. `session/resume` answers with no replay.
- `session/list` is the only way to learn that a tab opened or closed, and the
  gateway polls it every 5 seconds as its liveness clock (WIRE 1.5).
- `session/set_title` of `""` clears the name, and the tab falls back to the title
  the host generated. v1 used `null` for that, and v2 does not accept it.
- `_sh.maki/pop_queue` takes no argument. It acts on the tab the terminal has
  focused, because the host has no call for another queue.
- `_sh.maki/task_focus` takes `taskId`. WIRE 12 says a renamed argument is not
  aliased, so the v1 `id` fallback is gone and a frame that names no task is
  refused.
- `_sh.maki/run_command` rejects a `/reload` command line. It would rebuild the
  plugin host and take this plugin, its listener, and every open connection with it.
- `sh.maki/maki_version` in the handshake is the answer of `maki --version`. Maki
  reports no version to plugins through the Lua API, so the plugin asks its own
  binary once at startup, in the same way the gateway's health route does, and
  answers `unknown` when that probe fails.
- The socket closes when the plugin unloads or maki exits, and the file goes with
  it. After a crash the name can stay, and the next start removes it.
