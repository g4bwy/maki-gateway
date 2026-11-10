/**
 * State, HTTP client, and pure helpers. No DOM access in this module.
 * Wire contract: docs/CONTRACTS.md sections 2 (HTTP + SSE) and 3 (Message JSON).
 */

const qs = new URLSearchParams(location.search);

/** The one mutable app state. Owned by store, read by render/ui, written by app/acp. */
export const S = {
  mock: qs.get('mock') === '1',
  token: qs.get('token') || '',
  sessions: [],
  current: '',
  messages: [],
  pending: [],
  modals: new Map(),
  open: new Set(),
  mode: 'build',
  seq: 0,
  offline: false,
};

const LS = {
  get(k, d) { try { return localStorage.getItem('maki.' + k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem('maki.' + k, v); } catch { /* private mode */ } },
};
export const lastCwd = () => LS.get('cwd', '');
export const saveCwd = (cwd) => cwd && LS.set('cwd', cwd);
export const lastSession = () => LS.get('session', '');
export const saveSession = (id) => id && LS.set('session', id);

/** Append the auth token when present. Loopback MVP: token comes from ?token=. */
export function withToken(path) {
  if (!S.token) return path;
  return path + (path.includes('?') ? '&' : '?') + 'token=' + encodeURIComponent(S.token);
}

/** HTTP error carrying the response status so callers can branch on 404/409/501. */
export class ApiError extends Error {
  constructor(message, status) { super(message); this.name = 'ApiError'; this.status = status; }
}

/** One JSON fetch helper. Bodies are objects; responses are parsed or null. */
async function req(method, path, body) {
  const init = { method, headers: {} };
  if (body !== undefined) {
    init.headers['content-type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(withToken(path), init);
  } catch (e) {
    throw new ApiError(e.message || 'network error', 0);
  }
  const raw = await res.text();
  let data = null;
  if (raw) { try { data = JSON.parse(raw); } catch { data = { error: raw.slice(0, 200) }; } }
  if (!res.ok) throw new ApiError((data && data.error) || res.statusText || 'http ' + res.status, res.status);
  return data;
}

/** Gateway endpoints. Only non-empty optionals are sent. */
export const api = {
  health: () => req('GET', '/api/health'),
  sessions: () => req('GET', '/api/sessions'),
  messages: (id, last = 200) => req('GET', `/api/sessions/${encodeURIComponent(id)}/messages?last=${last}`),
  prompt: (id, text) => req('POST', `/api/sessions/${encodeURIComponent(id)}/prompt`, { text }),
  cancel: (id) => req('POST', `/api/sessions/${encodeURIComponent(id)}/cancel`),
  setMode: (id, mode) => req('POST', `/api/sessions/${encodeURIComponent(id)}/mode`, { mode }),
  setTitle: (id, title) => req('POST', `/api/sessions/${encodeURIComponent(id)}/title`, { title }),
  kill: (id) => req('DELETE', `/api/sessions/${encodeURIComponent(id)}`),
  plan: (id) => req('GET', '/api/plan/' + encodeURIComponent(id)),
  search: (q) => req('GET', '/api/search?q=' + encodeURIComponent(q)),
  analytics: (days = 30) => req('GET', '/api/analytics?days=' + days),
  create(cwd, model, prompt) {
    const body = { cwd };
    if (model) body.model = model;
    if (prompt) body.prompt = prompt;
    return req('POST', '/api/sessions', body);
  },
  permission(engine, requestId, decision, guidance) {
    const body = { decision };
    if (decision === 'deny' && guidance) body.guidance = guidance;
    return req('POST', `/api/permissions/${encodeURIComponent(engine)}/${encodeURIComponent(requestId)}`, body);
  },
};

/** Current session detail, or null when the id is unknown. */
export const session = (id = S.current) => S.sessions.find((s) => s.id === id) || null;
/** Engine name used by the permission endpoint for a session kind. */
export const engineOf = (s = session()) => (s && s.kind === 'live' ? 'attach' : 'acp');

/** Normalise Message.content to a block array (defensive: older shapes may vary). */
export function blocks(m) {
  const c = m && m.content;
  if (Array.isArray(c)) return c;
  if (typeof c === 'string') return [{ type: 'text', text: c }];
  if (c && typeof c === 'object') return [c];
  return [];
}

/** Plain text of a message, used to confirm optimistic bubbles after a refetch. */
export function textOf(m) {
  return blocks(m).filter((b) => b && b.type === 'text').map((b) => b.text || '').join('\n').trim();
}

/** tool_result content is a string or an array of blocks; flatten to text. */
export function resultText(content) {
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return content == null ? '' : json(content);
  return content.map((b) => (b && b.text) || (b && b.type === 'image' ? '[image]' : '')).join('\n');
}

export const json = (v) => { try { return JSON.stringify(v, null, 2) ?? String(v); } catch { return String(v); } };

/** Epoch seconds or ms to a short relative label. */
export function ago(ts) {
  if (!ts) return '';
  const ms = ts < 1e12 ? ts * 1000 : ts;
  const s = Math.max(0, (Date.now() - ms) / 1000);
  if (s < 45) return 'just now';
  if (s < 3600) return Math.round(s / 60) + 'm';
  if (s < 86400) return Math.round(s / 3600) + 'h';
  return Math.round(s / 86400) + 'd';
}

/** Trailing path segment of a cwd, for the rail rows. */
export const tail = (cwd) => (cwd ? cwd.replace(/\/+$/, '').split('/').pop() || cwd : '');

/** Token/cost formatter for footers and analytics. */
export function num(v) {
  if (v == null) return '-';
  if (Math.abs(v) >= 1e6) return (v / 1e6).toFixed(1) + 'M';
  if (Math.abs(v) >= 1e3) return (v / 1e3).toFixed(1) + 'k';
  return String(Math.round(v * 100) / 100);
}
export const money = (v) => (v == null ? '-' : '$' + (Math.abs(v) < 1 ? v.toFixed(4) : v.toFixed(2)));
