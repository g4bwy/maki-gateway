# Manual check list for `?mock=1`

No browser was available while this UI was written, so the modules were driven by a
DOM shim under Node (mock traffic, real code paths). Everything below still needs a
human pass on a phone or a browser.

Two passes are available.

Automated, no browser needed. It drives the real modules through a DOM shim and
prints one line per check:

```
cd maki-gateway/web && node test-mock.mjs     # 272 checks, then ALL CHECKS PASSED
```

By hand, in a browser:

```
cd maki-gateway/web && python3 -m http.server 8080
# then open http://localhost:8080/?mock=1
```

The browser console also drives the fake gateway, which is the only way to reach a
few paths on demand:

```
maki.mock.permission('live-1')               // open a permission dialog now
maki.mock.emit('notice', { text: 'hello' })  // any SSE envelope
maki.mock.emit('tasks', { session: 'live-1' })  // a task changed: the panel refetches
maki.mock.emit('task_focus', { session: 'live-1', id: 't-7' })  // the terminal follows that task
maki.mock.tasks('live-1', [])                // the rows live-1 reports from now on
maki.mock.taskTranscript('live-1', 't-8', [])  // the stored transcript of one task
maki.mock.oldPlugin('live-1')                // focus answers like a TUI with an old plugin
maki.mock.expire('acp', 101)                 // next answer to id 101 replies 404
maki.mock.drop(6)                            // cut the fake stream for 6 s
maki.mock.quiet()                            // stop the background status traffic
```

Mock sessions: `web-1` (kind web, streaming), `live-1` (kind live, dirty refetch),
`sto-1` (kind stored, read only), and `web-yolo-1` (kind web, idle, spawned with --yolo).
Creating a chat adds a new `web-*` session. `live-1` reports two fake sub-agent tasks (one
working, one done); every other kind answers 501 for the task list, like the gateway does.
The working task has no stored transcript, so its messages endpoint answers an empty list. The
done task has a short fake transcript (thinking, text, one failed tool result). Its focus
endpoint answers status for status with the gateway: 200 for a live row, 501
"follow is a live-session action" for web and stored, 501 "no interactive UI attached" for a
session marked old-plugin, 404 "no task with id …" for a row that tab does not have, and a
`task_focus` event after a successful follow.
A chat created with the yolo box ticked answers its own permission prompts, so no dialog
opens for it. `POST /api/sessions/{id}/yolo` flips the flag on a web row and raises a
`sessions` event. It replies 409 while that row is mid-prompt, so the working `web-1`
always rejects a toggle, and it replies 501 for the live and stored rows.

## Layout and style

1. Dark theme by default. Switch the OS to light and reload: text stays readable,
   code blocks stay dark.
2. Open on a phone or in a narrow window (under 768 px). The rail is off screen.
   The hamburger opens it over a scrim; tapping the scrim or a session closes it.
3. On a wide screen the rail is always visible and the hamburger is hidden.
4. Every icon button, list row, and dialog button is at least 44 px tall.
5. Tab with the keyboard. Focus shows a visible ring on every control.
6. With a software keyboard open, the composer stays above it, and the FAB does not
   cover the transcript's last line. Check a device with a home bar (safe area).

## Rail and top bar

7. Badges: green dot = live, blue dot = web, gray dot = stored. Working sessions show
   a spinner, needs-input amber, exited red, idle a hollow ring.
8. Each row shows title, relative time, and the last folder of the cwd. The selected
   row is highlighted and marked `aria-current`.
9. Tap the title, rename it, Save. Renaming is live-only: on `live-1` the rail row and the
   browser tab title follow the new name. Clear the field and save to go back to the file name.
   Against the real gateway a `web` session answers 501, because maki-acp dispatches no
   `session/set_title`, so the optimistic title rolls back and a toast reports the failure. The
   mock accepts every rename with 200, so the automated run keeps the new title on `web-1`.
10. The mode chip is visible for `web-1` and `live-1` only, and flips build/plan.
11. The plan icon appears only for `live-1`. The drawer shows the plan in markdown.
12. The Kill icon appears only for a `web` session. It asks first. After Confirm the
    row turns red and a toast reports the exit code.

## Transcript

13. `web-1` shows: a thinking section (collapsed), a read tool card, a result, a
    heading, a paragraph with bold text, a bullet list with two nested items, a code
    fence, a link, and a 3-column table. Items 55 to 58 check the new blocks.
14. The tool card is collapsed. Expand it: the input is pretty JSON. The result is
    inside the same card. A long result ends with "show all (N chars)".
15. `live-1` shows a failed card in red with an "error" label, an image placeholder,
    and an unknown block rendered as raw JSON.
16. `sto-1` is read-only: the composer is disabled, "read-only transcript" shows, and
    the send, cancel, mode, plan, kill, and yolo controls are all hidden.
17. Read the escaped sample: it prints `<script>alert(1)</script>` as text and there is
    no popup. Check the page source for a `<script>alert` inside the transcript.
18. Scroll up, then let a turn stream. The view stays where you left it. Near the
    bottom it follows the new text.
19. Expand a card, send a prompt on `live-1` (a refetch runs after ~1.5 s): the card is
    still expanded and rows are not rebuilt from scratch.

## Composer and stream

20. Type in the composer: it grows to a few lines, then scrolls. Enter sends,
    Shift+Enter adds a newline, the arrow button sends on touch.
21. Send on `web-1` while it is working: a toast reports it is busy and the text goes
    back into the box.
22. Send on `live-1` twice quickly: the second shows "Queued as the next turn."
23. Send on `web-101` (or a session you create): the message shows at once dimmed,
    then thinking, then text, then a tool card that walks queued to in_progress to
    completed, then a footer line with tokens, context, and cost.
24. About 10 s into that turn a permission dialog opens. It lists the tool name, the
    input JSON scrolls, and the guidance box is there.
25. Tap Allow: the turn continues. Tap Deny with guidance: the plan drawer picks it up.
26. Kill the process while a dialog is open (or wait ~55 s in the mock): the dialog
    closes on its own and a toast says the request expired.
27. The stop square shows only for a `web` session and answers with 501 elsewhere
    (the mock replies 501 for `live-1`; the button stays hidden for live anyway).

## Search, analytics, offline

28. Search finds "contract". Tapping a hit closes the overlay and opens that session.
29. Analytics shows summary cards, a top-costs table, and 30 daily rows.
30. In the console run `maki.S.mock` … there is none: instead call the mock helpers, or
    just reload. The offline banner appears when the stream drops and clears on its own.
31. Reload the page: the same session is selected, and the composer keeps one draft per
    session when you switch back and forth.
32. `?token=demo` in the URL: check the Network tab that `/api/events?token=demo` and
    every fetch carry the token.

## Yolo, always-allow, and sub-agents

33. New chat: the form has a yolo box under the model field, labelled "yolo — skip
    permission prompts, deny rules still apply". Leave it empty and watch the Network
    tab: the POST body has no `yolo` key at all.
34. Tick it, start the chat, then open the dialog again for the same folder: the box
    comes back ticked. Type a different folder and it goes back to unticked. The choice
    lives in `localStorage` under `maki.yolo`, one entry per folder.
35. Start a chat with yolo ticked and a first prompt: that session streams its turn and
    never opens a permission dialog, because the mock spawns it the way `--yolo` does.
36. Ask a `web` session for permission (`maki.mock.permission('web-1')` after opening it,
    or wait for the scripted ask): the dialog offers Deny, Allow once, and Always allow,
    and Always allow is the blue one. It posts `{"decision":"allow_always"}`.
37. Ask a `live` session (`maki.mock.permission('live-1')`): only Deny and Allow once
    show. The attach bridge has no "remember this" answer, so the button would lie.
38. Deny with text in the guidance box: the plan drawer picks that text up, as before.
39. Open `live-1`: a "Sub-agents" section sits under the top bar with a count of 2, one
    row spinning and one done. Each row shows the fields its task carries (name, tool,
    cost, elapsed time) and folds the rest as JSON. Tapping a row opens the task sheet
    (section below). `web-1` and `sto-1` have no section.
40. With `live-1` open, run `maki.mock.tasks('live-1', [])` then
    `maki.mock.emit('tasks', { session: 'live-1' })`: after half a second the panel
    refetches and reads "none running". Three `tasks` events in a row refetch once.
41. Reload on a phone width: the section starts collapsed, and its own header folds it.
    That does not touch the session rail, which folds separately.

## Yolo chip in the top bar

42. Open `web-1`: a `YOLO` pill sits next to the mode chip with a hollow dot, and the
    kind chip still reads WEB. Open `web-yolo-1`: the same pill shows an amber background
    with a filled dot. The pill is a switch (`role="switch"`), so a screen reader says
    "on" or "off". `sto-1` shows no pill.
43. Open `live-1`: the pill is dimmed and disabled. Its tooltip and its accessible name
    both read "toggle /yolo in the terminal — the web cannot change a running TUI
    session". Tapping it sends nothing. Watch the Network tab: no request appears.
44. Tap the pill on `web-yolo-1`: the request body is `{"on":false}` and the reply
    `{"yolo":false}` is what turns the pill off. Reload: the mock starts it on again,
    because the row, not the browser, owns the flag.
45. Tap the pill on `web-1` while it works: the reply is 409, the toast reads "finish or
    cancel the current turn first", and the pill stays off. The mock cannot fail a POST,
    so the automated run stubs `fetch` to cover the request-error toast.
46. Open `web-yolo-1` and run `maki.api.setYolo('web-yolo-1', true)` in the console. The
    mock answers, raises a `sessions` event, and the pill turns on with no tap. The same
    refetch brings the flag for a row you just created, so the pill and the new chat box
    always describe one value.
47. Look at the top bar at 390 px: the pills and icons move to a second row and nothing
    scrolls sideways. The pills stay small, so their tap area is grown to about 44 px
    around each one instead.

## Task detail sheet and follow

48. Open `live-1`. The sub-agent rows are buttons, each still 44 px tall. Tap
    `collect contract notes` (done). A sheet opens on the right. On a phone width it covers the
    whole screen, and the Close icon in its bar dismisses it. The header shows the task name, a
    green status dot, the cost, and the started and ended times. Tap the copy icon: the task id
    goes to the clipboard and a toast confirms it. Without clipboard permission a hidden text
    box feeds the copy, and no error appears.
49. Read the sheet body: it shows the task transcript with the same blocks as the main chat.
    Expect a folded thinking section, a markdown paragraph, and one tool card marked failed
    with its error text inside.
50. Tap `audit the reconcile pass` (working). Its transcript is not written yet, so the sheet
    shows an info banner instead of an error, with this text: "The transcript is written when
    the run ends. You can follow it in the terminal meanwhile." A finished task without a
    transcript answers "No transcript records." You can drive that with
    `maki.mock.taskTranscript('live-1', 't-8', [])` and a reload of the sheet.
51. Tap FOLLOW in a live sheet header. The request goes to
    `POST /api/sessions/live-1/tasks/{id}/focus`. On success the row and the sheet header get a
    small `followed in terminal` chip, and the toast reads exactly: "The terminal keyboard now
    types into this task; browser prompts still go to the main chat." That wording is the
    steering caveat: only the keys typed at the terminal follow the task, and the browser keeps
    sending its prompts to the main chat. The toast appears once per session: follow a second
    task in the same session and no new toast comes. Switch to another session and back, and
    the next follow toasts again. The sheet fetches the transcript once more about 2 s after a
    successful follow.
52. With a sheet open, run `maki.mock.emit('tasks', { session: 'live-1' })`. The transcript
    refetches once on the same 500 ms debounce as the panel. Three events in a row refetch
    once.
53. The mock answers a successful follow with a `task_focus` event after a short delay, like
    the terminal would. Run `maki.mock.emit('task_focus', { session: 'live-1', id: 't-7' })`:
    the chip moves to that row, and a sheet for another task hides its chip. `id: 'main'`
    clears every chip. A `task_focus` event with no id, or for another session, changes nothing
    on screen.
54. A `web` or `stored` session has no rows, so a FOLLOW button never shows there. Run
    `maki.api.taskFocus('web-1', 't-8')` and the same for `sto-1`: 501 with "follow is a
    live-session action". Run `maki.api.taskFocus('live-1', 'nope')`: 404 with "no task with id
    nope". Run `maki.mock.oldPlugin('live-1')` and follow again: 501 with "no interactive UI
    attached", the verbatim plugin error under the status its sentinel picks.

## Markdown blocks and TUI parity

`web/md.js` is the only markup step for message text. It escapes first, then splits
blocks, then applies the inline rules. The terminal runs `maki-markdown` on the same
text, so both show the same structure. Read these items next to the terminal.

55. Open `web-1`. The assistant answer starts with an h2 heading ("What the browser may
    send"), then a bullet list whose first item holds two nested items, then a code
    fence, then a 3-column table. On a phone width the table scrolls sideways inside its
    own wrapper, and the message body keeps its width.
56. Read the table: the Route column is left aligned, Verb is centered, Body is right
    aligned. `plan|execute` stays one cell in monospace, so the pipe inside the code span
    did not split the row. The last cell prints `<script>alert(1)</script>` as text, with
    no popup. Open the page source: inside the transcript the markup reads `&lt;script&gt;`.
57. Open the plan drawer on `live-1` (item 11). Its title shows as an h1 and its three
    steps as a numbered list, so the drawer and the task sheet use the same renderer as
    the chat.
58. Compare with the terminal on the same text. The heading, the list nesting, the table
    columns, and the alignments carry over. The gaps below are known, so do not
    chase them during a review:

- The web code fence has no syntax highlighting. The terminal colors every token.
- The terminal draws a box with column borders around a table. The web draws a rule under
  the header and between rows only.
- A row without outer pipes (`a | b` over `--- | ---`) is a table in the web client. The
  terminal needs `| a | b |` and shows that pair as plain text.
- A body row with more cells than the header keeps the header column count in the web and
  adds columns in the terminal.
- `> quote` renders as a quote block in the web client. The terminal has no blockquote, so
  it prints the line as text.
- A table that is still streaming shows raw pipes until its delimiter row arrives. Both
  sides behave like that, because a table needs that row.

## Notes for the real gateway

- `store.js:api` is the whole request surface. If the gateway renames a field, fix it
  in `store.js` or `render.js` only.
- Task rows arrive from `maki.task.list()` untouched, and the gateway only answers them
  for a live tab. Row 1 of that list is the main chat: it carries no `status`. The panel
  paints a plain ring for any row whose status it does not know, and keeps the fields it
  cannot show behind the JSON fold.
- The task sheet uses the frozen task-detail contract. `GET .../tasks/{id}/messages` answers
  200 with an empty list while a task runs, because maki writes sub transcripts only at run
  end. It answers 404 only for an unknown session. `POST .../tasks/{id}/focus` answers
  `{"ok":true}` for a live row, 501 with its own sentence for web and stored, and the refusal
  the terminal wrote with the status that refusal maps to: 501 "no interactive UI attached"
  and 404 "no task with id …". The browser expects a `task_focus` SSE event with fields
  `session` and `id` whenever the followed task in the terminal changes, `id` `"main"` included.
- Unknown message, block, or ACP shapes render as raw JSON rather than dropping text.
- The renderer trusts only `http:`, `https:`, and `mailto:` links.
- No endpoint reports the current mode, so the build/plan chip label is optimistic. It
  rolls back when the POST fails, and it follows the server when an ACP
  `current_mode_update` arrives. A `mode` field on `Session` would settle it.
- The yolo pill has no state of its own. It reads `yolo` off the row, and the row only
  changes from `GET /api/sessions` or from the `{"yolo":...}` body of its own POST. An
  absent flag reads as off, so a gateway that does not send it shows an off pill.
- `dirty` for a session that is not open is ignored: opening it fetches anyway. The same
  holds for `tasks`. The `tasks` op reports the tab the TUI has focused, which is not
  always the tab the event names, so a row set can describe another tab.
