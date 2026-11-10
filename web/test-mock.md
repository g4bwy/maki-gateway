# Manual check list for `?mock=1`

No browser was available while this UI was written, so the modules were driven by a
DOM shim under Node (mock traffic, real code paths). Everything below still needs a
human pass on a phone or a browser.

Two passes are available.

Automated, no browser needed. It drives the real modules through a DOM shim and
prints one line per check:

```
cd maki-gateway/web && node test-mock.mjs     # 113 checks, then ALL CHECKS PASSED
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
maki.mock.expire('acp', 101)                 // next answer to id 101 replies 404
maki.mock.drop(6)                            // cut the fake stream for 6 s
maki.mock.quiet()                            // stop the background status traffic
```

From the browser console the mock answers to `maki.mock`:

```
maki.mock.permission('live-1')     // a permission dialog, attach engine
maki.mock.emit('notice', { text: 'hi' })
maki.mock.expire('acp', 101)       // next answer to id 101 replies 404
maki.mock.drop(6)                  // kill the fake stream for 6 s
maki.mock.quiet()                  // stop the background status and session events
```

Mock sessions: `web-1` (kind web, streaming), `live-1` (kind live, dirty refetch),
`sto-1` (kind stored, read only). Creating a chat adds a new `web-*` session.

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
    paragraph with bold text, a code fence, and a link.
14. The tool card is collapsed. Expand it: the input is pretty JSON. The result is
    inside the same card. A long result ends with "show all (N chars)".
15. `live-1` shows a failed card in red with an "error" label, an image placeholder,
    and an unknown block rendered as raw JSON.
16. `sto-1` is read-only: the composer is disabled, "read-only transcript" shows, and
    the send, cancel, mode, plan, and kill controls are all hidden.
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

## Notes for the real gateway

- `store.js:api` is the whole request surface. If the gateway renames a field, fix it
  in `store.js` or `render.js` only.
- Unknown message, block, or ACP shapes render as raw JSON rather than dropping text.
- The renderer trusts only `http:`, `https:`, and `mailto:` links.
- No endpoint reports the current mode, so the build/plan chip label is optimistic. It
  rolls back when the POST fails, and it follows the server when an ACP
  `current_mode_update` arrives. A `mode` field on `Session` would settle it.
- `dirty` for a session that is not open is ignored: opening it fetches anyway.
