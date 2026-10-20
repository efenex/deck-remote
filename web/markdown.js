/* deck-remote — tiny, escape-first markdown for agent replies.
 *
 * Everything is HTML-escaped before any markup is added, and only a fixed set
 * of tags is ever emitted (p, br, ul/ol/li, b, code, pre, div, span, button),
 * so agent output can't inject markup (a, table/thead/tbody/tr/th/td join the
 * set for doc links and tables). Loaded before app.js (window.DeckMarkdown)
 * and by the node tests (web/markdown.test.js).
 */
(function (root) {
  'use strict';

  const CODE_COLLAPSE_LINES = 12;

  function esc(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  // Document references: absolute (/…), home (~/…) or relative paths ending in a
  // document extension, with an optional :line. A bare name (no slash) only
  // counts for .md. Existence is not checked here; the viewer reports
  // "not readable" on tap. Matched against already-escaped text, so the
  // character class deliberately excludes & and ;.
  const DOC_EXT = '(?:md|markdown|txt|log)';
  const DOC_BODY = '(?:~\\/|\\.{0,2}\\/)?[\\w.+-]+(?:\\/[\\w.+-]+)*';
  const DOC_PATH = new RegExp('(^|[^\\w./~@+-])(' + DOC_BODY + '\\.' + DOC_EXT + ')(?::(\\d{1,6}))?(?![\\w/@+-]|\\.\\w)', 'gi');
  const DOC_WHOLE = new RegExp('^(' + DOC_BODY + '\\.' + DOC_EXT + ')(?::(\\d{1,6}))?$', 'i');

  function docOK(path) { return path.indexOf('/') >= 0 || /\.(?:md|markdown)$/i.test(path); }

  function docLink(path, line, inner) {
    return '<a class="doclink" href="#" data-path="' + esc(path) + '"' + (line ? ' data-line="' + line + '"' : '') + '>' + inner + '</a>';
  }

  function linkDocs(escaped) {
    return escaped.replace(DOC_PATH, (m, pre, path, line) => (docOK(path) ? pre + docLink(path, line, path + (line ? ':' + line : '')) : m));
  }

  // Inline forms on one line of prose: `code` first (its content is literal; a
  // span that is exactly a document path becomes a link), then **bold** and doc
  // paths in the text between code spans.
  function inline(line) {
    return line.split('`').map((part, i, all) => {
      if (i % 2 === 1 && i < all.length - 1) {
        const m = DOC_WHOLE.exec(part);
        const code = '<code>' + esc(part) + '</code>';
        return m && docOK(m[1]) ? docLink(m[1], m[2], code) : code;
      }
      const text = i % 2 === 1 ? '`' + part : part; // unmatched trailing backtick
      return linkDocs(esc(text).replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>'));
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

  // Flat list lines {indent, tag, text} -> nested <ul>/<ol>; a line indented
  // deeper than the item above opens a child list inside that <li>.
  function listHTML(items) {
    const base = items[0].indent;
    items.forEach((it) => { if (it.indent < base) it.indent = base; }); // a dedent past the first item stays at its level
    let pos = 0;
    function level(ind) {
      const tag = items[pos].tag;
      let h = '<' + tag + '>';
      while (pos < items.length) {
        const it = items[pos];
        if (it.indent < ind || (it.indent === ind && it.tag !== tag)) break;
        pos++;
        let li = '<li>' + inline(it.text);
        if (pos < items.length && items[pos].indent > it.indent) li += level(items[pos].indent);
        h += li + '</li>';
      }
      return h + '</' + tag + '>';
    }
    let out = '';
    while (pos < items.length) out += level(items[pos].indent);
    return out;
  }

  const TABLE_SEP = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;
  function cells(line) {
    return line.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map((c) => c.trim());
  }
  function tableHTML(head, sep, rows) {
    const align = cells(sep).map((c) => (/^:-*:$/.test(c) ? 'center' : /-:$/.test(c) ? 'right' : /^:/.test(c) ? 'left' : ''));
    const cell = (tag, c, i) => '<' + tag + (align[i] ? ' style="text-align:' + align[i] + '"' : '') + '>' + inline(c) + '</' + tag + '>';
    return '<div class="md-table"><table><thead><tr>' + cells(head).map((c, i) => cell('th', c, i)).join('') + '</tr></thead><tbody>' +
      rows.map((r) => '<tr>' + cells(r).map((c, i) => cell('td', c, i)).join('') + '</tr>').join('') + '</tbody></table></div>';
  }

  function prose(text, opts) {
    const out = [];
    let para = [];
    let items = [];
    const flushPara = () => { if (para.length) { out.push('<p>' + para.map(inline).join('<br>') + '</p>'); para = []; } };
    const flushList = () => { if (items.length) { out.push(listHTML(items)); items = []; } };
    const lines = text.split('\n');
    for (let n = 0; n < lines.length; n++) {
      const line = lines[n];
      const bullet = line.match(/^(\s*)[-*•]\s+(.*)$/);
      const num = line.match(/^(\s*)\d+[.)]\s+(.*)$/);
      const head = line.match(/^(#{1,6})\s+(.*)$/);
      if (!line.trim()) { flushPara(); flushList(); continue; }
      if (line.indexOf('|') >= 0 && n + 1 < lines.length && lines[n + 1].indexOf('|') >= 0 && TABLE_SEP.test(lines[n + 1])) {
        flushPara(); flushList();
        const rows = [];
        let k = n + 2;
        while (k < lines.length && lines[k].trim() && lines[k].indexOf('|') >= 0) rows.push(lines[k++]);
        out.push(tableHTML(line, lines[n + 1], rows));
        n = k - 1;
        continue;
      }
      if (bullet || num) {
        flushPara();
        const m = bullet || num;
        items.push({ indent: m[1].replace(/\t/g, '    ').length, tag: bullet ? 'ul' : 'ol', text: m[2] });
        continue;
      }
      flushList();
      if (head) {
        flushPara();
        out.push('<div class="md-h' + (opts && opts.levels ? ' md-h' + head[1].length : '') + '">' + inline(head[2]) + '</div>');
        continue;
      }
      para.push(line);
    }
    flushPara();
    flushList();
    return out.join('');
  }

  // opts.levels adds md-h1..md-h6 to headings (the document viewer styles them).
  function render(src, opts) {
    const parts = String(src == null ? '' : src).replace(/\r\n/g, '\n').split('```');
    return parts.map((part, i) => (i % 2 === 1 ? codeBlock(part) : prose(part, opts))).join('');
  }

  const api = { render, esc, DOC_PATH, DOC_WHOLE, docOK };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckMarkdown = api;
})(typeof window !== 'undefined' ? window : globalThis);
