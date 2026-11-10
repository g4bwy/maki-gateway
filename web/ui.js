/**
 * All non-transcript UI: toasts, modal layers, dialogs, permission requests,
 * plan drawer, search overlay, analytics view, new chat form.
 * Nothing here knows about SSE or state sync; app.js calls these.
 */
import { S, api, ApiError, json, ago, money, num, lastCwd, saveCwd } from './store.js';
import { el, markdownEl, codeText } from './render.js';

const TOAST_MS = 4200;
const SEARCH_MS = 300;
const stack = [];

const host = (id) => document.getElementById(id);

/** Show a transient message. kind: '' | 'err' | 'ok'. */
export function toast(text, kind) {
  const t = el('div', 'toast' + (kind ? ' ' + kind : ''), text);
  host('toasts').append(t);
  setTimeout(() => {
    t.classList.add('out');
    setTimeout(() => t.remove(), 300);
  }, TOAST_MS);
}

/** Report a failed call without throwing at the caller. */
export function fail(e, fallback) {
  toast((e instanceof ApiError ? e.message + (e.status ? ' (' + e.status + ')' : '') : null) || fallback || 'Request failed', 'err');
}

/**
 * Open a modal layer.
 * @param {string} cls size class: sheet | drawer | full
 * @param {string} title heading text
 * @param {Array} nodes body content
 * @param {Function} [onclose] called when dismissed
 */
export function layer(cls, title, nodes, onclose) {
  const back = el('div', 'back');
  const box = el('section', 'layer ' + cls);
  box.setAttribute('role', 'dialog');
  box.setAttribute('aria-modal', 'true');
  box.setAttribute('aria-label', title || 'Dialog');
  const head = el('div', 'lhead');
  head.append(el('h2', '', title || ''));
  const x = el('button', 'icon', 'Close');
  x.type = 'button';
  x.setAttribute('aria-label', 'Close');
  x.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>';
  head.append(el('span', 'spacer'), x);
  const body = el('div', 'lbody');
  body.append(...nodes);
  box.append(head, body);
  back.append(box);
  host('overlays').append(back);
  const entry = { close };
  stack.push(entry);
  const was = document.activeElement;
  document.body.dataset.locked = '1';
  x.addEventListener('click', close);
  back.addEventListener('click', (e) => { if (e.target === back) close(); });
  function close() {
    const i = stack.indexOf(entry);
    if (i < 0) return;
    stack.splice(i, 1);
    if (!stack.length) delete document.body.dataset.locked;
    back.remove();
    if (onclose) onclose();
    if (was && was.focus) was.focus();
  }
  setTimeout(() => (body.querySelector('input,textarea,button.primary') || x).focus(), 0);
  return { box, body, close };
}

/** Close the topmost layer. Returns true when one was open. */
export function popLayer() {
  const top = stack[stack.length - 1];
  if (!top) return false;
  top.close();
  return true;
}

/** Section used by the read-only views; content arrives as HTML text. */
export const mdBox = (text) => markdownEl(document.createElement('div'), text);

/** Single-value text dialog (rename). Resolves null on cancel or dismiss. */
export function askText(title, value, okLabel) {
  return new Promise((resolve) => {
    const input = el('input', '');
    input.type = 'text';
    input.value = value || '';
    const actions = el('div', 'actions');
    let settled = false;
    const l = layer('sheet', title, [input, actions], () => { if (!settled) resolve(null); });
    const done = (v) => { settled = true; l.close(); resolve(v); };
    const ok = button(okLabel || 'Save', 'primary', () => done(input.value.trim()));
    actions.append(button('Cancel', '', () => done(null)), ok);
    input.addEventListener('keydown', (e) => { if (e.key === 'Enter') done(input.value.trim()); });
    input.select();
  });
}

/** Yes/no dialog. No native confirm anywhere. */
export function askYes(title, body, okLabel, danger) {
  return new Promise((resolve) => {
    const actions = el('div', 'actions');
    let settled = false;
    const l = layer('sheet', title, [el('p', 'lead', body), actions], () => { if (!settled) resolve(false); });
    const yes = button(okLabel || 'Confirm', 'primary' + (danger ? ' danger' : ''), () => {
      settled = true; l.close(); resolve(true);
    });
    actions.append(button('Cancel', '', () => { settled = true; l.close(); resolve(false); }), yes);
    if (danger) yes.focus();
  });
}

function button(label, cls, onclick) {
  const b = el('button', 'btn' + (cls ? ' ' + cls : ''), label);
  b.type = 'button';
  b.addEventListener('click', onclick);
  return b;
}

const field = (label, input) => {
  const wrap = el('label', 'field');
  wrap.append(el('span', 'flabel', label), input);
  return wrap;
};
const textInput = (name, value, placeholder, type) => {
  const i = el('input', '');
  i.name = name;
  i.type = type || 'text';
  i.value = value || '';
  i.placeholder = placeholder || '';
  i.autocomplete = 'off';
  return i;
};
const areaInput = (name, value, placeholder) => {
  const t = el('textarea', 'grow');
  t.name = name;
  t.value = value || '';
  t.placeholder = placeholder || '';
  t.rows = 3;
  return t;
};

/** New chat dialog: cwd required and prefilled, model and first prompt optional. */
export function newChat(onCreated) {
  const cwd = textInput('cwd', lastCwd(), '/path/to/project');
  const model = textInput('model', '', 'provider/model (optional)');
  const prompt = areaInput('prompt', '', 'First message (optional)');
  const err = el('p', 'lerr');
  err.hidden = true;
  const form = el('form', 'form');
  form.append(field('Working directory', cwd), field('Model', model), field('First prompt', prompt), err);
  const actions = el('div', 'actions');
  const l = layer('sheet', 'New chat', [form, actions]);
  const go = button('Start', 'primary', submit);
  actions.append(button('Cancel', '', l.close), go);
  async function submit(e) {
    if (e) e.preventDefault();
    const dir = cwd.value.trim();
    if (!dir) {
      err.textContent = 'A working directory is required.';
      err.hidden = false;
      cwd.focus();
      return;
    }
    go.disabled = true;
    go.textContent = 'Starting…';
    try {
      const r = await api.create(dir, model.value.trim(), prompt.value.trim());
      saveCwd(dir);
      l.close();
      if (r && r.session) onCreated(r.session);
    } catch (ex) {
      go.disabled = false;
      go.textContent = 'Start';
      fail(ex, 'Could not start the session.');
    }
  }
  form.addEventListener('submit', submit);
}

/** Plan drawer for live sessions. */
export async function planDrawer(id) {
  const box = el('div', 'plan');
  const l = layer('drawer', 'Plan', [box]);
  box.append(el('p', 'dim', 'Loading…'));
  try {
    const p = await api.plan(id);
    const meta = el('div', 'meta');
    meta.append(el('span', 'chip', p && p.mode ? String(p.mode) : 'plan'));
    if (p && p.path) meta.append(el('code', 'path', p.path));
    if (p && p.ready === false) meta.append(el('span', 'chip s-queued', 'not ready'));
    box.replaceChildren(meta);
    const text = p && p.content ? String(p.content) : '';
    box.append(text ? mdBox(text) : el('p', 'dim', 'No plan content yet.'));
  } catch (e) {
    box.replaceChildren(el('p', 'lerr', e instanceof ApiError && e.status === 501
      ? 'The plan is only available for live sessions.' : 'Could not load the plan: ' + e.message));
  }
  return l;
}

/** Search overlay: every hit opens that session. */
export function searchOverlay(openSession) {
  const q = textInput('q', '', 'Search transcripts');
  const out = el('div', 'hits');
  out.setAttribute('aria-live', 'polite');
  const l = layer('sheet', 'Search', [field('Query', q), out]);
  let timer = 0;
  let call = 0;
  async function run() {
    const text = q.value.trim();
    out.replaceChildren();
    if (text.length < 2) { out.append(el('p', 'dim', 'Type at least two characters.')); return; }
    out.replaceChildren(el('p', 'dim', 'Searching…'));
    const mine = ++call;
    try {
      const r = await api.search(text);
      if (mine !== call) return;
      const hits = (r && r.hits) || [];
      out.replaceChildren();
      if (!hits.length) out.append(el('p', 'dim', 'No hits.'));
      for (const h of hits) {
        const row = el('button', 'hit');
        row.type = 'button';
        row.append(el('div', 'hit-t', h.title || h.session || 'Untitled'));
        row.append(el('div', 'hit-s', h.snippet || ''));
        row.append(el('div', 'hit-m', h.session || ''));
        row.addEventListener('click', () => { l.close(); openSession(h.session); });
        out.append(row);
      }
    } catch (e) {
      if (mine === call) out.replaceChildren(el('p', 'lerr', 'Search failed: ' + e.message));
    }
  }
  q.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(run, SEARCH_MS); });
  q.addEventListener('keydown', (e) => { if (e.key === 'Enter') { clearTimeout(timer); run(); } });
  run();
}

const cell = (v, cls) => el('td', cls, v == null ? '-' : String(v));
const head = (labels) => {
  const tr = document.createElement('tr');
  tr.append(...labels.map((x) => el('th', '', x)));
  return tr;
};
const statCard = (label, value) => {
  const c = el('div', 'stat');
  c.append(el('span', 'stat-l', label), el('strong', 'stat-v', value));
  return c;
};
const wrap = (t) => {
  const d = el('div', 'table-wrap');
  d.append(t);
  return d;
};

/** Full analytics view: summary cards, per-session costs, daily table. */
export async function analyticsView() {
  const box = el('div', 'stats');
  const l = layer('full', 'Analytics', [box]);
  box.append(el('p', 'dim', 'Loading…'));
  let a;
  try {
    a = await api.analytics(30);
  } catch (e) {
    box.replaceChildren(el('p', 'lerr', 'Could not load analytics: ' + e.message));
    return l;
  }
  const daily = (a && a.daily) || [];
  const per = (a && a.per_session) || [];
  const sum = (k) => daily.reduce((n, d) => n + (Number(d[k]) || 0), 0);
  const cards = el('div', 'cards');
  cards.append(
    statCard('Sessions', String(per.length || sum('sessions'))),
    statCard('Cost 30d', money(sum('cost'))),
    statCard('Input', num(sum('input'))),
    statCard('Output', num(sum('output'))),
    statCard('Cache read', num(sum('cache_read'))),
    statCard('Active days', String(daily.filter((d) => (Number(d.sessions) || 0) > 0).length)),
  );
  box.replaceChildren(cards);

  const top = per.slice().sort((x, y) => (y.cost || 0) - (x.cost || 0)).slice(0, 10);
  const t2 = el('table', 'table');
  t2.append(el('thead', '', ''), el('tbody', '', ''));
  t2.tHead.append(head(['Session', 'Model', 'Cost', 'Updated']));
  for (const s of top) {
    const tr = document.createElement('tr');
    tr.append(cell(s.title || s.session), cell(s.model), cell(money(s.cost), 'num'), cell(ago(s.updated_at), 'dim'));
    t2.tBodies[0].append(tr);
  }
  box.append(el('h3', '', 'Top sessions by cost'), wrap(t2));

  const t1 = el('table', 'table');
  t1.append(el('thead', '', ''), el('tbody', '', ''));
  t1.tHead.append(head(['Date', 'Sessions', 'Input', 'Output', 'Cache read', 'Cache write', 'Cost']));
  for (const d of daily.slice().reverse()) {
    const tr = document.createElement('tr');
    tr.append(cell(d.date), cell(d.sessions, 'num'), cell(num(d.input), 'num'), cell(num(d.output), 'num'),
      cell(num(d.cache_read), 'num'), cell(num(d.cache_creation), 'num'), cell(money(d.cost), 'num'));
    t1.tBodies[0].append(tr);
  }
  if (!daily.length) t1.tBodies[0].append(el('tr', '', '')).append(el('td', 'dim', 'No data in this window.'));
  box.append(el('h3', '', 'Daily'), wrap(t1));
  return l;
}

/** Open (or refresh) the modal for one permission request. */
export function permissionModal(ev) {
  const key = ev.engine + ':' + ev.request_id;
  const existing = S.modals.get(key);
  if (existing) return existing;
  const title = 'Allow ' + (ev.tool || 'tool') + '?';
  const box = el('div', 'perm');
  box.append(el('div', 'perm-sub', 'Session ' + (ev.session || 'unknown') + ' · engine ' + (ev.engine || '?')));
  box.append(el('h3', '', 'Tool input'), codeText(json(ev.input == null ? null : ev.input)));
  const note = areaInput('guidance', '', 'Why are you denying this? Sent to the agent as guidance.');
  note.rows = 2;
  const actions = el('div', 'actions');
  const l = layer('sheet perm-layer', title, [box, field('Guidance (used on deny)', note), actions],
    () => S.modals.delete(key));
  const respond = async (decision) => {
    const btns = actions.querySelectorAll('button');
    for (const b of btns) b.disabled = true;
    try {
      await api.permission(ev.engine, ev.request_id, decision, note.value.trim());
      toast(decision === 'allow' ? 'Allowed ' + (ev.tool || 'tool') : 'Denied ' + (ev.tool || 'tool'), 'ok');
      l.close();
    } catch (e) {
      for (const b of btns) b.disabled = false;
      fail(e, 'Could not send the decision.');
      if (e instanceof ApiError && e.status === 404) l.close();
    }
  };
  actions.append(button('Deny', 'danger', () => respond('deny')), button('Allow', 'primary', () => respond('allow')));
  S.modals.set(key, l);
  return l;
}

/** Drop the modal that a permission_cancel refers to. */
export function permissionGone(engine, requestId, answered) {
  const key = engine + ':' + requestId;
  const m = S.modals.get(key);
  if (!m) return false;
  m.close();
  toast(answered ? 'Answered elsewhere.' : 'Request ' + requestId + ' expired. Check the terminal.', 'err');
  return true;
}
