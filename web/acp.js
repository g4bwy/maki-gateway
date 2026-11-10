/**
 * Reducer for ACP passthrough updates (CONTRACTS 2 SSE `update` with engine
 * "acp"; names from section 3). It writes straight into the Message/block shapes
 * the renderer already knows, so a later refetch can replace S.messages with no
 * translation layer. Local-only fields are prefixed with `_`.
 */
import { S } from './store.js';

/** Concatenate the text carried by an ACP content chunk. */
function chunkText(c) {
  if (c == null) return '';
  if (typeof c === 'string') return c;
  if (Array.isArray(c)) return c.map(chunkText).join('');
  if (c.text != null) return String(c.text);
  if (c.content != null) return chunkText(c.content);
  return '';
}

/** Assistant message accepting stream events, created when the tail is not one. */
function streamMsg() {
  const last = S.messages[S.messages.length - 1];
  if (last && last.role === 'assistant' && last._stream) return last;
  const m = { role: 'assistant', content: [], created_at: Date.now(), _stream: true };
  S.messages.push(m);
  return m;
}

/** Final block of a type, or null. Consecutive chunks of one type share a block. */
function tailBlock(m, type) {
  const b = m.content[m.content.length - 1];
  return b && b.type === type ? b : null;
}

const addBlock = (m, b) => (m.content.push(b), b);
const nameOf = (ev) => ev.name || ev.title || (ev.tool && (ev.tool.name || ev.tool.title)) || 'tool';

/** Add or update the tool_use card that a tool_call / tool_call_update describes. */
function upsertTool(ev) {
  const m = streamMsg();
  const id = ev.toolCallId;
  let b = id ? m.content.find((x) => x.type === 'tool_use' && x.id === id) : null;
  if (!b) {
    b = { type: 'tool_use', id: id || 'acp' + ++S.seq, name: nameOf(ev), input: null, status: 'queued' };
    m.content.push(b);
  }
  if (ev.name || ev.title) b.name = nameOf(ev);
  if (ev.rawInput !== undefined) b.input = ev.rawInput;
  if (ev.status) b.status = ev.status === 'pending' ? 'queued' : ev.status;
  const out = ev.rawOutput !== undefined ? ev.rawOutput
    : ev.output !== undefined ? ev.output
      : ev.status === 'failed' ? (ev.error ?? ev.message) : undefined;
  if (out !== undefined) b.output = out;
}

const firstNum = (o, keys) => {
  for (const k of keys) {
    const v = o && o[k];
    if (typeof v === 'number') return v;
  }
  return null;
};

/** Current-turn tokens and cost, shown as a footer on the streaming message. */
function applyUsage(ev) {
  const u = ev.usage && typeof ev.usage === 'object' ? ev.usage : {};
  const pick = (keys) => firstNum(u, keys) ?? firstNum(ev, keys);
  streamMsg()._usage = {
    input: pick(['input', 'input_tokens', 'inputTokens']),
    output: pick(['output', 'output_tokens', 'outputTokens']),
    cost: pick(['cost', 'total_cost', 'costUsd']),
    size: pick(['size', 'used', 'context_size', 'total_tokens', 'totalTokens']),
  };
}

/**
 * Fold one raw ACP session/update into the transcript.
 * @param {object} ev raw session/update params
 * @returns {string} what changed: text|thinking|tool|usage|mode|ignored
 */
export function applyUpdate(ev) {
  if (!ev || typeof ev !== 'object') return 'ignored';
  switch (ev.sessionUpdate || ev.type) {
    case 'agent_message_chunk': {
      const m = streamMsg();
      const b = tailBlock(m, 'text') || addBlock(m, { type: 'text', text: '' });
      b.text += chunkText(ev.content);
      return 'text';
    }
    case 'agent_thought_chunk': {
      const m = streamMsg();
      const b = tailBlock(m, 'thinking') || addBlock(m, { type: 'thinking', thinking: '' });
      b.thinking += chunkText(ev.content);
      return 'thinking';
    }
    case 'tool_call':
    case 'tool_call_update':
      upsertTool(ev);
      return 'tool';
    case 'usage_update':
      applyUsage(ev);
      return 'usage';
    case 'current_mode_update': {
      const mode = ev.currentModeId || ev.modeId || (ev.mode && (ev.mode.id || ev.mode));
      if (mode === 'build' || mode === 'plan') S.mode = mode;
      return 'mode';
    }
    default:
      return 'ignored';
  }
}

/** Stop attributing stream events to the tail message (turn over, or refetch). */
export function closeStream() {
  for (const m of S.messages) if (m._stream) m._stream = false;
}
