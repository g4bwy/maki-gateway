/**
 * Self-check for the mock UI, no browser needed: `node test-mock.mjs`.
 *
 * A 150-line DOM shim stands in for the browser, so the real modules (app, store,
 * render, acp, ui) run their real code against mock.js. It asserts what a human
 * would click through: rail badges, block renderers and escaping, expand state
 * across a refetch, permission modals, toasts, dialogs, the read-only case.
 * Development only. Nothing in app.js or store.js imports this file.
 */
import { readFileSync } from 'node:fs';

const WEB = '/home/gab/test/maki-gateway/web/';
const byId = new Map();

class Node {
  constructor(tag) {
    this.tagName = (tag || '').toUpperCase();
    this.kids = [];
    this.parent = null;
    this.attrs = {};
    this.style = {};
    this.dataset = {};
    this.listeners = {};
    this.scrollHeight = 0;
    this._text = '';
    this._html = '';
    this._set = new Set();
    this.classList = {
      add: (...c) => c.forEach((x) => this._set.add(x)),
      remove: (...c) => c.forEach((x) => this._set.delete(x)),
      contains: (c) => this._set.has(c),
      toggle: (c, on) => (on === undefined
        ? (this._set.has(c) ? this._set.delete(c) : this._set.add(c))
        : (on ? this._set.add(c) : this._set.delete(c))),
    };
  }
  get className() { return [...this._set].join(' '); }
  set className(v) { this._set = new Set(String(v || '').split(' ').filter(Boolean)); }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k]; }
  get textContent() {
    let t = this._text;
    if (this._html) t += this._html.replace(/<[^>]*>/g, ' ');
    return t + this.kids.map((k) => k.textContent).join('');
  }
  set textContent(v) { this._text = String(v); this.kids = []; this._html = ''; }
  set innerHTML(v) { this._html = String(v); this.kids = []; }
  get innerHTML() { return this._html; }
  get children() { return this.kids.filter((k) => k.tagName); }
  get childElementCount() { return this.children.length; }
  get firstChild() { return this.kids[0] || null; }
  get lastChild() { return this.kids[this.kids.length - 1] || null; }
  get firstElementChild() { return this.children[0] || null; }
  get tHead() { return this.children.find((c) => c.tagName === 'THEAD') || null; }
  get tBodies() { return this.children.filter((c) => c.tagName === 'TBODY'); }
  get nextSibling() { return this.parent ? this.parent.kids[this.parent.kids.indexOf(this) + 1] || null : null; }
  append(...ns) { for (const n of ns) { if (n.parent) n.parent.removeChild(n); n.parent = this; this.kids.push(n); } }
  appendChild(n) { this.append(n); return n; }
  replaceChildren(...ns) { for (const k of this.kids) k.parent = null; this.kids = []; this.append(...ns); }
  insertBefore(n, ref) {
    if (n.parent) n.parent.removeChild(n);
    const i = ref ? this.kids.indexOf(ref) : this.kids.length;
    n.parent = this;
    this.kids.splice(i < 0 ? this.kids.length : i, 0, n);
    return n;
  }
  removeChild(n) { const i = this.kids.indexOf(n); if (i >= 0) this.kids.splice(i, 1); n.parent = null; return n; }
  remove() { if (this.parent) this.parent.removeChild(this); }
  addEventListener(t, f) { (this.listeners[t] ||= new Set()).add(f); }
  fire(t, ev = {}) { for (const f of this.listeners[t] || []) f({ target: this, preventDefault() {}, stopPropagation() {}, ...ev }); }
  focus() {}
  select() {}
  click() { this.fire('click'); }
  *[Symbol.iterator]() { yield* this.kids; }
  *all() { for (const k of this.kids) if (k.tagName) { yield k; yield* k.all(); } }
  matches(sel) {
    if (sel.startsWith('.')) return this.classList.contains(sel.slice(1));
    const m = /^\[data-id="(.*)"\]$/.exec(sel);
    if (m) return this.dataset.id === m[1];
    return this.tagName === sel.toUpperCase();
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) {
    const parts = sel.split(',').map((s) => s.trim().replace(/^:scope\s*>\s*/, ''));
    return [...this.all()].filter((n) => parts.some((p) => n.matches(p)));
  }
}

const html = readFileSync(WEB + 'index.html', 'utf8');
for (const m of html.matchAll(/<([a-z0-9]+)([^>]*?)>/g)) {
  const id = /id="([\w-]+)"/.exec(m[2]);
  if (!id) continue;
  const cls = /class="([\w -]+)"/.exec(m[2]);
  const n = new Node(m[1]);
  if (cls) n.className = cls[1];
  byId.set(id[1], n);
}
const el = (id) => byId.get(id);
const body = new Node('body');
body.append(el('app'));

globalThis.document = {
  body,
  title: '',
  hidden: false,
  activeElement: body,
  createElement: (t) => new Node(t),
  createComment: () => new Node(''),
  getElementById: (id) => byId.get(id) || null,
  addEventListener: () => {},
  querySelector: () => null,
};
globalThis.window = { matchMedia: () => ({ matches: false }), addEventListener: () => {} };
globalThis.location = { search: '?mock=1', href: 'http://localhost:8000/', pathname: '/' };
globalThis.localStorage = { m: new Map(), getItem(k) { return this.m.get(k) ?? null; }, setItem(k, v) { this.m.set(k, String(v)); } };
globalThis.requestAnimationFrame = (f) => setTimeout(f, 0);
globalThis.CSS = { escape: (s) => String(s).replace(/[^\w-]/g, (c) => '\\' + c) };

const tick = (ms) => new Promise((r) => setTimeout(r, ms));
let fails = 0;
const check = (name, cond, extra) => {
  if (cond) console.log('ok   ' + name);
  else { fails++; console.log('FAIL ' + name + (extra !== undefined ? ' <' + extra + '>' : '')); }
};
const has = (root, cls) => [...root.all()].filter((n) => n.classList.contains(cls));
const txt = (n) => (n ? n.textContent : '');
const all = (id) => txt(el(id));
const btn = (cls) => has(top(), cls)[0];
const top = () => el('overlays').children[el('overlays').childElementCount - 1] || el('overlays');
const waitFor = async (what, cond, ms = 9000) => {
  const until = Date.now() + ms;
  for (;;) {
    let v;
    try { v = cond(); } catch { v = false; }
    if (v) return v;
    if (Date.now() > until) return false;
    await tick(60);
  }
};

const need = ['rail-toggle', 'rail-close', 'session-list', 'scrim', 'title-btn', 'kind-chip', 'mode-btn',
  'yolo-btn', 'plan-btn', 'search-btn', 'stats-btn', 'kill-btn', 'offline', 'retry-btn', 'transcript', 'readonly',
  'input', 'cancel-btn', 'send-btn', 'fab', 'overlays', 'toasts',
  'tasks', 'tasks-head', 'tasks-body', 'tasks-count'];
check('index.html has every id app.js looks up', need.every((i) => !!el(i)), need.filter((i) => !el(i)).join(','));

await import(WEB + 'app.js');
const mock = await import(WEB + 'mock.js');
const store = await import(WEB + 'store.js');
mock.quiet();
const f0 = globalThis.fetch;
const calls = [];
globalThis.fetch = async (u, i) => {
  const url = typeof u === 'string' ? u : u.url;
  let body = null;
  try { body = i && i.body ? JSON.parse(i.body) : null; } catch { body = i && i.body; }
  calls.push({ method: (i && i.method) || 'GET', url, body });
  const r = await f0(u, i);
  if (r.status >= 400) console.log('HTTP' + r.status, (i && i.method) || 'GET', url);
  return r;
};
/** Last recorded call whose url matches, optionally filtered by method. */
const lastCall = (re, method) => calls.filter((c) => (!method || c.method === method) && re.test(c.url)).at(-1);
const callCount = (re) => calls.filter((c) => re.test(c.url)).length;
/** Run fn with fetch stubbed, and hand back the bodies it sent. */
const probeBodies = async (fn) => {
  const real = globalThis.fetch;
  const seen = [];
  globalThis.fetch = async (u, i) => {
    seen.push(i && i.body ? JSON.parse(i.body) : null);
    return new Response('{"session":"probe"}', { status: 201, headers: { 'content-type': 'application/json' } });
  };
  try { await fn(); } finally { globalThis.fetch = real; }
  return seen;
};
await tick(600);
const M = globalThis.maki;
const tr = el('transcript');
const list = el('session-list');

/* boot: rail, auto-selected web session, rendered blocks */
check('rail rows', list.children.length === 4, list.children.length);
check('rail dots carry kind', has(list, 'dot').every((d) => /dot (web|live|stored)/.test(d.className)));
check('rail status badge', has(list, 'st').some((s) => s.className.includes('working')));
check('rail cwd tail', has(list, 'rcwd').every((c) => c.textContent.length > 2), txt(has(list, 'rcwd')[0]));
check('rail relative time', has(list, 'rtime').every((c) => c.textContent.length > 0));
check('topbar title', all('title-btn').length > 4, all('title-btn'));
check('kind chip WEB', all('kind-chip') === 'WEB', all('kind-chip'));
check('web: kill visible', el('kill-btn').hidden === false);
check('web: cancel visible', el('cancel-btn').hidden === false);
check('web: plan hidden', el('plan-btn').hidden === true);
check('web: mode visible', el('mode-btn').hidden === false);
check('web: writable', el('readonly').hidden === true && el('input').disabled === false);
check('transcript rows', tr.children.length === 2, tr.children.length);
check('rail stays open on a wide screen', document.body.dataset.rail === 'open', document.body.dataset.rail);
check('markdown bold', has(tr, 'md').some((m) => m._html.includes('<strong>contract</strong>')));
check('markdown fenced code escaped', has(tr, 'md').some((m) => m._html.includes('<pre class="code"') && m._html.includes('&lt;script&gt;')));
check('markdown emits no live script', !has(tr, 'md').some((m) => /<script>/i.test(m._html)));
check('markdown link scheme filtered', has(tr, 'md').every((m) => !m._html.includes('javascript:')));
check('markdown italic', has(tr, 'md').some((m) => m._html.includes('<em>italic</em>')));
check('thinking block collapsed', has(tr, 'thinking').length === 1 && has(tr, 'fold-body')[0].hidden === true);
check('tool card with status chip', has(tr, 'tool').length === 1 && has(tr, 'tool')[0].className.includes('s-completed'), txt(has(tr, 'chip')[0]));
check('tool result folded into card', has(has(tr, 'tool')[0], 'res').length === 1);
check('short result not clipped', has(tr, 'clip').length === 0, has(tr, 'clip').length);
check('image placeholder', has(tr, 'imgph').length === 1);
check('cards collapsed by default', has(tr, 'card-body').every((b) => b.hidden === true));
check('expand set starts empty', M.S.open.size === 0);

/* expand survives a refetch; rows are reused, not rebuilt */
has(tr, 'card-head')[0].fire('click');
check('card expands', has(tr, 'card-body')[0].hidden === false);
check('expand remembered in state', M.S.open.size === 1, [...M.S.open][0]);
const row0 = tr.children[0];
const head0 = has(tr, 'card-head')[0];
await M.resync();
check('refetch reuses row nodes', tr.children[0] === row0 && has(tr, 'card-head')[0] === head0);
check('refetch keeps expansion', has(tr, 'card-body')[0].hidden === false);
check('refetch keeps source label', !!tr.dataset.source, tr.dataset.source);

/* busy 409: no leftover bubble, draft returned */
el('input').value = 'while busy';
el('send-btn').fire('click');
await tick(600);
check('busy toast', has(el('toasts'), 'toast').some((t) => /Busy/i.test(txt(t))), txt(has(el('toasts'), 'toast').at(-1)));
check('no bubble after 409', M.S.pending.length === 0, M.S.pending.length);
check('draft restored after 409', el('input').value === 'while busy', el('input').value);
el('input').value = '';

/* live session */
M.selectSession('live-1');
await tick(500);
check('kind chip LIVE', all('kind-chip') === 'LIVE', all('kind-chip'));
check('live: plan visible', el('plan-btn').hidden === false);
check('live: cancel hidden', el('cancel-btn').hidden === true);
check('live: kill hidden', el('kill-btn').hidden === true);
check('live: failed card is red', has(tr, 'tool').some((c) => c.className.includes('s-failed')));
check('live: error label on result', has(tr, 'card-label').some((c) => txt(c) === 'error'));
check('live: standalone error result clipped', has(tr, 'out').some((o) => o.className.includes('err')));
check('live: unknown block rendered raw', has(tr, 'fold').some((f) => txt(f).includes('weird_block')));
el('input').value = 'draft for live';
el('plan-btn').fire('click');
await tick(500);
check('plan drawer renders content', txt(top()).includes('reconcile'), txt(top()).slice(0, 60));
top().querySelector('.icon').fire('click');
check('drawer closed', el('overlays').childElementCount === 0);

/* live turn: dirty refetch brings the reply, a second send queues */
el('send-btn').fire('click');
await tick(400);
check('optimistic bubble while sending', M.S.pending.length === 1 || txt(tr).includes('draft for live'), M.S.pending.length);
el('input').value = 'second while busy';
el('send-btn').fire('click');
await tick(400);
check('queued toast', has(el('toasts'), 'toast').some((t) => /Queued as the next turn/.test(txt(t))), txt(has(el('toasts'), 'toast').at(-1)));
check('refetch delivered the attach reply', await waitFor('attach reply', () => txt(tr).includes('Mock attach reply for: draft for live')), txt(tr).slice(-140));
check('bubbles cleared after confirm', M.S.pending.length === 0, M.S.pending.length);

/* stored session */
M.selectSession('sto-1');
await tick(500);
check('kind chip STORED', all('kind-chip') === 'STORED', all('kind-chip'));
check('stored: read-only chip', el('readonly').hidden === false);
check('stored: input disabled', el('input').disabled === true);
check('stored: send hidden', el('send-btn').hidden === true);
check('stored: mode, kill, plan hidden', el('mode-btn').hidden && el('kill-btn').hidden && el('plan-btn').hidden);
check('stored: string content rendered', has(tr, 'md').length >= 2, tr.children.length);
check('stored: placeholder explains', el('input').placeholder.includes('Read-only'), el('input').placeholder);

/* per-session drafts */
M.selectSession('sto-1');
await tick(500);
el('input').value = '';
M.selectSession('live-1');
await tick(500);
el('input').value = 'draft kept here';
M.selectSession('sto-1');
await tick(400);
check('stored: draft cleared on switch', el('input').value === '', el('input').value);
M.selectSession('live-1');
await tick(400);
check('draft kept per session', el('input').value === 'draft kept here', el('input').value);
el('input').value = '';

/* sub-agents panel: live only, rows from GET /api/sessions/{id}/tasks */
const tasksCalls = () => callCount(/\/api\/sessions\/live-1\/tasks$/);
const trows = () => has(el('tasks-body'), 'task');
check('live: sub-agents panel visible', el('tasks').hidden === false);
check('the section is expanded by default on a wide screen', el('tasks-body').hidden === false);
check('task rows rendered from the tasks endpoint',
  await waitFor('task rows', () => trows().length === 2), txt(el('tasks-body')).slice(0, 70));
check('tasks fetched when the live session opens', tasksCalls() >= 1, tasksCalls());
check('task count badge', txt(el('tasks-count')) === '2', txt(el('tasks-count')));
check('every task row carries one status dot', trows().every((r) => has(r, 'st').length === 1));
check('working task reuses the spinner dot',
  trows().some((r) => has(r, 'st')[0].className.includes('st working')), txt(trows()[0]));
check('done task gets its own dot state',
  trows().some((r) => has(r, 'st')[0].className.includes('st done')), txt(trows()[1]));
check('known scalar fields show as name/value pairs',
  txt(trows()[0]).includes('name') && txt(trows()[0]).includes('audit the reconcile pass')
  && txt(trows()[0]).includes('01J8TQ') && txt(trows()[0]).includes('$0.0021'), txt(trows()[0]));
check('nested task fields fold behind JSON',
  has(el('tasks-body'), 'extra').length === 1 && txt(has(el('tasks-body'), 'extra')[0]).includes('probe'));
check('times read as relative labels', /\d+m|h/.test(txt(trows()[1])), txt(trows()[1]));

let base = tasksCalls();
await M.resync();
await tick(500);
check('resync refetches tasks for the open live session', tasksCalls() === base + 1, tasksCalls() - base);
mock.emit('tasks', { session: 'web-1' });
await tick(900);
check('tasks event for another session refetches nothing', tasksCalls() === base + 1, tasksCalls() - base);
base = tasksCalls();
mock.emit('tasks', { session: 'live-1' });
mock.emit('tasks', { session: 'live-1' });
mock.emit('tasks', { session: 'live-1' });
await tick(1500);
check('tasks events debounce into one refetch for the open session',
  tasksCalls() === base + 1, tasksCalls() - base);

mock.fakePermission('live-1', 777);
await tick(300);
const roundFrom = tasksCalls();
btn('primary').fire('click');
check('the answered bash round raises a tasks event the panel acts on',
  await waitFor('tasks after allow', () => tasksCalls() > roundFrom, 5000), tasksCalls() - roundFrom);
mock.fakePermission('live-1', 778);
await tick(300);
const deniedFrom = tasksCalls();
btn('danger').fire('click');
await tick(1400);
check('a denied bash round raises no tasks event', tasksCalls() === deniedFrom, tasksCalls() - deniedFrom);

mock.tasksFor('live-1', []);
mock.emit('tasks', { session: 'live-1' });
check('empty state after a tasks event',
  await waitFor('empty panel', () => txt(el('tasks-body')).includes('none running')), txt(el('tasks-body')));
check('empty state badge reads zero', txt(el('tasks-count')) === '0', txt(el('tasks-count')));

mock.tasksFor('live-1', [
  { id: 'x1', status: 'queued_by_backend', note: 'unknown status', flag: true, deep: { nested: [1, 2] } },
  'a bare string row',
  { label: 'no status at all' },
]);
mock.emit('tasks', { session: 'live-1' });
check('odd payloads still render rows', await waitFor('odd rows', () => trows().length === 3), txt(el('tasks-body')));
check('unknown task status still gets a dot', trows().every((r) => has(r, 'st').length === 1));
check('unlisted status falls back to the idle dot',
  has(trows()[0], 'st')[0].className.includes('idle'), has(trows()[0], 'st')[0].className);
check('unknown scalar fields show and nested ones fold',
  txt(trows()[0]).includes('unknown status') && txt(trows()[0]).includes('yes')
  && has(trows()[0], 'extra').length === 1, txt(trows()[0]));
check('a row that is not an object is shown raw', txt(trows()[1]).includes('bare string'), txt(trows()[1]));
check('a row with no status still shows its label', txt(trows()[2]).includes('no status at all'), txt(trows()[2]));

mock.tasksFor('live-1', [
  { id: 't-7', name: 'review the reconcile pass', status: 'working' },
  { id: 't-8', name: 'collect contract notes', status: 'done' },
]);
mock.emit('tasks', { session: 'live-1' });
check('a refresh keeps the rows on screen while it loads', trows().length === 3, trows().length);
check('restored rows render',
  await waitFor('two rows', () => trows().length === 2 && txt(el('tasks-body')).includes('review the reconcile pass')),
  txt(el('tasks-body')));
el('tasks-head').fire('click');
check('sub-agents section collapses',
  el('tasks-body').hidden === true && el('tasks-head').getAttribute('aria-expanded') === 'false');
el('tasks-head').fire('click');
check('sub-agents section expands again',
  el('tasks-body').hidden === false && trows().length === 2 && txt(el('tasks-count')) === '2', txt(el('tasks-count')));

const allTasks = () => callCount(/\/tasks$/);
M.selectSession('web-1');
const noTasksFor = allTasks();
await tick(500);
check('web: sub-agents panel hidden and emptied',
  el('tasks').hidden === true && el('tasks-body').childElementCount === 0);
check('no tasks request is sent for a web session', allTasks() === noTasksFor, allTasks() - noTasksFor);
M.selectSession('sto-1');
await tick(400);
check('stored: sub-agents panel hidden', el('tasks').hidden === true);
check('no tasks request is sent for a stored session', allTasks() === noTasksFor, allTasks() - noTasksFor);
const e501 = await store.api.tasks('web-1').then(() => null, (e) => e);
check('tasks for a web session answers 501', !!e501 && e501.status === 501, e501 && e501.status);
const switched = allTasks();
await M.selectSession('live-1');
check('panel returns on a switch back to live', el('tasks').hidden === false);
await tick(600);
check('switching back to live asks for the rows again', allTasks() === switched + 1, allTasks() - switched);
check('the live rows are on screen after the switch',
  await waitFor('rows back', () => trows().length === 2 && txt(el('tasks-body')).includes('review the reconcile pass')),
  txt(el('tasks-body')));
M.selectSession('web-1');
await tick(400);

/* task detail sheet: rows open a sheet, transcript shares the chat renderers, follow works */
const FOLLOW_CAVEAT = 'The terminal now sends typed input to this task';
const RUN_BANNER = 'The transcript is written when the run ends. You can follow it in the terminal meanwhile.';
const tsec = Math.floor(Date.now() / 1000);
const detailRows = [
  { id: 't-7', session: '01J8TQ', name: 'audit the reconcile pass', status: 'working', tool: 'task', cost: 0.0021, started_at: tsec - 90 },
  { id: 't-8', session: '01J8TQ', name: 'collect contract notes', status: 'done', tool: 'task', cost: 0.011, started_at: tsec - 400, ended_at: tsec - 60 },
];
const msgCalls = () => callCount(/\/tasks\/[^/]+\/messages$/);
const focusCalls = () => callCount(/\/focus$/);
const taskPathCalls = () => callCount(/\/api\/sessions\/[^/]+\/tasks\/[^/]+\//);
const rowNamed = (name) => trows().find((r) => txt(r).includes(name));
const sheetChips = () => has(el('tasks-body'), 'chip-mini');
/** Toasts expire, so count the caveat at insertion instead of reading the DOM later. */
let caveatCount = 0;
const toastsBox = el('toasts');
const realToastAppend = toastsBox.append;
toastsBox.append = (...ns) => {
  for (const n of ns) if (n && n.textContent === FOLLOW_CAVEAT) caveatCount++;
  return realToastAppend.call(toastsBox, ...ns);
};

mock.tasksFor('live-1', detailRows);
M.selectSession('live-1');
check('task rows with an id are buttons', await waitFor('button rows',
  () => trows().length === 2 && txt(el('tasks-body')).includes('audit the reconcile pass')
    && trows().every((r) => r.tagName === 'BUTTON' && r.type === 'button')), trows().map((r) => r.tagName).join());
check('no followed chip before anything happens', sheetChips().length === 0);

rowNamed('collect contract notes').fire('click');
check('a finished task sheet renders the transcript blocks', await waitFor('sheet blocks',
  () => has(top(), 'msg').length === 2 && has(top(), 'thinking').length === 1
    && has(top(), 'md').length >= 1 && has(top(), 'tool').length === 1), el('overlays').childElementCount + ' layers');
check('the sheet transcript came from the task messages endpoint', !!lastCall(/\/tasks\/t-8\/messages$/, 'GET'));
check('opening the sheet fetched the transcript exactly once', msgCalls() === 1, msgCalls());
const scard = has(top(), 'tool')[0];
check('the sheet tool card shows the failed result', scard.className.includes('s-failed')
  && has(scard, 'res').length === 1 && txt(scard).includes('connection refused'), txt(scard).slice(0, 80));
const shead = () => has(top(), 'sheet-head')[0];
check('the sheet header carries the task name', txt(top()).includes('collect contract notes'), txt(top()).slice(0, 60));
check('the sheet header shows the status dot', has(shead(), 'st').length === 1
  && /(^| )done( |$)/.test(has(shead(), 'st')[0].className), has(shead(), 'st')[0].className);
check('the sheet header shows cost, started and ended', txt(shead()).includes('$0.011')
  && txt(shead()).includes('started') && txt(shead()).includes('ended'), txt(shead()));
check('the sheet offers Follow for a live row', has(top(), 'follow').length === 1 && txt(has(top(), 'follow')[0]) === 'Follow');
check('the sheet offers a copy-id affordance', has(top(), 'copy-id').length === 1
  && has(top(), 'copy-id')[0].getAttribute('aria-label') === 'Copy task id');

const m0 = msgCalls();
mock.emit('tasks', { session: 'live-1' });
mock.emit('tasks', { session: 'live-1' });
check('a tasks event refreshes the open sheet transcript', await waitFor('sheet refetch', () => msgCalls() === m0 + 1), msgCalls() - m0);
await tick(700);
check('two tasks events refresh the open sheet once, on the same debounce', msgCalls() === m0 + 1, msgCalls() - m0);

has(top(), 'copy-id')[0].fire('click');
check('copy id toasts without throwing headless', await waitFor('copy toast',
  () => /task id copied|copy failed/i.test(txt(has(el('toasts'), 'toast').at(-1)))), txt(has(el('toasts'), 'toast').at(-1)));

const fCount = focusCalls();
has(top(), 'follow')[0].fire('click');
check('follow posts to the task focus endpoint', await waitFor('focus post', () => focusCalls() > fCount)
  && !!lastCall(/\/api\/sessions\/live-1\/tasks\/t-8\/focus$/, 'POST'), focusCalls() - fCount);
check('the followed row gets the chip', await waitFor('row chip',
  () => has(rowNamed('collect contract notes'), 'chip-mini').length === 1), txt(rowNamed('collect contract notes')).slice(0, 90));
check('the followed chip reads followed in terminal', txt(sheetChips()[0]) === 'followed in terminal', txt(sheetChips()[0]));
check('the sheet header shows the followed chip too', has(top(), 'chip-mini')[0].hidden === false);
check('the caveat toast uses the exact wording', await waitFor('caveat', () => caveatCount === 1), caveatCount);
const mFollow = msgCalls();
check('follow schedules one more transcript refresh', await waitFor('follow refetch', () => msgCalls() === mFollow + 1, 3800), msgCalls() - mFollow);

mock.emit('task_focus', { session: 'live-1', id: 't-7' });
check('the scripted event moves the chip to the named task', await waitFor('chip on t-7',
  () => has(rowNamed('audit the reconcile pass'), 'chip-mini').length === 1
    && has(rowNamed('collect contract notes'), 'chip-mini').length === 0), txt(el('tasks-body')).slice(0, 120));
check('the sheet header chip hides when another task is followed', has(top(), 'chip-mini')[0].hidden === true);
mock.emit('task_focus', { session: 'live-1', id: 'main' });
check('task_focus main clears every chip', await waitFor('chips gone', () => sheetChips().length === 0));

has(top(), 'follow')[0].fire('click');
check('a re-follow marks the chip again', await waitFor('chip back', () => has(top(), 'chip-mini')[0].hidden === false));
check('the caveat toasts once per session switch', await waitFor('refetch settles', () => caveatCount === 1, 3200)
  && caveatCount === 1, caveatCount);

mock.emit('task_focus', { session: 'live-1' });
mock.emit('task_focus', { session: 'live-1', id: 42 });
mock.emit('task_focus', { session: 'nope-x', id: 't-7', extra: { deep: 1 } });
await tick(250);
check('task_focus frames with no usable id or session change nothing', sheetChips().length === 1);

rowNamed('audit the reconcile pass').fire('click');
check('a running task shows the write-later banner, not an error', await waitFor('banner',
  () => has(top(), 'sheet-note').some((n) => txt(n) === RUN_BANNER) && has(top(), 'lerr').length === 0
    && has(top(), 'msg').length === 0), txt(top()).slice(0, 90));
check('the running banner keeps the info style', has(top(), 'info').length === 1);
top().querySelector('.icon').fire('click');
check('the close button dismisses the sheet', el('overlays').childElementCount === 0);

mock.tasksFor('live-1', detailRows.concat([{ id: 'x-9', name: 'dropped chat', status: 'done', ended_at: tsec - 30 }]));
mock.emit('tasks', { session: 'live-1' });
check('the panel takes the third row', await waitFor('three rows', () => trows().length === 3));
rowNamed('dropped chat').fire('click');
check('a finished task with no transcript records says so', await waitFor('no records',
  () => txt(top()).includes('No transcript records') && has(top(), 'lerr').length === 0), txt(top()).slice(0, 90));
mock.tasksFor('live-1', detailRows);

M.selectSession('sto-1');
check('switching sessions closes the sheet', el('overlays').childElementCount === 0);
M.selectSession('live-1');
check('the followed chip survives a round trip', await waitFor('chips back', () => sheetChips().length === 1), sheetChips().length);
rowNamed('collect contract notes').fire('click');
check('the sheet opens again on the way back', await waitFor('sheet back', () => has(top(), 'msg').length === 2));
has(top(), 'follow')[0].fire('click');
check('after a session switch a new follow toasts the caveat again',
  await waitFor('caveat 2', () => caveatCount === 2, 5000), caveatCount);
toastsBox.append = realToastAppend;

M.selectSession('web-1');
check('the web row hides every task control', el('overlays').childElementCount === 0 && el('tasks').hidden === true);
const webTaskPathFrom = taskPathCalls();
await tick(500);
check('no task detail request runs while a web session is open', taskPathCalls() === webTaskPathFrom, taskPathCalls() - webTaskPathFrom);
const tmWeb = await store.api.taskMessages('web-1', 't-8').catch(() => null);
check('task messages for a web session answer empty, not 404', !!tmWeb && Array.isArray(tmWeb.messages) && tmWeb.messages.length === 0);
const tm404 = await store.api.taskMessages('nope-9', 't-8').then(() => null, (e) => e);
check('task messages 404 only for an unknown session', !!tm404 && tm404.status === 404, tm404 && tm404.status);
const tfWeb = await store.api.taskFocus('web-1', 't-8').then(() => null, (e) => e);
check('focus on a web session answers 501', !!tfWeb && tfWeb.status === 501, tfWeb && tfWeb.status);
const tfNoTask = await store.api.taskFocus('live-1', 'nope-9').then(() => null, (e) => e);
check('focus on an unknown task 404s with the plugin wording', !!tfNoTask && tfNoTask.status === 404
  && tfNoTask.message === 'no task with id nope-9', tfNoTask && tfNoTask.message);
mock.oldPlugin('live-1');
const tfOld = await store.api.taskFocus('live-1', 't-8').then(() => null, (e) => e);
check('a TUI without the updated plugin 404s verbatim', !!tfOld && tfOld.status === 404
  && tfOld.message === 'no interactive UI attached', tfOld && tfOld.message);
mock.oldPlugin('live-1', false);
const tfMain = await store.api.taskFocus('live-1', 'main');
check('following the main chat succeeds', !!tfMain && tfMain.ok === true);
await tick(400);
const rowsSeen = await store.api.tasks('live-1');
check('the two detail rows remain what live-1 reports', rowsSeen.tasks.length === 2 && rowsSeen.tasks[0].id === 't-7', txt(el('tasks-body')));

/* mode chip */
M.selectSession('web-1');
await tick(500);
el('mode-btn').fire('click');
await tick(400);
check('mode toggles to plan', M.S.mode === 'plan' && all('mode-btn') === 'plan', all('mode-btn'));
check('mode chip restyled', el('mode-btn').className.includes('m-plan'));

/* permission modal: deny with guidance */
mock.fakePermission('live-1');
await tick(300);
check('permission layer opened', el('overlays').childElementCount === 1);
check('permission names the tool', txt(top()).includes('bash'), txt(top()).slice(0, 60));
check('permission shows JSON input', has(top(), 'out').some((o) => txt(o).includes('git push')));
check('modal tracked by engine:id', [...M.S.modals.keys()].join() === 'attach:101', [...M.S.modals.keys()].join());
check('live modal: two buttons only', has(top(), 'btn').length === 2, has(top(), 'btn').map((b) => txt(b)).join('|'));
check('live modal hides Always allow', !txt(top()).includes('Always allow'), txt(top()).slice(0, 90));
check('live modal: Allow once is primary', txt(btn('primary')) === 'Allow once', txt(btn('primary')));
top().querySelector('textarea').value = 'use --dry-run';
btn('danger').fire('click');
await tick(600);
check('answered modal closed', el('overlays').childElementCount === 0 && M.S.modals.size === 0);
check('deny toast', has(el('toasts'), 'toast').some((t) => /Denied bash/.test(txt(t))), txt(has(el('toasts'), 'toast').at(-1)));
const denyCall = lastCall(/api\/permissions\/attach\/101$/);
check('deny posts deny with the guidance',
  !!denyCall && denyCall.body.decision === 'deny' && denyCall.body.guidance === 'use --dry-run',
  JSON.stringify(denyCall && denyCall.body));

/* stacked modals and permission_cancel */
mock.fakePermission('live-1', 108);
mock.fakePermission('live-1', 109);
await tick(300);
check('two modals stacked', M.S.modals.size === 2, [...M.S.modals.keys()].join());
mock.emit('permission_cancel', { engine: 'attach', request_id: 108 });
await tick(300);
check('cancel closed one only', M.S.modals.size === 1 && M.S.modals.has('attach:109'), [...M.S.modals.keys()].join());
mock.emit('permission_cancel', { engine: 'attach', request_id: 109 });
await tick(300);
check('cancel closed the rest', M.S.modals.size === 0 && el('overlays').childElementCount === 0);

/* answering an expired request */
mock.fakePermission('live-1', 902);
await tick(300);
mock.expire('attach', 902);
btn('primary').fire('click');
await tick(600);
check('expired answer closes with a toast', M.S.modals.size === 0 && has(el('toasts'), 'toast').some((t) => /expired/i.test(txt(t))), txt(has(el('toasts'), 'toast').at(-1)));
const liveAllow = lastCall(/api\/permissions\/attach\/902$/);
check('live Allow once posts allow with no guidance',
  !!liveAllow && liveAllow.body.decision === 'allow' && !('guidance' in liveAllow.body),
  JSON.stringify(liveAllow && liveAllow.body));

/* search overlay */
el('search-btn').fire('click');
await tick(200);
const q = top().querySelector('input');
q.value = 'contract';
q.fire('input');
await waitFor('hits', () => has(top(), 'hit').length > 0, 3000);
const hits = has(top(), 'hit');
check('search returned hits', hits.length >= 1, hits.length);
if (hits.length) {
  hits[0].fire('click');
  await tick(600);
  check('hit opened its session and closed the layer', el('overlays').childElementCount === 0 && M.S.current === 'web-1', M.S.current);
}

/* analytics */
el('stats-btn').fire('click');
await tick(800);
const tables = el('overlays').querySelectorAll('table');
check('analytics cards', has(top(), 'stat').length === 6, has(top(), 'stat').length);
check('analytics daily table has 30 rows', tables[1].children[1].children.length === 30, tables[1].children[1].children.length);
check('analytics money format', /\$\d/.test(txt(top())));
top().querySelector('.icon').fire('click');

/* rename */
el('title-btn').fire('click');
await tick(200);
const ri = top().querySelector('input');
ri.value = 'Renamed here';
ri.fire('keydown', { key: 'Enter' });
await tick(600);
check('rename in topbar', all('title-btn') === 'Renamed here', all('title-btn'));
check('rename in rail', txt(list).includes('Renamed here'));

/* new chat: validation, then create and stream */
el('fab').fire('click');
await tick(200);
console.log('DBG layers', el('overlays').childElementCount, 'heads', has(el('overlays'), 'lhead').map((h) => txt(h).slice(0, 20)));
const inputs = top().querySelectorAll('input');
console.log('LAYERS', el('overlays').childElementCount, has(el('overlays'),'lhead').map((h)=>txt(h)));
check('form has cwd, model, and a yolo checkbox',
  inputs.length === 3 && inputs[0].name === 'cwd' && inputs[1].name === 'model'
  && inputs[2].name === 'yolo' && inputs[2].type === 'checkbox',
  inputs.map((x) => x.tagName + ':' + x.name).join(' | '));
check('yolo checkbox starts unticked for a new cwd', inputs[2].checked === false, String(inputs[2].checked));
check('yolo label explains the rule', txt(top()).includes('yolo — skip permission prompts, deny rules still apply'),
  txt(top()).replace(/\s+/g, ' ').slice(0, 120));
inputs[0].value = '';
btn('primary').fire('click');
await tick(300);
check('cwd required blocks the submit', el('overlays').childElementCount === 1 && txt(top()).includes('required'));
inputs[0].value = '/tmp/phone-ui';
top().querySelector('textarea').value = 'first prompt';
btn('primary').fire('click');
await waitFor('layer closed', () => el('overlays').childElementCount === 0);
check('dialog closed after create', el('overlays').childElementCount === 0);
check('created session is current', await waitFor('selection', () => /^web-\d{3}$/.test(M.S.current) && M.S.current !== 'web-1'), M.S.current);
check('cwd remembered', globalThis.localStorage.getItem('maki.cwd') === '/tmp/phone-ui');
const createCall = lastCall(/\/api\/sessions$/, 'POST');
check('unticked dialog create sends no yolo key',
  !!createCall && !('yolo' in createCall.body)
  && createCall.body.cwd === '/tmp/phone-ui' && createCall.body.prompt === 'first prompt',
  JSON.stringify(createCall && createCall.body));
check('yolo choice remembered for that cwd',
  JSON.parse(globalThis.localStorage.getItem('maki.yolo'))['/tmp/phone-ui'] === false,
  globalThis.localStorage.getItem('maki.yolo'));
const probed = await probeBodies(() => store.api.create('/tmp/probe', '', '', false));
check('unticked create sends no yolo key', !('yolo' in probed[0]), JSON.stringify(probed[0]));
check('ticked create sends yolo true',
  (await probeBodies(() => store.api.create('/tmp/probe', '', '', true)))[0].yolo === true);
store.saveYolo('/tmp/other', false);
check('yolo memory is kept per cwd',
  store.yoloFor('/tmp/other') === false && store.yoloFor('/tmp/never-set') === undefined,
  globalThis.localStorage.getItem('maki.yolo'));
store.saveYolo('/tmp/phone-ui', true);
el('fab').fire('click');
await tick(200);
check('reopened dialog ticks yolo for the known cwd',
  top().querySelectorAll('input')[2].checked === true, String(top().querySelectorAll('input')[2].checked));
top().querySelector('.icon').fire('click');
await tick(150);
check('reopened dialog closed without creating',
  el('overlays').childElementCount === 0 && /^web-\d{3}$/.test(M.S.current), M.S.current);
const dump = () => M.S.current + ' rows=' + tr.children.length + ' :: ' + tr.children.map((c) => txt(c).slice(0, 46)).join(' // ');
check('first prompt shown', await waitFor('prompt', () => txt(tr).includes('first prompt')), dump());
check('acp chunks streamed', await waitFor('chunks', () => txt(tr).includes('Reading the contract and the current renderer now.')), dump());
check('acp thought block', has(tr, 'thinking').length >= 1, dump());
check('acp tool card', await waitFor('tool card', () => has(tr, 'tool').length >= 1), dump());
check('acp chip states', await waitFor('chips', () => has(tr, 'chip').some((c) => /s-(queued|in_progress|completed)/.test(c.className))));
check('streaming row marked', has(tr, 'msg').some((m) => m.className.includes('stream')));
check('acp tool output shown', await waitFor('tool output', () => has(tr, 'card-body').some((b) => txt(b).includes('paintBody'))));
check('permission asked after ~10s', await waitFor('permission', () => M.S.modals.size === 1, 14000), dump());
check('modal scrolls long input', has(top(), 'out').length >= 1);
check('web modal: three buttons', has(top(), 'btn').length === 3, has(top(), 'btn').map((b) => txt(b)).join('|'));
check('web modal: Always allow is the primary', txt(btn('primary')) === 'Always allow', txt(btn('primary')));
check('web modal: Allow once is offered too',
  has(top(), 'btn').some((b) => txt(b) === 'Allow once'), has(top(), 'btn').map((b) => txt(b)).join('|'));
btn('primary').fire('click');
check('turn completed after allow', await waitFor('close', () => M.S.modals.size === 0), [...M.S.modals.keys()].join());
const alwaysCall = lastCall(/\/api\/permissions\/acp\//);
check('always allow posts allow_always',
  !!alwaysCall && alwaysCall.body.decision === 'allow_always', JSON.stringify(alwaysCall && alwaysCall.body));
check('allow toast names the choice',
  has(el('toasts'), 'toast').some((t) => /Always allowed edit/.test(txt(t))), txt(has(el('toasts'), 'toast').at(-1)));
check('usage footer for the turn', await waitFor('usage', () => has(tr, 'usage').length >= 1), dump());
check('usage footer shape', /\d+(\.\d+)?k? in · .+ out · .+ · \$/.test(txt(has(tr, 'usage')[0])), txt(has(tr, 'usage')[0]));

/* yolo create survives the mock: that session answers its prompts unasked */
const yoloRes = await store.api.create('/tmp/yolo-dir', '', 'first prompt', true);
const yid = yoloRes && yoloRes.session;
check('ticked create reaches the wire with yolo true',
  (lastCall(/\/api\/sessions$/, 'POST') || { body: {} }).body.yolo === true,
  JSON.stringify((lastCall(/\/api\/sessions$/, 'POST') || {}).body));
check('mock creates a yolo session', /^web-\d{3}$/.test(yid || ''), yid);
let yoloWorked = false;
check('yolo turn runs to its end with no dialog', await waitFor('yolo over', () => {
  const s = M.S.sessions.find((x) => x.id === yid) || {};
  if (s.status === 'working') yoloWorked = true;
  return yoloWorked && s.status === 'idle';
}), yid + ' ' + JSON.stringify(M.S.sessions.find((x) => x.id === yid)));
check('no permission modal opened for the yolo session',
  M.S.modals.size === 0 && el('overlays').childElementCount === 0, [...M.S.modals.keys()].join());
await M.selectSession(yid);
await tick(500);
check('the yolo session says it answered itself', txt(tr).includes('answered itself'), txt(tr).slice(-90));

/* yolo chip in the top bar: a web row toggles, a live row explains, stored has none */
const YOLO_TIP_LIVE = 'toggle /yolo in the terminal — the web cannot change a running TUI session';
const chip = () => el('yolo-btn');
const checked = () => chip().getAttribute('aria-checked');
const yoloCalls = () => callCount(/\/yolo$/);
const rowYolo = (id) => (M.S.sessions.find((x) => x.id === id) || {}).yolo;
check('the YOLO chip follows the mode chip in the markup',
  /id="mode-btn"[\s\S]{0,200}?id="yolo-btn"[^>]*>YOLO</.test(html));
M.selectSession('web-1');
await tick(500);
check('web row shows the yolo chip as a switch that is off', chip().hidden === false && chip().disabled === false
  && checked() === 'false' && !chip().className.includes('on'), chip().hidden + ' ' + checked());
check('yolo chip carries a name, a state, and a tooltip', chip().getAttribute('aria-label') === 'YOLO mode'
  && chip().getAttribute('aria-disabled') === 'false' && chip().getAttribute('title').includes('YOLO'),
  chip().getAttribute('aria-label') + ' | ' + chip().getAttribute('title'));
const busyFrom = yoloCalls();
chip().fire('click');
check('tapping the chip posts', await waitFor('yolo post', () => yoloCalls() > busyFrom), yoloCalls() - busyFrom);
check('an off web row posts {on:true}',
  (lastCall(/\/api\/sessions\/web-1\/yolo$/, 'POST') || { body: {} }).body.on === true,
  JSON.stringify((lastCall(/\/yolo$/, 'POST') || {}).body));
check('busy keeps the chip off and names what to do', await waitFor('busy chip', () => checked() === 'false'
  && has(el('toasts'), 'toast').some((t) => /finish or cancel the current turn first/.test(txt(t)))),
  txt(has(el('toasts'), 'toast').at(-1)));
check('a rejected toggle leaves the row untouched', !rowYolo('web-1'), String(rowYolo('web-1')));
M.selectSession('live-1');
await tick(500);
check('live row shows the chip disabled', chip().hidden === false && chip().disabled === true
  && chip().getAttribute('aria-disabled') === 'true' && checked() === 'false', String(chip().disabled));
check('the live chip points at the terminal', chip().getAttribute('title') === YOLO_TIP_LIVE
  && chip().getAttribute('aria-label') === YOLO_TIP_LIVE, chip().getAttribute('title'));
const liveFrom = yoloCalls();
chip().fire('click');
await tick(600);
check('a disabled chip sends no request', yoloCalls() === liveFrom, yoloCalls() - liveFrom);
M.selectSession('sto-1');
await tick(500);
check('stored row has no yolo chip at all', chip().hidden === true, String(chip().hidden));
M.selectSession('web-yolo-1');
await tick(500);
check('the seeded yolo row reads on from the payload', chip().hidden === false && chip().disabled === false
  && checked() === 'true' && chip().className.includes('on'), checked());
chip().fire('click');
const offPost = await waitFor('yolo off post', () => lastCall(/\/api\/sessions\/web-yolo-1\/yolo$/, 'POST'), 3000);
check('tapping an on chip posts {on:false}', !!offPost && offPost.body.on === false,
  JSON.stringify(offPost && offPost.body));
check('the chip takes the state the reply reports', await waitFor('chip off', () => checked() === 'false'
  && !chip().className.includes('on') && !rowYolo('web-yolo-1')), checked() + ' ' + rowYolo('web-yolo-1'));
await store.api.setYolo('web-yolo-1', true);
check('a sessions refetch moves the chip with no click',
  await waitFor('chip on from the payload', () => checked() === 'true' && chip().className.includes('on')),
  checked());
M.selectSession(yid);
await tick(500);
check('a chat created with yolo ticks the chip from its own row',
  chip().hidden === false && chip().disabled === false && checked() === 'true', M.S.current + ' ' + rowYolo(yid));
const fetchReal = globalThis.fetch;
globalThis.fetch = async () => { throw new TypeError('gateway down'); };
chip().fire('click');
check('a network failure toasts and changes nothing', await waitFor('net toast', () => checked() === 'true'
  && has(el('toasts'), 'toast').some((t) => /gateway down/.test(txt(t)))), txt(has(el('toasts'), 'toast').at(-1)));
globalThis.fetch = fetchReal;

/* cancel then kill a web session */
el('cancel-btn').fire('click');
await tick(600);
check('cancel toast', has(el('toasts'), 'toast').some((t) => /Cancel|Nothing/.test(txt(t))), txt(has(el('toasts'), 'toast').at(-1)));
el('kill-btn').fire('click');
await tick(300);
check('kill asks before acting', el('overlays').childElementCount === 1 && txt(top()).includes('Kill this session'));
btn('danger').fire('click');
await tick(1200);
check('exit toast carries the code', has(el('toasts'), 'toast').some((t) => /exited with code 137/.test(txt(t))), txt(has(el('toasts'), 'toast').at(-1)));
check('rail row turns red on exit', has(list, 'st').some((s) => s.className.includes('exited')) && list.children.some((r) => r.className.includes('exited')));

/* SSE lifecycle */
mock.dropStream(1);
await tick(300);
check('offline banner on stream error', el('offline').hidden === false);
await tick(1900);
check('banner clears after reconnect', el('offline').hidden === true);
mock.emit('notice', { text: 'plugin disconnected' });
await tick(300);
check('notice becomes a toast', txt(has(el('toasts'), 'toast').at(-1)) === 'plugin disconnected');
mock.emit('status', { session: 'live-1', status: 'working', title: 'Renamed by event' });
await tick(400);
check('status event patches the rail in place', txt(list).includes('Renamed by event'));
const rowsBefore = [...list.children];
mock.emit('sessions', {});
await tick(800);
// web-1 + live-1 + sto-1 + web-yolo-1 + the two sessions this run created (one plain, one yolo)
check('sessions event keeps row count', list.children.length === 6, list.children.length);
check('sessions event reuses rows', rowsBefore.every((n, i) => list.children[i] === n));
check('no leaked layers', el('overlays').childElementCount === 0, txt(el('overlays')).slice(0, 60));
const rowsNow = M.S.messages.length;
mock.emit('dirty', { session: 'sto-1', reason: 'turn_end' });
await tick(900);
check('dirty for another session is ignored', M.S.messages.length === rowsNow);
mock.emit('bogus', { x: 1 });
check('unknown event type ignored', true);

/* narrow layout: the rail becomes an overlay drawer */
window.matchMedia = () => ({ matches: true });
await M.selectSession('live-1');
check('narrow: selecting closes the drawer', document.body.dataset.rail === 'closed' && el('scrim').hidden === true, document.body.dataset.rail);
el('rail-toggle').fire('click');
check('narrow: hamburger opens the drawer', document.body.dataset.rail === 'open' && el('scrim').hidden === false);
el('rail-close').fire('click');
check('narrow: close collapses it', document.body.dataset.rail === 'closed' && el('scrim').hidden === true);
el('rail-toggle').fire('click');
el('scrim').fire('click');
check('narrow: scrim dismisses', document.body.dataset.rail === 'closed');
el('tasks-head').fire('click');
check('narrow: sub-agents collapses on its own, the rail stays shut',
  document.body.dataset.rail === 'closed' && el('tasks-body').hidden === true);
el('tasks-head').fire('click');
check('narrow: sub-agents expands again', el('tasks-body').hidden === false);
check('narrow: the rows are still there after expanding',
  await waitFor('rows after expand', () => trows().length >= 1), txt(el('tasks-body')).slice(0, 60));

await tick(120);
check('transcript still rendered after rail work', tr.children.length >= 2, tr.children.length + ' rows, current=' + M.S.current);
check('row text still present', txt(tr).length > 20, JSON.stringify(txt(tr).slice(0, 60)));

/* render unit: tool cards keep their state across repaints with pooled rows.
   A tool_use and its tool_result are separate messages on the wire (CONTRACTS 3),
   so this drives that shape directly instead of through the mock sessions. */
const { renderTranscript } = await import(WEB + 'render.js');
const saveMsgs = M.S.messages;
const savePend = M.S.pending;
M.S.pending = [];
const useRow = {
  role: 'assistant', created_at: 1700000001000,
  content: [{ type: 'tool_use', id: 'tu_d', name: 'bash', status: 'completed', input: { command: 'git push' } }],
};
const resRow = {
  role: 'user', created_at: 1700000002000,
  content: [{ type: 'tool_result', tool_use_id: 'tu_d', content: 'denied by user', is_error: true }],
};
M.S.messages = [useRow, resRow];
const probe = document.createElement('div');
renderTranscript(probe);
const pcard = has(probe, 'tool')[0];
check('failed result marks the tool card failed', !!pcard && pcard.className.includes('s-failed')
  && !pcard.className.includes('s-completed'), pcard && pcard.className);
check('failed result rewrites the chip text', txt(has(pcard, 'chip')[0]) === 'failed', txt(has(pcard, 'chip')[0]));
check('result body lands inside the card', has(pcard, 'res').length === 1 && txt(pcard).includes('denied by user'), txt(has(pcard, 'res')[0]));

const useNode = probe.children[0];
const resNode = probe.children[1];
resRow.content = [{ type: 'tool_result', tool_use_id: 'tu_d', content: 'denied by user (asked twice)', is_error: true }];
renderTranscript(probe);
check('repaint reuses both pooled rows', probe.children[0] === useNode && probe.children[1] === resNode);
check('pooled card restore reattaches the new result', has(pcard, 'res').length === 1
  && txt(pcard).includes('asked twice') && has(probe, 'result').length === 0,
has(pcard, 'res').length + ' res, ' + txt(pcard).slice(0, 60));
check('pooled repaint keeps the failed chip', txt(has(pcard, 'chip')[0]) === 'failed', txt(has(pcard, 'chip')[0]));

useRow.content = [{ type: 'tool_use', id: 'tu_d', name: 'bash', status: 'failed', is_error: true, input: { command: 'git push' } }];
renderTranscript(probe);
const rebuilt = has(probe, 'tool')[0];
check('repainted card rebuilds with its result attached', rebuilt !== pcard
  && has(rebuilt, 'res').length === 1 && txt(rebuilt).includes('asked twice')
  && has(probe, 'result').length === 0, txt(rebuilt).slice(0, 80));
check('rebuilt card keeps the failed chip', txt(has(rebuilt, 'chip')[0]) === 'failed'
  && rebuilt.className.includes('s-failed'), txt(has(rebuilt, 'chip')[0]));

const orphan = {
  role: 'user', created_at: 1700000003000,
  content: [{ type: 'tool_result', tool_use_id: 'tu_gone', content: 'no tool_use in this window', is_error: true }],
};
M.S.messages = [orphan];
const probe2 = document.createElement('div');
renderTranscript(probe2);
const solo = has(probe2, 'result')[0];
check('orphan result rebuilds a failed card of its own', !!solo && solo.className.includes('s-failed')
  && txt(has(solo, 'chip')[0]) === 'failed' && txt(solo).includes('no tool_use in this window')
  && has(solo, 'card-label').length === 1 && txt(has(solo, 'card-label')[0]) === 'error', txt(solo).slice(0, 60));
M.S.messages = saveMsgs;
M.S.pending = savePend;

console.log('\n--- transcript tail ---');
console.log(tr.children.slice(-3).map((c) => c.className + ' :: ' + txt(c).replace(/\s+/g, ' ').slice(0, 100)).join('\n'));
console.log('\n' + (fails ? fails + ' CHECKS FAILED' : 'ALL CHECKS PASSED'));
process.exit(fails ? 1 : 0);
