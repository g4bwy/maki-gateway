/**
 * Development mock. Canned /api replies plus a scripted fake EventSource, so the
 * UI is reviewable without a gateway.
 *
 * Loaded by app.js only when the URL carries `?mock=1`. It replaces fetch and
 * EventSource in place, so app code runs unchanged and every production path still
 * hits the real endpoints. Requests outside /api/ pass through untouched.
 */
import { S } from './store.js';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const clone = (v) => JSON.parse(JSON.stringify(v));
const now = () => Math.floor(Date.now() / 1000);
const DAY = { input: 210000, output: 41000, cache_read: 1200000, cache_creation: 180000, cost: 3.1 };

let seq = 100;
const waiting = new Map();
const live = new Set();

const sessions = [
  { id: 'web-1', kind: 'web', title: 'Fix the reconcile loop', status: 'working', cwd: '/home/gab/test/maki-gateway', model: 'claude-sonnet', updated_at: now() - 40 },
  { id: 'live-1', kind: 'live', title: 'Portal smoke test', status: 'needs_input', cwd: '/home/gab/test/maki', model: 'zai-glm-4.6', updated_at: now() - 600, plugin_pid: 4242 },
  { id: 'sto-1', kind: 'stored', title: 'Lua plugin notes', status: 'idle', cwd: '/home/gab/test/maki/plugins', model: 'claude-haiku', updated_at: now() - 90000 },
  // A web row the gateway spawned with --yolo. The chip has to read this flag off the
  // payload, and web-1 stays non-yolo so its scripted permission round still happens.
  { id: 'web-yolo-1', kind: 'web', title: 'Tidy the rail rows', status: 'idle', yolo: true, cwd: '/home/gab/test/maki-gateway/web', model: 'claude-haiku', updated_at: now() - 1200 },
];

/** Decisions the permission endpoint takes. Anything else is a client bug. */
const DECISIONS = new Set(['allow', 'allow_always', 'deny']);
const TASKS_LATER = 300;
const FOCUS_LATER = 250;
// The two focus refusals the gateway itself writes: its own sentence for a kind
// with no terminal, and the plugin sentence it forwards under the 501 sentinel
// that refusal carries (CONTRACTS 2 and 3).
const FOLLOW_LIVE_TEXT = 'follow is a live-session action';
const NO_UI_TEXT = 'no interactive UI attached';

/**
 * Fake maki.task.list rows per live session. Field names and shapes follow the
 * engine (id, session, name, status, tool, cost, times), and one row carries a
 * key maki never sends (probe) so the JSON fold path has something to show.
 */
const tasks = {
  'live-1': [
    {
      id: 't-7', session: '01J8TQ', name: 'audit the reconcile pass', status: 'working', tool: 'task',
      cost: 0.0021, started_at: now() - 90, focused: false,
      probe: [{ id: 't-7:bash', tool: 'bash', status: 'working' }],
    },
    {
      id: 't-8', session: '01J8TQ', name: 'collect contract notes', status: 'done', tool: 'task',
      cost: 0.011, started_at: now() - 400, ended_at: now() - 60, wrapper: true,
    },
  ],
};

const longOutput = 'GET /api/sessions 200 ok\n' + '  120 lines of file output\n'.repeat(28);

/**
 * Task transcripts, per session then task id. The working row t-7 has no entry:
 * maki writes a sub transcript only when the run ends, so its fetch answers [].
 * A live session without a `task_focus` plugin answers focus with NO_UI 501.
 */
const taskTranscripts = {
  'live-1': {
    't-8': [
      { role: 'user', created_at: Date.now() - 390000, content: [{ type: 'text', text: 'Collect the contract notes for section 2.' }] },
      {
        role: 'assistant', created_at: Date.now() - 380000,
        content: [
          { type: 'thinking', thinking: 'The browser surface is short. List the POST routes and the SSE envelope, skip the socket ops.' },
          { type: 'text', text: 'Notes collected: **POST + SSE only**, no WebSocket. See `docs/CONTRACTS.md` section 2.' },
          { type: 'tool_use', id: 'tsk_1', name: 'webfetch', input: { url: 'https://example.com/acp' } },
          { type: 'tool_result', tool_use_id: 'tsk_1', is_error: true, content: 'fetch failed: connection refused' },
        ],
      },
    ],
  },
};
const noUiPlugin = new Set();

const transcripts = {
  'web-1': [
    { role: 'user', created_at: Date.now() - 300000, content: [{ type: 'text', text: 'Read the **contract** and tell me what the browser may send.' }] },
    {
      role: 'assistant', created_at: Date.now() - 290000,
      content: [
        { type: 'thinking', thinking: 'They want the request surface. Section 2 lists it. Summarise the POST routes and skip the socket ops.' },
        { type: 'tool_use', id: 'tu_1', name: 'read', status: 'completed', input: { path: '/home/gab/test/maki-gateway/docs/CONTRACTS.md', offset: 80, limit: 60 } },
        { type: 'tool_result', tool_use_id: 'tu_1', content: [{ type: 'text', text: longOutput }] },
        {
          type: 'text',
          text: '## What the browser may send\n\nFour POST routes matter:\n\n- `/api/sessions/{id}/prompt` with `{text}`\n  - the answer is an SSE stream, not one JSON body\n  - a second prompt while the run is busy gets 409\n- `/api/sessions/{id}/cancel`, 501 for live\n- `/api/sessions/{id}/mode` with `{mode}`\n- `/api/permissions/{engine}/{request_id}`\n\nSee `docs/CONTRACTS.md:22` and [the spec](https://example.com/acp).\n\n```\njson\n{"state": "queued"}\n```\n\n| Route | Verb | Body |\n| :--- | :---: | ---: |\n| `prompt` | POST | `{text}` |\n| `mode` | POST | `plan|execute` |\n| `permissions` | POST | `<script>alert(1)</script>` |\n\nEscape check: <script>alert(1)</script>, **bold**, _italic_, `raw`.',
        },
        { type: 'image', mimeType: 'image/png' },
      ],
    },
  ],
  'live-1': [
    { role: 'user', created_at: Date.now() - 900000, content: [{ type: 'text', text: 'Open the portal landing page and list the apps.' }] },
    {
      role: 'assistant', created_at: Date.now() - 890000,
      content: [
        { type: 'tool_use', id: 'tu_2', name: 'bash', status: 'failed', is_error: true, input: { command: 'cd /tmp && ./drive.sh' } },
        { type: 'tool_result', tool_use_id: 'tu_2', is_error: true, content: 'exit status 127: ./drive.sh: No such file or directory' },
        { type: 'text', text: 'The helper is missing. I need to know where it lives.' },
        { type: 'weird_block', note: 'unknown to the UI', value: [1, 2, 3] },
      ],
    },
  ],
  'sto-1': [
    { role: 'user', created_at: Date.now() - 80000000, content: 'Write notes about the Lua plugin API.' },
    { role: 'assistant', created_at: Date.now() - 79000000, content: [{ type: 'text', text: 'Done. Notes are in `plugins/README.md`.' }] },
  ],
  'web-yolo-1': [
    { role: 'user', created_at: Date.now() - 700000, content: [{ type: 'text', text: 'Shorten the rail rows on a phone.' }] },
    { role: 'assistant', created_at: Date.now() - 690000, content: [{ type: 'text', text: 'Spawned with --yolo, so every ask answered itself and no prompt reached the browser.' }] },
  ],
};

let planBody = '# Plan: reconcile loop\n\n1. Key rows by a stable message key.\n2. Repaint only what changed.\n3. Keep the scroll position.\n\n**Status:** waiting for approval.\n';
let source = 'acp';

/* ------------------------------------------------------------------- emitter */

let sink = null;

/** Publish one SSE envelope, the way the gateway hub does. */
export function emit(type, fields) {
  if (!sink) return;
  if (type === 'permission') live.add(pkey(fields.engine, fields.request_id));
  if (type === 'permission_cancel') live.delete(pkey(fields.engine, fields.request_id));
  sink.fire({ type, ...fields });
}

const pkey = (engine, requestId) => engine + ':' + requestId;
const acp = (session, event) => emit('update', { session, engine: 'acp', event });
const current = (id) => sessions.find((s) => s.id === id);
const push = (id, msg) => {
  (transcripts[id] ||= []).push(msg);
  const s = current(id);
  if (s) s.updated_at = now();
};
/** Change a session status and tell subscribers, as the registry would. */
function setStatus(id, status) {
  const s = current(id);
  if (s) s.status = status;
  emit('status', { session: id, status, title: s ? s.title : '' });
}

/* --------------------------------------------------------- scripted web turn */

const CHUNKS = ['Reading the contract', ' and the current', ' renderer now.', ' Then I patch the reconcile pass.'];

/** Mirror one update into the engine-side copy of the assistant message. */
function record(rec, ev) {
  const kind = ev.sessionUpdate;
  if (kind === 'agent_message_chunk' || kind === 'agent_thought_chunk') {
    const type = kind === 'agent_message_chunk' ? 'text' : 'thinking';
    const last = rec.content[rec.content.length - 1];
    if (last && last.type === type) last[type] += ev.content.text;
    else rec.content.push({ type, [type]: ev.content.text });
    return;
  }
  if (kind === 'tool_call') {
    rec.content.push({ type: 'tool_use', id: ev.toolCallId, name: ev.name, input: ev.rawInput, status: ev.status });
    return;
  }
  if (kind === 'tool_call_update') {
    const b = rec.content.find((x) => x.type === 'tool_use' && x.id === ev.toolCallId);
    if (b) {
      b.status = ev.status;
      if (ev.rawOutput !== undefined) b.output = ev.rawOutput;
    }
  }
}

/** One fake web turn: thoughts, chunks, a tool card, a permission ask, usage. */
async function webTurn(id, text) {
  push(id, { role: 'user', created_at: Date.now(), content: [{ type: 'text', text }] });
  const rec = { role: 'assistant', created_at: Date.now() + 1, content: [] };
  push(id, rec);
  setStatus(id, 'working');
  const send = (ev) => { acp(id, ev); record(rec, ev); };
  await sleep(200);
  send({ sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: 'Plan: read first, then edit. ' } });
  send({ sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: 'Two files are involved.' } });
  for (const c of CHUNKS) {
    await sleep(320);
    send({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: c } });
  }
  await sleep(300);
  const cardId = 'tc_' + (++seq);
  send({
    sessionUpdate: 'tool_call', toolCallId: cardId, name: 'read', status: 'pending',
    rawInput: { path: '/home/gab/test/maki-gateway/web/render.js', offset: 100, limit: 40 },
  });
  await sleep(500);
  send({ sessionUpdate: 'tool_call_update', toolCallId: cardId, status: 'in_progress' });
  await sleep(900);
  send({
    sessionUpdate: 'tool_call_update', toolCallId: cardId, status: 'completed',
    rawOutput: { text: 'function paintBody(node, m, cards) {\n  node._cards = new Map();\n  ...\n}' },
  });
  await sleep(400);
  send({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: '\n\nNow I want to write the fix. ' } });
  if (!(await permissionRound(id, send))) return;
  const editId = 'tc_' + (++seq);
  send({ sessionUpdate: 'tool_call', toolCallId: editId, name: 'edit', status: 'completed', rawInput: { path: 'web/render.js' }, rawOutput: { bytes: 412 } });
  await sleep(500);
  send({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Done. Rows are reused across refetches now.' } });
  await sleep(400);
  send({ sessionUpdate: 'usage_update', usage: { input: 41200, output: 3200, cache_read: 88000 }, size: 118000, cost: 0.0412 });
  endTurn(id, 'Reconcile is patched. Mock turn over.');
}

/**
 * The permission ask of a turn. A session created with yolo was spawned with
 * --yolo, so its prompts are answered without ever reaching the browser
 * (CONTRACTS 2). Returns false when the ask fell through to the terminal.
 */
async function permissionRound(id, send) {
  if ((current(id) || {}).yolo === true) {
    send({
      sessionUpdate: 'agent_message_chunk',
      content: { type: 'text', text: 'Spawned with --yolo, so this ask answered itself. ' },
    });
    await sleep(400);
    return true;
  }
  await sleep(6000);
  const requestId = ++seq;
  emit('permission', {
    session: id, engine: 'acp', request_id: requestId, tool: 'edit',
    input: {
      path: '/home/gab/test/maki-gateway/web/render.js',
      old_string: 'node._body.replaceChildren(...parts);',
      new_string: 'node._body.replaceChildren(...parts);\n  node._sig = sigOf(m);',
      reason: 'The renderer must remember what it painted, otherwise every append rebuilds the whole transcript.',
      more: 'Guidance given here is forwarded to the agent. The terminal prompt is still available.',
    },
  });
  setStatus(id, 'needs_input');
  const answered = await waitForAnswer(requestId, 55000);
  setStatus(id, 'working');
  if (answered) return true;
  send({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'No answer arrived, so I stop here.' } });
  endTurn(id, 'The permission request fell through to the terminal.');
  return false;
}

/** Resolve true when the browser answers this request id before the timeout. */
function waitForAnswer(requestId, ms) {
  const key = pkey('acp', requestId);
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      waiting.delete(key);
      emit('permission_cancel', { engine: 'acp', request_id: requestId });
      resolve(false);
    }, ms);
    waiting.set(key, () => { clearTimeout(timer); resolve(true); });
  });
}

/** The engine cache already holds the message; close the turn for subscribers. */
function endTurn(id, note) {
  setStatus(id, 'idle');
  emit('notice', { text: note });
}

/* -------------------------------------------------------------------- router */

const ok = (data, status) => new Response(JSON.stringify(data), {
  status: status || 200, headers: { 'content-type': 'application/json' },
});
const http = (status, error) => ok({ error }, status);

function create(body) {
  const cwd = String((body && body.cwd) || '');
  if (!cwd) return http(400, 'cwd is required');
  const id = 'web-' + (++seq);
  const s = { id, kind: 'web', title: '', status: 'idle', cwd, model: body.model || 'mock-model', updated_at: now() };
  // Rows carry `yolo` only when it is on, the way the gateway reports them.
  if (body && body.yolo === true) s.yolo = true;
  sessions.unshift(s);
  transcripts[id] = [];
  emit('sessions', {});
  Promise.resolve().then(() => {
    if (body.prompt) return webTurn(id, String(body.prompt));
    const rec = { role: 'assistant', created_at: Date.now() + 1, content: [] };
    push(id, rec);
    setStatus(id, 'working');
    const send = (ev) => { acp(id, ev); record(rec, ev); };
    send({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Mock session ready. Send a message.' } });
    send({ sessionUpdate: 'usage_update', usage: { input: 1200, output: 40 }, size: 12000, cost: 0.001 });
    return setStatus(id, 'idle');
  });
  return ok({ session: id }, 201);
}

const messages = (id) => {
  const s = current(id);
  return ok({ source: s && s.kind === 'live' ? 'attach' : source, messages: clone(transcripts[id] || []) });
};

function permission(engine, requestId, decision, guidance) {
  const key = pkey(engine, requestId);
  // Same order as the gateway: a bad decision is 400 even for an expired request.
  if (!DECISIONS.has(decision)) return http(400, 'unknown decision ' + decision);
  if (!live.has(key)) return http(404, 'expired');
  live.delete(key);
  if (decision === 'deny' && guidance) planBody = '# Plan\n\nMock notes from the deny guidance: ' + guidance + '\n';
  const waiter = waiting.get(key);
  waiting.delete(key);
  if (waiter) waiter(decision);
  return ok({ ok: true });
}

/** Tasks of a live session, as GET /api/sessions/{id}/tasks answers. */
function tasksOf(id) {
  const s = current(id);
  if (!s) return http(404, 'unknown session ' + id);
  if (s.kind !== 'live') return http(501, 'tasks are only available for live sessions');
  return ok({ tasks: clone(tasks[id] || []) });
}

/**
 * Transcript of one task. [] while the task runs (maki writes sub transcripts
 * only at run end); 404 only for a session no engine reports.
 */
function taskMessagesOf(id, tid) {
  if (!current(id)) return http(404, 'unknown session ' + id);
  return ok({ messages: clone((taskTranscripts[id] || {})[tid] || []) });
}

/**
 * Follow a task in the terminal, status for status with the gateway. Only a live
 * row has a screen to move (501 followLiveText), and a refusal the terminal wrote
 * keeps its own words: 501 for a maki with no interactive UI behind its plugin,
 * 404 for an id that is not one of this tab's chats. Success raises the
 * task_focus event the browser moves its marker on.
 */
function focusTask(id, tid) {
  const s = current(id);
  if (!s) return http(404, 'unknown session ' + id);
  if (s.kind !== 'live') return http(501, FOLLOW_LIVE_TEXT);
  if (noUiPlugin.has(id)) return http(501, NO_UI_TEXT);
  const rows = tasks[id] || [];
  if (tid !== 'main' && !rows.some((t) => t && t.id === tid)) return http(404, 'no task with id ' + tid);
  setTimeout(() => emit('task_focus', { session: id, id: tid }), FOCUS_LATER);
  return ok({ ok: true });
}

function search(q) {
  const needle = String(q || '').toLowerCase();
  const hits = [];
  if (needle.length < 2) return ok({ hits });
  for (const s of sessions) {
    for (const m of transcripts[s.id] || []) {
      const parts = Array.isArray(m.content) ? m.content : [{ type: 'text', text: String(m.content) }];
      for (const b of parts) {
        const text = b && (b.text || b.thinking);
        if (!text) continue;
        const at = String(text).toLowerCase().indexOf(needle);
        if (at < 0) continue;
        hits.push({
          session: s.id, title: s.title || s.id,
          snippet: String(text).slice(Math.max(0, at - 40), at + 120).replace(/\s+/g, ' '),
        });
      }
    }
  }
  return ok({ hits: hits.slice(0, 50) });
}

function analytics(days) {
  const out = [];
  for (let i = (Number(days) || 30) - 1; i >= 0; i--) {
    const d = new Date(Date.now() - i * 86400000);
    const quiet = d.getDay() === 0 || d.getDay() === 6;
    const f = (quiet ? 0.2 : 1) * (0.4 + ((i * 37) % 10) / 10);
    const scale = (k) => Math.round(DAY[k] * f);
    out.push({
      date: d.toISOString().slice(0, 10), sessions: Math.max(1, Math.round(6 * f)),
      input: scale('input'), output: scale('output'), cache_read: scale('cache_read'),
      cache_creation: scale('cache_creation'), cost: +(DAY.cost * f).toFixed(4),
    });
  }
  const per = sessions.map((s, i) => ({
    session: s.id, title: s.title || s.id, model: s.model,
    cost: +(0.4 + i * 1.7).toFixed(4), updated_at: s.updated_at,
  }));
  return ok({ daily: out, per_session: per });
}

/** A live session has no deltas: the mock pushes a reply and reports dirty. */
function attachTurn(id, text) {
  setStatus(id, 'working');
  setTimeout(() => {
    push(id, {
      role: 'assistant', created_at: Date.now(),
      content: [{ type: 'text', text: 'Mock attach reply for: ' + text + '. The live engine sends no deltas, so this arrived by refetch.' }],
    });
    emit('dirty', { session: id, reason: 'tool_start' });
    emit('dirty', { session: id, reason: 'tool_done' });
  }, 1200);
  setTimeout(() => {
    emit('dirty', { session: id, reason: 'turn_end' });
    setStatus(id, 'idle');
  }, 2400);
}

/** A web row is mid-prompt while its turn streams or waits on a decision. */
const midPrompt = (s) => s.status === 'working' || s.status === 'needs_input';

function route(method, path, body) {
  const u = new URL(path, 'http://mock');
  const p = u.pathname;
  const seg = p.split('/').filter(Boolean);
  const id = seg[2];
  const s = current(id);
  if (p === '/api/health') return ok({ version: 'mock', maki: 'mock maki', sessions: sessions.length });
  if (p === '/api/sessions' && method === 'GET') return ok(clone(sessions));
  if (p === '/api/sessions' && method === 'POST') return create(body);
  if (seg[1] === 'sessions' && seg[3] === 'messages') return messages(id);
  if (seg[1] === 'sessions' && seg[3] === 'tasks' && seg[5] === 'messages') return taskMessagesOf(id, decodeURIComponent(seg[4]));
  if (seg[1] === 'sessions' && seg[3] === 'tasks' && seg[5] === 'focus') return focusTask(id, decodeURIComponent(seg[4]));
  if (seg[1] === 'sessions' && seg[3] === 'tasks') return tasksOf(id);
  if (seg[1] === 'permissions') return permission(seg[2], Number(seg[3]), body && body.decision, body && body.guidance);
  if (seg[1] === 'plan') {
    if (s && s.kind === 'web') return http(501, 'plan is only available for live sessions');
    return ok({ mode: 'plan', path: '/home/gab/test/maki/plans/reconcile-loop.md', ready: true, content: planBody });
  }
  if (seg[1] === 'search') return search(u.searchParams.get('q'));
  if (seg[1] === 'analytics') return analytics(u.searchParams.get('days'));
  if (seg[1] !== 'sessions' || !s) return http(404, 'unknown path ' + p);
  const action = seg[3];
  if (method === 'DELETE') {
    if (s.kind !== 'web') return http(400, 'only web sessions can be killed');
    s.status = 'exited';
    emit('exit', { session: id, code: 137 });
    emit('sessions', {});
    return ok({ killed: true });
  }
  if (action === 'prompt') {
    const text = String((body && body.text) || '');
    if (!text) return http(400, 'text is required');
    if (s.kind === 'stored') return http(409, 'read-only session');
    if (s.kind === 'live') {
      push(id, { role: 'user', created_at: Date.now(), content: [{ type: 'text', text }] });
      emit('dirty', { session: id, reason: 'status' });
      if (s.status === 'working') return ok({ state: 'queued' });
      attachTurn(id, text);
      return ok({ state: 'started' });
    }
    if (s.status === 'working') return http(409, 'busy');
    webTurn(id, text);
    return ok({ state: 'started' });
  }
  if (action === 'cancel') {
    if (s.kind === 'live') return http(501, 'cancel is not supported for live sessions');
    setStatus(id, 'idle');
    return ok({ cancelled: true });
  }
  if (action === 'mode') {
    S.mode = String((body && body.mode) || 'build');
    return ok({ ok: true });
  }
  if (action === 'yolo') {
    if (s.kind !== 'web') return http(501, 'yolo is only available for web sessions');
    if (midPrompt(s)) return http(409, 'busy');
    const on = !!(body && body.on);
    if (on) s.yolo = true; else delete s.yolo;
    emit('sessions', {});
    return ok({ yolo: on });
  }
  if (action === 'title') {
    s.title = body && body.title != null ? String(body.title) : '';
    emit('sessions', {});
    return ok({ ok: true });
  }
  return http(404, 'unknown path ' + p);
}

/* -------------------------------------------------------------- EventSource */

class FakeEventSource {
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.handlers = new Map();
    sink = this;
    setTimeout(() => {
      this.readyState = 1;
      if (this.onopen) this.onopen({});
      emit('notice', { text: 'mock gateway attached (this is ?mock=1)' });
    }, 50);
  }

  addEventListener(type, cb) {
    if (!this.handlers.has(type)) this.handlers.set(type, new Set());
    this.handlers.get(type).add(cb);
  }

  /** Deliver one envelope the way the browser does: event.data is the JSON text. */
  fire(envelope) {
    if (this.readyState !== 1) return;
    const data = JSON.stringify(envelope);
    for (const cb of this.handlers.get('maki') || []) cb({ data });
    if (this.onmessage) this.onmessage({ data });
  }

  close() {
    this.readyState = 2;
    this.handlers.clear();
    if (sink === this) sink = null;
  }
}

/** Cut the fake stream, then restore it: shows the offline banner and the resync. */
export function dropStream(seconds = 2) {
  if (!sink) return;
  sink.readyState = 0;
  if (sink.onerror) sink.onerror({});
  const s = sink;
  setTimeout(() => {
    if (s.readyState !== 0) return;
    s.readyState = 1;
    if (s.onopen) s.onopen({});
  }, seconds * 1000);
}

/** Push a permission ask for one session, with no turn behind it. */
export function fakePermission(id, requestId) {
  const s = current(id || 'live-1');
  const engine = s && s.kind === 'live' ? 'attach' : 'acp';
  const rid = requestId || ++seq;
  emit('permission', {
    session: s ? s.id : 'live-1', engine, request_id: rid, tool: 'bash',
    input: { command: 'git push origin HEAD', note: 'Stand-alone mock request, no turn behind it.' },
  });
  // Answering with an allow lets the round spawn work, so the mock reports a task
  // change the way the gateway does. A deny spawns nothing and raises no event.
  const key = pkey(engine, rid);
  if (s) {
    waiting.set(key, (verdict) => {
      if (verdict !== 'deny') setTimeout(() => emit('tasks', { session: s.id }), TASKS_LATER);
    });
  }
}

/** Set the rows a live session reports, so the panel can be driven empty or full. */
export function tasksFor(id, list) {
  tasks[id] = Array.isArray(list) ? list : [];
}

/** Seed the stored transcript of one task, the way a finished run would leave it. */
export function taskTranscriptFor(id, tid, msgs) {
  (taskTranscripts[id] ||= {})[tid] = Array.isArray(msgs) ? msgs : [];
}

/** Make a live session answer focus like a TUI without the updated plugin. */
export function oldPlugin(id, on = true) {
  if (on) noUiPlugin.add(id); else noUiPlugin.delete(id);
}

/** Forget a request id, so the next answer replies 404 expired. */
export function expire(engine, requestId) {
  live.delete(pkey(engine, requestId));
}

/** Background traffic: a status flip, then a session that appears from nowhere. */
let ambientOn = true;

/** Turn the background traffic off, for scripted runs. */
export function quiet() {
  ambientOn = false;
}

function later(ms, fn) {
  setTimeout(() => { if (ambientOn) fn(); }, ms);
}

function ambient() {
  later(15000, () => emit('status', { session: 'live-1', status: 'working', title: 'Portal smoke test' }));
  later(30000, () => {
    sessions.push({ id: 'live-2', kind: 'live', title: 'Gateway review', status: 'idle', cwd: '/home/gab/test/maki-gateway', model: 'claude-sonnet', updated_at: now() });
    transcripts['live-2'] = [{ role: 'user', created_at: Date.now(), content: [{ type: 'text', text: 'A session that appeared while you were watching.' }] }];
    emit('sessions', {});
  });
  later(50000, () => emit('notice', { text: 'mock: still here, nothing to do' }));
}

/** Replace fetch and EventSource with the mock. Called by app.js for ?mock=1. */
export function install() {
  const real = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async (input, init) => {
    const raw = typeof input === 'string' ? input : input.url;
    const u = new URL(raw, location.href);
    if (!u.pathname.startsWith('/api/')) return real(input, init);
    const method = ((init && init.method) || 'GET').toUpperCase();
    let body = null;
    if (init && init.body) {
      try { body = JSON.parse(init.body); } catch { body = null; }
    }
    await sleep(120 + Math.random() * 160);
    return route(method, u.pathname + u.search, body);
  };
  globalThis.EventSource = FakeEventSource;
  ambient();
}
