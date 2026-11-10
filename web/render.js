/**
 * Transcript renderer. Message JSON (CONTRACTS 3) plus ACP card fields to DOM.
 *
 * Reconcile: each row is keyed by a stable message key and remembers the content
 * signature it was painted from. A re-render reuses rows whose key still matches and
 * repaints only bodies whose signature changed, so an append or a refetch touches one
 * or two nodes instead of the whole list.
 */
import { S, blocks, json, resultText, ago, num, money } from './store.js';
import { markdown } from './md.js';

const TRUNC = 2000;
const NEAR = 140;

export const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
};
const pre = (text, cls) => {
  const p = el('pre', 'out' + (cls ? ' ' + cls : ''));
  p.append(el('code', '', text == null ? '' : String(text)));
  return p;
};
/** Untrusted text shown inside a code block: never marked up. */
export const codeText = (text) => pre(text, 'json');
/** Markdown-lite text node. markdown() escapes before it adds any tag. */
export const markdownEl = (node, text) => {
  node.classList.add('md');
  node.innerHTML = markdown(text);
  return node;
};

/** Signature of everything this module paints for one message. */
function sigOf(m) {
  return m.role + '|' + (JSON.stringify(m.content) || '') + '|' + (m._stream ? 's' : '') + '|' + (JSON.stringify(m._usage) || '');
}

const hash = (s) => {
  let h = 5381;
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) | 0;
  return (h >>> 0).toString(36);
};

/** Stable key: server timestamp when present, else a content hash. */
function keyOf(m, sig) {
  if (m._key == null) m._key = m.created_at != null ? 'c' + m.created_at : 'h' + hash(sig);
  return m._key;
}

export function nearBottom(root) {
  return root.scrollHeight - root.scrollTop - root.clientHeight < NEAR;
}

/** Reconcile the container with S.messages plus the optimistic S.pending tail. */
export function renderTranscript(root) {
  const pinned = !root.children.length || nearBottom(root);
  const top = root.scrollTop;
  const list = S.pending.length ? S.messages.concat(S.pending) : S.messages;
  const pool = new Map();
  for (const n of Array.from(root.children)) if (n.dataset.k) pool.set(n.dataset.k, n);

  const cards = new Map();
  const nodes = [];
  const used = new Set();
  for (const m of list) {
    const sig = sigOf(m);
    let key = keyOf(m, sig);
    while (used.has(key)) key += '~';
    used.add(key);
    let node = pool.get(key);
    if (node) {
      pool.delete(key);
      if (node._sig !== sig) paintBody(node, m, cards);
      else for (const [id, c] of node._cards) cards.set(id, c);
    } else {
      node = el('div', 'msg');
      node.dataset.k = key;
      node._cards = new Map();
      node._who = el('div', 'who');
      node._body = el('div', 'body');
      node.append(node._who, node._body);
      paintBody(node, m, cards);
    }
    nodes.push(node);
  }

  let ref = root.firstChild;
  for (const n of nodes) {
    if (ref === n) { ref = ref.nextSibling; continue; }
    root.insertBefore(n, ref);
  }
  while (ref) { const next = ref.nextSibling; root.removeChild(ref); ref = next; }
  if (nodes.length) root.querySelector('.empty')?.remove();
  else root.append(el('div', 'empty', 'No messages yet.'));

  if (pinned) root.scrollTop = root.scrollHeight;
  else if (top) root.scrollTop = top;
}

const whoFor = (m) => (m.role === 'user'
  ? (m._pending ? 'you · sending' : 'you')
  : (m._stream ? 'assistant · streaming' : 'assistant'));

function paintBody(node, m, cards) {
  node._cards = new Map();
  node.classList.toggle('user', m.role === 'user');
  node.classList.toggle('stream', !!m._stream);
  node._who.textContent = whoFor(m);
  if (m.created_at) node._who.append(el('span', 'when', ago(m.created_at)));
  const parts = [];
  let i = 0;
  for (const b of blocks(m)) parts.push(block(b, i++, m, cards));
  if (m._usage) parts.push(usageLine(m._usage));
  node._body.replaceChildren(...parts);
  node._sig = sigOf(m);
}

/** One content block to a node. A tool_result folds into its tool_use card. */
function block(b, i, m, cards) {
  const type = (b && b.type) || 'unknown';
  if (type === 'text') return markdownEl(el('div'), b.text || '');
  if (type === 'thinking') return fold('thinking', 'Thinking', 'th:' + (m._key || 'x') + ':' + i, pre(b.thinking || b.text || ''));
  if (type === 'tool_use') return toolCard(b, cards);
  if (type === 'tool_result') {
    const card = b.tool_use_id ? cards.get(b.tool_use_id) : null;
    if (card) return attachResult(card, b);
    return fold('card result', b.is_error ? 'Error' : 'Result', 'r:' + (b.tool_use_id || i),
      preClip(resultText(b.content), b.is_error));
  }
  if (type === 'image') {
    const box = el('div', 'imgph');
    const mime = b.mimeType || b.media_type || '';
    box.append(el('span', 'imgph-i', 'IMG'), el('span', '', 'Image' + (mime ? ' · ' + mime : '')));
    return box;
  }
  return fold('card raw', 'Unknown block: ' + type, 'u:' + i, pre(json(b)));
}

/** Apply an expand choice and remember it, so it survives a repaint. */
function setOpen(node, want) {
  if (node._body) node._body.hidden = !want;
  if (node._head) node._head.setAttribute('aria-expanded', String(want));
  if (want) S.open.add(node._key); else S.open.delete(node._key);
}

/** Wire a collapsible node: click toggles, key remembers. */
function collapsible(node, head, body, key) {
  node._head = head;
  node._body = body;
  node._key = key;
  setOpen(node, S.open.has(key));
  head.addEventListener('click', () => setOpen(node, body.hidden));
  return node;
}

/** Collapsible section shared by thinking and standalone cards. */
function fold(cls, label, key, content) {
  const box = el('div', 'fold ' + cls);
  const head = el('button', 'fold-head', label);
  head.type = 'button';
  const body = el('div', 'fold-body');
  body.append(content);
  box.append(head, body);
  return collapsible(box, head, body, key);
}

function toolCard(b, cards) {
  const status = b.status === 'pending' ? 'queued' : (b.status || (b.is_error ? 'failed' : 'completed'));
  const card = el('div', 'card tool s-' + status);
  const head = el('button', 'card-head');
  head.type = 'button';
  head.append(el('span', 'chip s-' + status, status), el('span', 'card-name', b.name || 'tool'));
  const body = el('div', 'card-body');
  body.append(el('div', 'card-label', 'input'), codeText(json(b.input === undefined ? null : b.input)));
  card.append(head, body);
  collapsible(card, head, body, 'tool:' + (b.id || b.name + ':' + status));
  if (b.id) cards.set(b.id, card);
  if (b.output != null) attachResult(card, { content: b.output, is_error: status === 'failed' });
  return card;
}

/** Attach a tool_result to the card of its tool_use. */
function attachResult(card, b) {
  card.classList.add('has-res');
  if (b.is_error) card.classList.add('s-failed');
  const node = el('div', 'res' + (b.is_error ? ' err' : ''));
  node.append(el('div', 'card-label', b.is_error ? 'error' : 'result'), preClip(resultText(b.content), b.is_error));
  card._body.append(node);
  return document.createComment('result');
}

/** Pre block with a 2000-char clip and an expand control. */
function preClip(text, isErr) {
  const s = text == null ? '' : String(text);
  if (s.length <= TRUNC) return pre(s, isErr ? 'err' : '');
  const box = el('div', 'clip');
  const head = s.slice(0, TRUNC);
  const p = pre(head, isErr ? 'err' : '');
  const btn = el('button', 'more', 'show all (' + s.length + ' chars)');
  btn.type = 'button';
  let all = false;
  btn.addEventListener('click', () => {
    all = !all;
    p.firstChild.textContent = all ? s : head;
    btn.textContent = all ? 'show less' : 'show all (' + s.length + ' chars)';
  });
  box.append(p, btn);
  return box;
}

/** Footer line for the current turn: tokens and cost. */
function usageLine(u) {
  const bits = [];
  if (u.input != null) bits.push(num(u.input) + ' in');
  if (u.output != null) bits.push(num(u.output) + ' out');
  if (u.size != null) bits.push('ctx ' + num(u.size));
  if (u.cost != null) bits.push(money(u.cost));
  return el('div', 'usage', bits.join(' · '));
}
