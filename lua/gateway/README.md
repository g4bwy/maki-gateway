# gateway

A maki Lua plugin that lets an external gateway attach to a **running** maki TUI.
It opens a unix socket and speaks newline-delimited JSON on it. The wire format is
`../../docs/CONTRACTS.md` section 1. The gateway process owns the browser side. This
plugin only serves that socket.

Through the socket a gateway can list the tabs, read a transcript, send a prompt, open a
background tab, switch mode, rename, notify, read the plan, list tasks, run a slash
command, pop the input queue, and answer tool permission prompts. Every state change in
any tab comes back out as an event line.

## Install

The plugin is one directory. Put it in your maki config dir, load it, and grant the
permissions.

1. Copy or link the directory:

   ```bash
   mkdir -p ~/.config/maki/lua
   cp -r lua/gateway ~/.config/maki/lua/gateway
   # or keep it in a checkout and link it:
   ln -s /path/to/maki-gateway/lua/gateway ~/.config/maki/lua/gateway
   ```

2. Load it. Add one line to `~/.config/maki/init.lua`, creating the file if you have none:

   ```lua
   require("gateway")
   ```

   Nothing under `lua/` loads on its own. `require("gateway")` finds `lua/gateway/init.lua`.

3. Grant the permissions. Maki reads grants for your own Lua from the manifest in the
   config dir that loads it, so the keys in `plugin.toml` next to this README belong in
   `~/.config/maki/plugin.toml`. Merge them into a file you already have, or copy this one
   in place when you have none:

   ```toml
   [permissions]
   run = true
   fs_read = true
   fs_write = true
   env = true
   agent = true
   session_read = true
   session_write = true
   session_settings = true
   ```

   `run` opens the socket. `agent` sends prompts to the model. `session_read` reads
   transcripts and event content. `session_write` creates sessions and pops the queue.
   `session_settings` changes mode and title. A call without its grant fails with
   `permission denied: '<name>' not granted for this plugin`, and the plugin logs it.

4. Start maki, or run `/reload` in a session that is already up. The plugin logs one line
   when the socket is open:

   ```bash
   grep 'control socket' ~/.local/logs/maki/*.log
   ```

For a project-only install use `<project>/.maki/` in place of `~/.config/maki/` in every
step, and trust that folder so its plugins load.

## Socket path

- `MAKI_GATEWAY_SOCK`, when set, is the path. Use an absolute path. One fixed name shared
  by two maki processes means the later one takes it over.
- Otherwise `XDG_STATE_HOME` if set, else `~/.local/state`, then
  `maki/gateway/pid-<pid>.sock`.

The pid in the name keeps one socket per maki process. The gateway finds every running maki
by listing that directory:

```bash
ls ~/.local/state/maki/gateway/
# pid-41123.sock
```

The plugin removes a stale socket file left by a crashed maki before it binds, and asks for
mode 0600 on the socket and 0700 on that directory.

## Try it by hand

With `socat`. Every line is one request, and answers and events arrive on the same
connection:

```bash
socat - UNIX-CONNECT:"$HOME/.local/state/maki/gateway/pid-41123.sock"
{"id":1,"op":"hello"}
{"id":2,"op":"list"}
{"id":3,"op":"messages","session":"<id>","last":20}
{"id":4,"op":"permissions","on":true}
```

Type slowly, or events for a busy session arrive while you think. `hello` is what subscribes
the connection, so send it first.

With Go, saved as `attach.go` and run as `go run attach.go <socket-path>`:

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
	for _, line := range []string{`{"id":1,"op":"hello"}`, `{"id":2,"op":"ping"}`, `{"id":3,"op":"list"}`} {
		fmt.Fprintln(conn, line)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 8<<20), 8<<20)
	for i := 0; i < 4 && scanner.Scan(); i++ {
		fmt.Println(scanner.Text())
	}
}
```

## A host with no terminal

The plugin needs a TUI. Under `maki -p`, or under `maki acp`, session calls answer
`no interactive UI attached` and the ops that need a tab fail. A gateway that starts maki
itself, over ssh or from a service manager, must give it a real pseudo terminal. `tmux` is
the smallest way:

```bash
tmux new-session -d -s maki-attach 'cd ~/src/project && exec maki'
# The socket appears a moment later. Take the pid from the file name:
sock=$(ls -t ~/.local/state/maki/gateway/*.sock | head -1)
socat - "UNIX-CONNECT:$sock"
```

`tmux attach -t maki-attach` shows the same screen the plugin is driving. Keep one session
per project, because tabs belong to the maki process that opened them.

## Events

A connection that sent `hello` receives every event for the life of the plugin: `status`,
`turn_start`, `turn_end`, `tool_start`, `tool_done`, `title`, `model`, `question`, and, while
the permission bridge is on, `permission` and `permission_cancel`.

The permission bridge stays off until a gateway sends `{"op":"permissions","on":true}`. From
then on a tool call that would show a permission prompt becomes a `permission` event with a
`request_id`. Answer it with `{"op":"respond","request_id":N,"decision":"allow"}` or
`"deny"` plus `guidance`. After 55 seconds without an answer the plugin stops waiting, lets
the terminal prompt appear, and sends `permission_cancel` so the dialog can be dropped.

## Notes on behaviour

- One request gets one answer. A handler that fails replies `{"id":N,"error":"..."}` and the
  connection stays open. A line over 8 MiB, a write error, or a read error closes it.
- `new` needs a prompt. A blank tab can only open in the foreground, and this op is defined
  as a background one, so `new` without `prompt` returns an error.
- `new` does not accept another `cwd`. A new tab inherits the focused tab's project, so
  create the session in the maki process that has that project open.
- `list` reads every live tab through `maki.session.read`, which is metadata only. Missing
  fields come back as `""`.
- `messages` always passes both `session` and `last`, so a live tab answers from memory and a
  stored one comes off disk. A reply that would not fit one line comes back as an error, so
  lower `last`.
- `pop_queue` acts on the focused tab, because the host has no call for another one. A
  `session` that is not the focused tab returns an error.
- `run_command` rejects a `/reload` command line. It would rebuild the plugin host and take
  this plugin, its listener, and every open connection with it.
- The socket closes when the plugin unloads or maki exits, and the file goes with it. After a
  crash the name may stay, and the next start removes it.
