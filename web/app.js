/**
 * maki-gateway web client. Phone-first control surface for maki coding sessions.
 *
 * Data flow (docs/CONTRACTS.md sections 2 and 3):
 *   GET /api/sessions               -> rail rows, patched in place by session id
 *   GET /api/sessions/{id}/messages -> transcript, with optimistic bubbles on top
 *   GET /api/events (SSE)           -> one `maki` envelope per message, dispatched below
 *
 * Two live paths write the same Message shape, so the renderer never branches on
 * source. Sessions of kind `web` fold ACP `session/update` passthrough in place
 * (acp.js); sessions of kind `live` only report `dirty`, so a debounced 500 ms
 * refetch pulls the whole tail. `sessions`, `status`, `notice`, `exit` and
 * `permission` events drive the rail, the toasts, and the decision modals. A
 * `tasks` event refetches the sub-agents panel the same debounced way.
 *
 * Development without a gateway: `/?mock=1` loads mock.js. Real paths stay real.
 */
import { S, api, ApiError, session, textOf, withToken, tail, ago, saveSession, lastSession, saveCwd } from './store.js';
import { renderTranscript, el } from './render.js';
import { applyUpdate, closeStream } from './acp.js';
import {
  toast, askText, askYes, popLayer, newChat, searchOverlay, analyticsView,
  planDrawer, permissionModal, permissionGone, taskRows,
} from './ui.js';

const MAX_GROW = 180;
const DIRTY_MS = 500;
const TICK_MS = 30000;
const KIND_LABEL = { web: 'WEB', live: 'LIVE', stored: 'STORED' };

const $ = (id) => document.getElementById(id);
const row = (id) => els.list.querySelector(`[data-id="${CSS.escape(id)}"]`);
const els = {};
const drafts = new Map();
const dirty = new Map();
let events = null;
let queued = false;
let tasksOpen = true;
let tasksReq = 0;

/** Short text a thumb can read in a toast. */
const errText = (e, fallback) => (e instanceof ApiError
  ? e.message + (e.status ? ` (${e.status})` : '')
  : (e && e.message) || fallback);

/** Batch transcript repaints into one frame. */
function paint() {
  if (queued) return;
  queued = true;
  requestAnimationFrame(() => { queued = false; renderTranscript(els.transcript); });
}

/* ---------------------------------------------------------------- transcript */

/** Replace the transcript. An in-flight stream keeps its tail so chunks stay alive. */
async function loadMessages() {
  const id = S.current;
  if (!id) return;
  try {
    const r = await api.messages(id, 200);
    if (id !== S.current) return;
    const wasStreaming = !!(S.messages[S.messages.length - 1] || {})._stream;
    closeStream();
    S.messages = ((r && r.messages) || []).map((m) => ({ ...m }));
    if (wasStreaming) {
      let tail = S.messages[S.messages.length - 1];
      if (!tail || tail.role !== 'assistant') {
        tail = { role: 'assistant', content: [], created_at: Date.now(), _stream: true };
        S.messages.push(tail);
      } else {
        tail._stream = true;
      }
    }
    S.pending = S.pending.filter((p) => !S.messages.some((m) => m.role === 'user' && textOf(m) === p.text));
    els.transcript.dataset.source = (r && r.source) || '';
    paint();
  } catch (e) {
    if (id === S.current) toast(errText(e, 'Could not load this transcript.'), 'err');
  }
}

/** Refetch at most once per DIRTY_MS window per key. */
function debounce(key, run) {
  if (dirty.has(key)) return;
  dirty.set(key, setTimeout(() => { dirty.delete(key); run(); }, DIRTY_MS));
}

/** A live session changed: refetch its tail once, after the stream settles. */
function scheduleMessages(id) {
  if (id !== S.current) return;
  debounce('m' + id, loadMessages);
}

export async function selectSession(id) {
  if (!id) return;
  const changed = id !== S.current;
  if (changed && S.current) drafts.set(S.current, els.input.value);
  S.current = id;
  saveSession(id);
  if (!changed) {
    loadTasks();
    return loadMessages();
  }
  S.messages = [];
  S.pending = [];
  els.transcript.replaceChildren();
  els.input.value = drafts.get(id) || '';
  grow();
  if (narrow()) rail(false);
  paintTopbar();
  paintRail();
  loadTasks();
  await loadMessages();
}

/* ---------------------------------------------------------------------- rail */

function patchRow(li, s) {
  li._dot.className = 'dot ' + s.kind;
  li._dot.setAttribute('aria-label', s.kind);
  li._st.className = 'st ' + s.status;
  li._st.setAttribute('title', s.status);
  li._title.textContent = s.title || 'Untitled session';
  li._when.textContent = ago(s.updated_at);
  li._cwd.textContent = tail(s.cwd) + (s.model ? ' · ' + s.model : '');
  li.classList.toggle('on', s.id === S.current);
  li.classList.toggle('exited', s.status === 'exited');
  li.setAttribute('aria-current', s.id === S.current ? 'true' : 'false');
}

function rowFor(s) {
  const li = row(s.id);
  if (li) { patchRow(li, s); return li; }
  const node = el('li', 'row');
  node.dataset.id = s.id;
  node.tabIndex = 0;
  node._dot = el('span');
  node._st = el('span', 'st');
  node._title = el('span', 'rtitle');
  node._when = el('span', 'rtime');
  node._cwd = el('span', 'rcwd');
  const top = el('div', 'rtop');
  top.append(node._title, el('span', 'rgrow'), node._when);
  node.append(node._dot, node._st, top, node._cwd);
  node.addEventListener('click', () => selectSession(s.id));
  node.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); selectSession(s.id); }
  });
  patchRow(node, s);
  return node;
}

/** Keep rows in server order; add, patch, and drop only what changed. */
function paintRail() {
  const keep = [];
  for (const s of S.sessions) keep.push(rowFor(s));
  const wanted = new Set(keep);
  for (const li of Array.from(els.list.children)) if (!wanted.has(li)) li.remove();
  els.list.append(...keep);
}

async function loadSessions() {
  try {
    const list = await api.sessions();
    S.sessions = Array.isArray(list) ? list : [];
    paintRail();
    if (!S.current && S.sessions.length) {
      const last = lastSession();
      await selectSession(S.sessions.some((s) => s.id === last) ? last : S.sessions[0].id);
    } else {
      paintTopbar();
    }
  } catch (e) {
    toast(errText(e, 'Could not load sessions.'), 'err');
  }
}

/** Merge defined fields from an event into a rail row without a list refetch. */
function patchRail(id, patch) {
  if (!id) return null;
  const s = S.sessions.find((x) => x.id === id);
  if (!s) return null;
  for (const [k, v] of Object.entries(patch)) if (v !== undefined) s[k] = v;
  const li = row(id);
  if (li) patchRow(li, s);
  return s;
}

/* -------------------------------------------------------------------- topbar */

function paintTopbar() {
  const s = session();
  document.title = s ? `${s.title || 'Untitled'} · maki` : 'maki';
  els.title.textContent = s ? s.title || 'Untitled session' : 'No session';
  els.title.disabled = !s;
  els.kind.hidden = !s;
  if (s) {
    els.kind.textContent = KIND_LABEL[s.kind] || s.kind;
    els.kind.className = 'chip k-' + s.kind;
  }
  const live = !!s && s.kind === 'live';
  const web = !!s && s.kind === 'web';
  els.mode.hidden = !(live || web);
  els.mode.textContent = S.mode;
  els.mode.className = 'chip mode m-' + S.mode;
  els.plan.hidden = !live;
  els.kill.hidden = !web;
  paintTasksPanel();
  paintComposer();
}

/* -------------------------------------------------------------- sub-agents */

/** The panel belongs to live rows only: the gateway answers 501 for the rest. */
function paintTasksPanel() {
  const show = (session() || {}).kind === 'live';
  els.tasks.hidden = !show;
  if (show) return;
  els.tasksCount.textContent = '';
  els.tasksBody.dataset.for = '';
  els.tasksBody.replaceChildren();
}

function setTasksOpen(open) {
  tasksOpen = open;
  els.tasksBody.hidden = !open;
  els.tasksHead.setAttribute('aria-expanded', String(open));
}

function paintTasks(tasks) {
  const list = Array.isArray(tasks) ? tasks : [];
  els.tasksCount.textContent = String(list.length);
  els.tasksBody.replaceChildren(...taskRows(list));
}

async function loadTasks() {
  const id = S.current;
  const s = session(id);
  if (!s || s.kind !== 'live') return;
  // Rows belong to the session they were read for: drop another tab's set, then keep
  // what this tab has while the refresh is in flight, so the panel does not blink.
  if (els.tasksBody.dataset.for !== id) els.tasksBody.replaceChildren();
  els.tasksBody.dataset.for = id;
  if (!els.tasksBody.children.length) els.tasksBody.replaceChildren(el('p', 'dim', 'Loading…'));
  const mine = ++tasksReq;
  try {
    const r = await api.tasks(id);
    if (mine === tasksReq && id === S.current) paintTasks(r && r.tasks);
  } catch (e) {
    if (mine !== tasksReq || id !== S.current) return;
    els.tasksBody.replaceChildren(el('p', 'dim', e instanceof ApiError && e.status === 501
      ? 'This gateway does not report sub-agents.'
      : 'Could not load sub-agents: ' + e.message));
  }
}

/** Tasks changed: refetch once, after the stream settles. */
function scheduleTasks(id) {
  if (id !== S.current) return;
  debounce('t' + id, loadTasks);
}

function paintComposer() {
  const s = session();
  const readOnly = !s || s.kind === 'stored';
  els.input.disabled = readOnly;
  els.ro.hidden = !(s && s.kind === 'stored');
  els.send.hidden = readOnly;
  els.cancel.hidden = !s || s.kind !== 'web';
  els.input.placeholder = !s ? 'Select a session'
    : readOnly ? 'Read-only transcript'
      : s.status === 'working' ? 'Queued for the next turn'
        : 'Message';
}

function setMode(next) {
  const s = session();
  if (!s) return;
  const prev = S.mode;
  S.mode = next;
  paintTopbar();
  api.setMode(s.id, next).catch((e) => {
    S.mode = prev;
    paintTopbar();
    toast(errText(e, 'Could not change the mode.'), 'err');
  });
}

/* ------------------------------------------------------------------ composer */

function grow() {
  const t = els.input;
  t.style.height = '0px';
  t.style.height = Math.min(t.scrollHeight, MAX_GROW) + 'px';
}

async function send() {
  const s = session();
  const text = els.input.value.trim();
  if (!s || !text || els.input.disabled) return;
  saveCwd(s.cwd);
  els.input.value = '';
  drafts.delete(s.id);
  grow();
  closeStream();
  const bubble = { role: 'user', content: [{ type: 'text', text }], created_at: Date.now(), _pending: true, text };
  S.pending.push(bubble);
  paint();
  try {
    const r = await api.prompt(s.id, text);
    if (r && r.state === 'queued') toast('Queued as the next turn.');
  } catch (e) {
    S.pending = S.pending.filter((x) => x !== bubble);
    if (e instanceof ApiError && e.status === 409) {
      toast(e.message.includes('elsewhere') ? 'That session is open elsewhere.' : 'Busy: the session is mid-prompt.');
    } else {
      toast(errText(e, 'Could not send that.'), 'err');
    }
    if (!els.input.value) els.input.value = text;
    grow();
  }
  paint();
}

async function cancel() {
  const s = session();
  if (!s) return;
  try {
    const r = await api.cancel(s.id);
    toast(r && r.cancelled ? 'Cancellation requested.' : 'Nothing was running.');
  } catch (e) {
    if (e instanceof ApiError && e.status === 501) { els.cancel.hidden = true; return; }
    toast(errText(e, 'Cancel failed.'), 'err');
  }
}

async function kill() {
  const s = session();
  if (!s) return;
  const ok = await askYes('Kill this session?',
    `The gateway stops ${s.title || s.id}. The transcript stays on disk.`, 'Kill', true);
  if (!ok) return;
  try {
    await api.kill(s.id);
    toast('Session killed.', 'ok');
    await loadSessions();
  } catch (e) {
    toast(errText(e, 'Kill failed.'), 'err');
  }
}

async function rename() {
  const s = session();
  if (!s) return;
  const value = await askText('Rename session', s.title || '');
  if (value == null) return;
  patchRail(s.id, { title: value });
  paintTopbar();
  try {
    await api.setTitle(s.id, value === '' ? null : value);
    toast('Title saved.', 'ok');
  } catch (e) {
    toast(errText(e, 'Could not rename.'), 'err');
    loadSessions();
  }
}

/* ----------------------------------------------------------------------- sse */

function setOffline(v) {
  S.offline = v;
  els.offline.hidden = !v;
}

function subscribe() {
  if (events) events.close();
  events = new EventSource(withToken('/api/events'));
  events.addEventListener('maki', (e) => {
    let d;
    try { d = JSON.parse(e.data); } catch { return; }
    onEvent(d);
  });
  events.onopen = () => { setOffline(false); resync(); };
  events.onerror = () => setOffline(true);
}

/** Pull everything again. Used on connect, reconnect, and tab return. */
async function resync() {
  try {
    await api.health();
    setOffline(false);
  } catch { /* the banner clears when EventSource recovers */ }
  await loadSessions();
  if (S.current) {
    loadTasks();
    await loadMessages();
  }
}

/** One SSE envelope. Shapes: CONTRACTS section 2. */
function onEvent(ev) {
  if (!ev || !ev.type) return;
  switch (ev.type) {
    case 'update': {
      if (!ev.event || ev.session !== S.current || (ev.engine && ev.engine !== 'acp')) return;
      if (applyUpdate(ev.event) === 'mode') paintTopbar();
      paint();
      return;
    }
    case 'dirty':
      scheduleMessages(ev.session);
      return;
    case 'tasks':
      scheduleTasks(ev.session);
      return;
    case 'sessions':
      loadSessions();
      return;
    case 'status': {
      if (ev.status && ev.status !== 'working') closeStream();
      patchRail(ev.session, { status: ev.status, title: ev.title });
      if (ev.session === S.current) paintTopbar();
      return;
    }
    case 'notice':
      toast(ev.text || '');
      return;
    case 'exit': {
      closeStream();
      const s = patchRail(ev.session, { status: 'exited' });
      const code = ev.code === undefined || ev.code === null ? '' : ` with code ${ev.code}`;
      const name = (s && s.title) || (s && tail(s.cwd)) || 'Session';
      toast(`${name} exited${code}.`, 'err');
      if (ev.session === S.current) paintTopbar();
      loadSessions();
      return;
    }
    case 'permission':
      permissionModal(ev);
      return;
    case 'permission_cancel':
      permissionGone(ev.engine, ev.request_id);
      return;
    default:
      return;
  }
}

/* ------------------------------------------------------------------- chrome */

const narrow = () => window.matchMedia('(max-width: 767px)').matches;

function rail(open) {
  document.body.dataset.rail = open ? 'open' : 'closed';
  els.scrim.hidden = !(open && narrow());
}

/* --------------------------------------------------------------------- boot */

function boot() {
  const ids = {
    scrim: 'scrim', list: 'session-list', title: 'title-btn', kind: 'kind-chip',
    mode: 'mode-btn', plan: 'plan-btn', search: 'search-btn', stats: 'stats-btn', kill: 'kill-btn',
    offline: 'offline', retry: 'retry-btn', transcript: 'transcript', ro: 'readonly',
    input: 'input', cancel: 'cancel-btn', send: 'send-btn', fab: 'fab',
    tasks: 'tasks', tasksHead: 'tasks-head', tasksBody: 'tasks-body', tasksCount: 'tasks-count',
  };
  for (const [k, id] of Object.entries(ids)) els[k] = $(id);
  tasksOpen = !narrow();
  setTasksOpen(tasksOpen);
  rail(!narrow());
  $('rail-toggle').addEventListener('click', () => rail(document.body.dataset.rail !== 'open'));
  $('rail-close').addEventListener('click', () => rail(false));
  els.scrim.addEventListener('click', () => rail(false));
  els.title.addEventListener('click', rename);
  els.mode.addEventListener('click', () => setMode(S.mode === 'build' ? 'plan' : 'build'));
  els.plan.addEventListener('click', () => session() && planDrawer(S.current));
  els.tasksHead.addEventListener('click', () => setTasksOpen(!tasksOpen));
  els.search.addEventListener('click', () => searchOverlay(selectSession));
  els.stats.addEventListener('click', () => analyticsView());
  els.kill.addEventListener('click', kill);
  els.fab.addEventListener('click', () => newChat((id) => loadSessions().then(() => selectSession(id))));
  els.send.addEventListener('click', send);
  els.cancel.addEventListener('click', cancel);
  els.retry.addEventListener('click', resync);
  els.input.addEventListener('input', grow);
  els.input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
  });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') popLayer(); });
  window.addEventListener('resize', () => rail(document.body.dataset.rail === 'open'));
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    if (S.offline) subscribe();
    resync();
  });
  setInterval(() => {
    for (const s of S.sessions) {
      const li = row(s.id);
      if (li) li._when.textContent = ago(s.updated_at);
    }
  }, TICK_MS);
  paintTopbar();
  grow();
  subscribe();
  loadSessions();
  if (S.mock) toast('Mock mode: fake data, no gateway.', 'ok');
}

/** The one global this app owns. Handy from a devtools console. */
globalThis.maki = { S, api, selectSession, resync, loadMessages, version: 'ui-1' };

/** Mock mode also exposes the fake gateway, so a human can drive edge cases. */
function start() {
  if (!S.mock) return boot();
  return import('./mock.js').then((m) => {
    m.install();
    globalThis.maki.mock = {
      emit: m.emit, permission: m.fakePermission, expire: m.expire, drop: m.dropStream, quiet: m.quiet,
      tasks: m.tasksFor,
    };
    boot();
  }, () => boot());
}

start();
