/**
 * Markdown-lite: bold, italic, inline code, fenced code, links. Nothing else.
 * Every fragment is HTML-escaped before any tag is added, so output is safe for innerHTML.
 * Substitution uses NUL-delimited placeholders that cannot appear in the source
 * (control characters are stripped from the input first).
 */

const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

/** Escape HTML significant characters. */
export function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ESCAPES[c]);
}

/** Render a markdown-lite string to an HTML string. */
export function markdown(src) {
  const out = [];
  const keep = (html) => '\u0000' + (out.push(html) - 1) + '\u0000';
  let text = String(src == null ? '' : src).replace(/[\u0000\u0001]/g, '');

  text = text.replace(/```([\w+.-]{0,32})[ \t]*\r?\n?([\s\S]*?)(?:```|$)/g,
    (_m, lang, code) => keep(`<pre class="code"${lang ? ` data-lang="${lang}"` : ''}><code>${escapeHtml(code.replace(/\n$/, ''))}</code></pre>`));
  text = escapeHtml(text);
  text = text.replace(/`([^`\n]+)`/g, (_m, code) => keep(`<code class="ic">${code}</code>`));
  text = text.replace(/\[([^\]\n]*)\]\(([^)\s]+)\)/g, (m, label, href) => {
    const url = /^https?:\/\//i.test(href) ? href : /^mailto:/i.test(href) ? href : '';
    return url ? keep(`<a href="${url}" target="_blank" rel="noopener noreferrer">${label || url}</a>`) : m;
  });
  text = text.replace(/\*\*([^*\n]+)\*\*/g, (_m, b) => keep(`<strong>${b}</strong>`));
  text = text.replace(/(^|[\s(])([*_])(?!\s)([^*_\n]+?)(?<!\s)\2/g, (_m, lead, mark, i) => lead + keep(`<em>${i}</em>`));
  text = text.replace(/\n/g, '<br>');
  return text.replace(/\u0000(\d+)\u0000/g, (_m, i) => out[+i] ?? '');
}
