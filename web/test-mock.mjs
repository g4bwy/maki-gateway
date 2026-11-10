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
  'plan-btn', 'search-btn', 'stats-btn', 'kill-btn', 'offline', 'retry-btn', 'transcript', 'readonly',
  'input', 'cancel-btn', 'send-btn', 'fab', 'overlays', 'toasts'];
check('index.html has every id app.js looks up', need.every((i) => !!el(i)), need.filter((i) => !el(i)).join(','));

await import(WEB + 'app.js');
const mock = await import(WEB + 'mock.js');
mock.quiet();
const f0 = globalThis.fetch;
globalThis.fetch = async (u, i) => {
  const r = await f0(u, i);
  if (r.status >= 400) console.log('HTTP' + r.status, (i && i.method) || 'GET', typeof u === 'string' ? u : u.url);
  return r;
};
await tick(600);
const M = globalThis.maki;
const tr = el('transcript');
const list = el('session-list');

/* boot: rail, auto-selected web session, rendered blocks */
check('rail rows', list.children.length === 3, list.children.length);
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
top().querySelector('textarea').value = 'use --dry-run';
btn('danger').fire('click');
await tick(600);
check('answered modal closed', el('overlays').childElementCount === 0 && M.S.modals.size === 0);
check('deny toast', has(el('toasts'), 'toast').some((t) => /Denied bash/.test(txt(t))), txt(has(el('toasts'), 'toast').at(-1)));

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
check('form has cwd and model fields', inputs.length === 2, top().kids.length && inputs.map((x) => x.tagName + ':' + x.placeholder).join(' | '));
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
btn('primary').fire('click');
check('turn completed after allow', await waitFor('close', () => M.S.modals.size === 0), [...M.S.modals.keys()].join());
check('usage footer for the turn', await waitFor('usage', () => has(tr, 'usage').length >= 1), dump());
check('usage footer shape', /\d+(\.\d+)?k? in · .+ out · .+ · \$/.test(txt(has(tr, 'usage')[0])), txt(has(tr, 'usage')[0]));

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
check('sessions event keeps row count', list.children.length === 4, list.children.length);
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
