/**
 * Markdown-lite: fenced code, headings, lists, quotes, rules, GFM tables, then
 * bold, italic, inline code, and links. Nothing else.
 * Every fragment is HTML-escaped before any tag is added, so output is safe for innerHTML.
 * Substitution uses NUL-delimited placeholders that cannot appear in the source
 * (control characters are stripped from the input first).
 */

const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
const INDENT_STEP = 2;
const FENCE = /```([\w+.-]{0,32})[ \t]*\r?\n?([\s\S]*?)(?:```|$)/g;
const CODE_SPAN = /`([^`\n]+)`/g;
const LINK = /\[([^\]\n]*)\]\(([^)\s]+)\)/g;
const BOLD = /\*\*([^*\n]+)\*\*/g;
const ITALIC = /(^|[\s(])([*_])(?!\s)([^*_\n]+?)(?<!\s)\2/g;
const CODE_LINE = /^\u0000\d+\u0000$/;
const MARKED = /\u0000(\d+)\u0000/g;
const HEADING = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$/;
const RULE = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const QUOTE = /^ {0,3}>[ \t]?(.*)$/;
const BULLET = /^( *)([-*+])[ \t]+(.*)$/;
const NUMBERED = /^( *)(\d+)\.[ \t]+(.*)$/;
const DELIM_CELL = /^:?-+:?$/;

/** Escape HTML significant characters. */
export function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ESCAPES[c]);
}

/** Render a markdown-lite string to an HTML string. */
export function markdown(src) {
  const kept = [];
  const stash = (html) => '\u0000' + (kept.push(html) - 1) + '\u0000';
  let text = String(src == null ? '' : src).replace(/[\u0000\u0001]/g, '').replace(/\r\n?/g, '\n');
  text = text.replace(FENCE, (_m, lang, code) => stash(
    `<pre class="code"${lang ? ` data-lang="${lang}"` : ''}><code>${escapeHtml(code.replace(/\n$/, ''))}</code></pre>`));

  let html = blocks(text.split('\n'), stash);
  // A stashed fragment can hold another one (bold around a code span), so resolve until none is left.
  for (let n = 0; n < 8 && html.includes('\u0000'); n++) html = html.replace(MARKED, (_m, i) => kept[+i] ?? '');
  return html;
}

/** Walk raw lines and render each block. A line holding a lone fenced-code mark is a block. */
function blocks(lines, stash) {
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) i++;
    else if (CODE_LINE.test(line)) { out.push(line); i++; }
    else if (HEADING.test(line)) { out.push(heading(line, stash)); i++; }
    else if (RULE.test(line)) { out.push('<hr>'); i++; }
    else if (QUOTE.test(line)) { out.push(quote(lines, i, stash)); i = endQuote(lines, i); }
    else if (isTable(lines, i)) { out.push(table(lines, i, stash)); i = endTable(lines, i); }
    else if (isItem(line)) { out.push(list(lines, i, stash)); i = endItems(lines, i); }
    else { out.push(para(lines, i, stash)); i = endPara(lines, i); }
  }
  return out.join('');
}

/** Any line that opens a block ends the paragraph before it, so a table can interrupt one. */
function startsBlock(lines, i) {
  const line = lines[i];
  return CODE_LINE.test(line) || HEADING.test(line) || RULE.test(line) || QUOTE.test(line) || isItem(line) || isTable(lines, i);
}

function endPara(lines, i) {
  let j = i + 1;
  while (j < lines.length && lines[j].trim() && !startsBlock(lines, j)) j++;
  return j;
}

function para(lines, i, stash) {
  return `<p>${lines.slice(i, endPara(lines, i)).map((l) => inline(l, stash)).join('<br>')}</p>`;
}

function heading(line, stash) {
  const m = HEADING.exec(line);
  const tag = 'h' + m[1].length;
  return `<${tag}>${inline(m[2] || '', stash)}</${tag}>`;
}

function endQuote(lines, i) {
  let j = i;
  while (QUOTE.test(lines[j] || '')) j++;
  return j;
}

function quote(lines, i, stash) {
  const inner = lines.slice(i, endQuote(lines, i)).map((l) => QUOTE.exec(l)[1]);
  return `<blockquote>${blocks(inner, stash)}</blockquote>`;
}

const isItem = (line) => !!line && (BULLET.test(line) || NUMBERED.test(line));

/** A run of items ends where the text runs out or the list kind changes (GFM starts a new list). */
function endItems(lines, i) {
  const ordered = NUMBERED.test(lines[i]);
  let j = i;
  while (isItem(lines[j]) && NUMBERED.test(lines[j]) === ordered) j++;
  return j;
}

/** Nested <ul>/<ol>. Depth counts two-space indents, the same step the TUI uses. */
function list(lines, i, stash) {
  const items = [];
  for (let j = i, end = endItems(lines, i); j < end; j++) {
    const b = BULLET.exec(lines[j]);
    const m = b || NUMBERED.exec(lines[j]);
    items.push({ depth: Math.floor(m[1].length / INDENT_STEP), text: m[3], ordered: !b, num: b ? 0 : +m[2] });
  }
  const root = { kids: [] };
  const open = [{ depth: -1, node: root }];
  for (const it of items) {
    while (open.length > 1 && open[open.length - 1].depth >= it.depth) open.pop();
    const node = { ...it, kids: [] };
    open[open.length - 1].node.kids.push(node);
    open.push({ depth: it.depth, node });
  }
  return listTag(root.kids, stash);
}

function listTag(kids, stash) {
  if (!kids.length) return '';
  const tag = kids[0].ordered ? 'ol' : 'ul';
  const start = kids[0].ordered && kids[0].num !== 1 ? ` start="${kids[0].num}"` : '';
  const body = kids.map((k) => `<li>${inline(k.text, stash)}${listTag(k.kids, stash)}</li>`).join('');
  return `<${tag}${start}>${body}</${tag}>`;
}

/* ------------------------------------------------------------------ tables */

/** Cells of a row, or null unless the line holds a pipe that is neither escaped nor inside code. */
function rowOf(line) {
  if (!line || !line.includes('|')) return null;
  const s = line.trim();
  const cells = [];
  let cur = '';
  let split = false;
  let i = 0;
  while (i < s.length) {
    const c = s[i];
    if (c === '`') {
      let run = 1;
      while (s[i + run] === '`') run++;
      const close = s.indexOf('`'.repeat(run), i + run);
      const span = s.slice(i, close < 0 ? undefined : close + run);
      cur += span;
      i += span.length;
    } else if (c === '\\' && s[i + 1] === '|') { cur += '|'; i += 2; }
    else if (c === '|') { split = true; cells.push(cur.trim()); cur = ''; i++; }
    else { cur += c; i++; }
  }
  cells.push(cur.trim());
  if (!split) return null;
  if (cells.length > 1 && !cells[0]) cells.shift();
  if (cells.length > 1 && !cells[cells.length - 1]) cells.pop();
  return cells.some((c) => c) ? cells : null;
}

/** One alignment per column of a delimiter row, or null when the line is not one. */
function delimOf(line) {
  const cells = rowOf(line);
  if (!cells || !cells.every((c) => DELIM_CELL.test(c))) return null;
  return cells.map(alignOf);
}

const alignOf = (c) => (c.startsWith(':') ? (c.endsWith(':') ? 'center' : 'left') : c.endsWith(':') ? 'right' : '');

/** True when lines[i] opens a table: a row over a delimiter row with at least as many cells. */
const isTable = (lines, i) => {
  const head = rowOf(lines[i]);
  const cols = head && delimOf(lines[i + 1]);
  return !!head && !!cols && cols.length >= head.length;
};

function endTable(lines, i) {
  let j = i + 2;
  while (rowOf(lines[j])) j++;
  return j;
}

/** Semantic table. Short rows pad, long rows truncate to the header column count. */
function table(lines, i, stash) {
  const head = rowOf(lines[i]);
  const aligns = delimOf(lines[i + 1]);
  const cell = (text, a, tag) => `<${tag}${a ? ` style="text-align:${a}"` : ''}>${inline(text, stash)}</${tag}>`;
  const tr = (cells, tag) => `<tr>${head.map((_h, k) => cell(cells[k] || '', aligns[k], tag)).join('')}</tr>`;
  const body = [];
  for (let j = i + 2, end = endTable(lines, i); j < end; j++) body.push(tr(rowOf(lines[j]), 'td'));
  return `<div class="table-wrap"><table class="table"><thead>${tr(head, 'th')}</thead><tbody>${body.join('')}</tbody></table></div>`;
}

/* ------------------------------------------------------------------ inline */

/** Escape first, then add inline tags, so source text never reaches the output unescaped. */
function inline(src, stash) {
  let text = escapeHtml(src);
  text = text.replace(CODE_SPAN, (_m, code) => stash(`<code class="ic">${code}</code>`));
  text = text.replace(LINK, (m, label, href) => {
    const url = /^https?:\/\//i.test(href) ? href : /^mailto:/i.test(href) ? href : '';
    return url ? stash(`<a href="${url}" target="_blank" rel="noopener noreferrer">${label || url}</a>`) : m;
  });
  text = text.replace(BOLD, (_m, b) => stash(`<strong>${b}</strong>`));
  text = text.replace(ITALIC, (_m, lead, mark, i) => lead + stash(`<em>${i}</em>`));
  return text;
}
