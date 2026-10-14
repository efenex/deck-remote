/* deck-remote — tiny, escape-first markdown for agent replies.
 *
 * Everything is HTML-escaped before any markup is added, and only a fixed set
 * of tags is ever emitted (p, br, ul/ol/li, b, code, pre, div, span, button),
 * so agent output can't inject markup. Loaded before app.js (window.DeckMarkdown)
 * and by the node tests (web/markdown.test.js).
 */
(function (root) {
  'use strict';

  const CODE_COLLAPSE_LINES = 12;

  function esc(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  // Inline forms on one line of prose: `code` first (its content is literal),
  // then **bold** in the text between code spans.
  function inline(line) {
    return line.split('`').map((part, i, all) => {
      if (i % 2 === 1 && i < all.length - 1) return '<code>' + esc(part) + '</code>';
      const text = i % 2 === 1 ? '`' + part : part; // unmatched trailing backtick
      return esc(text).replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>');
    }).join('');
  }

  function codeBlock(raw) {
    let lang = '';
    let code = raw;
    const nl = raw.indexOf('\n');
    const first = nl < 0 ? raw : raw.slice(0, nl);
    if (/^[\w+#.-]*$/.test(first.trim())) {
      lang = first.trim();
      code = nl < 0 ? '' : raw.slice(nl + 1);
    }
    code = code.replace(/\n+$/, '');
    const lines = code ? code.split('\n').length : 0;
    const long = lines > CODE_COLLAPSE_LINES;
    return '<div class="code wrap' + (long ? ' collapsed' : '') + '">' +
      '<div class="code-bar"><span class="code-lang">' + esc(lang || 'code') + '</span>' +
        '<button type="button" class="code-btn" data-act="wrap">Unwrap</button>' +
        '<button type="button" class="code-btn" data-act="copy">Copy</button></div>' +
      '<pre><code>' + esc(code) + '</code></pre>' +
      (long ? '<button type="button" class="code-more" data-act="more">Show all ' + lines + ' lines</button>' : '') +
      '</div>';
  }

  function prose(text) {
    const out = [];
    let para = [];
    let list = null; // { tag, items }
    const flushPara = () => { if (para.length) { out.push('<p>' + para.map(inline).join('<br>') + '</p>'); para = []; } };
    const flushList = () => {
      if (list) { out.push('<' + list.tag + '>' + list.items.map((i) => '<li>' + inline(i) + '</li>').join('') + '</' + list.tag + '>'); list = null; }
    };
    text.split('\n').forEach((line) => {
      const bullet = line.match(/^\s*[-*•]\s+(.*)$/);
      const num = line.match(/^\s*\d+[.)]\s+(.*)$/);
      const head = line.match(/^#{1,6}\s+(.*)$/);
      if (!line.trim()) { flushPara(); flushList(); return; }
      if (bullet || num) {
        flushPara();
        const tag = bullet ? 'ul' : 'ol';
        if (list && list.tag !== tag) flushList();
        if (!list) list = { tag, items: [] };
        list.items.push((bullet || num)[1]);
        return;
      }
      flushList();
      if (head) { flushPara(); out.push('<div class="md-h">' + inline(head[1]) + '</div>'); return; }
      para.push(line);
    });
    flushPara();
    flushList();
    return out.join('');
  }

  function render(src) {
    const parts = String(src == null ? '' : src).replace(/\r\n/g, '\n').split('```');
    return parts.map((part, i) => (i % 2 === 1 ? codeBlock(part) : prose(part))).join('');
  }

  const api = { render, esc };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckMarkdown = api;
})(typeof window !== 'undefined' ? window : globalThis);
