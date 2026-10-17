// A small Markdown renderer for the agent's replies: headings, paragraphs,
// lists (nested, numbered, task lists), block quotes, fenced code, tables,
// rules, and inline code, emphasis, strikethrough and links. Everything is
// escaped first, so a reply can't put markup of its own on the page.
'use strict';

const Markdown = (() => {
  const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ESC[c]);

  const FENCE = /^\s{0,3}(`{3,}|~{3,})\s*([^`\s]*)/;
  const HEADING = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
  const RULE = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/;
  const QUOTE = /^\s{0,3}>/;
  const ITEM = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
  const TABLE_RULE = /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/;

  function inline(src) {
    const held = [];
    const hold = (html) => '\u0000' + (held.push(html) - 1) + '\u0000';
    let s = src.replace(/(`+)([\s\S]*?[^`])\1(?!`)/g, (m, ticks, code) => hold('<code>' + esc(code.replace(/^ ([\s\S]*) $/, '$1')) + '</code>'));
    s = s.replace(/\\([\\`*_{}\[\]()#+\-.!|~>])/g, (m, c) => hold(esc(c)));
    s = esc(s);
    s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+|mailto:[^\s)]+)\)/g, (m, text, url) => hold(`<a href="${url}" target="_blank" rel="noopener noreferrer">${text}</a>`));
    s = s.replace(/&lt;(https?:\/\/[^\s&]+)&gt;/g, (m, url) => hold(`<a href="${url}" target="_blank" rel="noopener noreferrer">${url}</a>`));
    s = s.replace(/(^|[\s(])(https?:\/\/[^\s<]*[^\s<.,;:!?)'"\]])/g, (m, pre, url) => pre + hold(`<a href="${url}" target="_blank" rel="noopener noreferrer">${url}</a>`));
    s = s.replace(/\*\*(?=\S)([\s\S]*?\S)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/(^|[^\w])__(?=\S)([\s\S]*?\S)__(?!\w)/g, '$1<strong>$2</strong>');
    s = s.replace(/(^|[^*\w])\*(?=[^\s*])([^*]*?[^\s*])\*(?![*\w])/g, '$1<em>$2</em>');
    s = s.replace(/(^|[^\w])_(?=[^\s_])([^_]*?[^\s_])_(?!\w)/g, '$1<em>$2</em>');
    s = s.replace(/~~(?=\S)([\s\S]*?\S)~~/g, '<del>$1</del>');
    return s.replace(/\u0000(\d+)\u0000/g, (m, n) => held[n]);
  }

  function code(text, lang) {
    return '<div class="code"><div class="code-head"><span>' + esc(lang || '') + '</span>' +
      '<button type="button" class="copy" data-copy>Copy</button></div>' +
      '<pre><code>' + esc(text) + '</code></pre></div>';
  }

  const indentOf = (line) => line.match(/^\s*/)[0].replace(/\t/g, '    ').length;

  function startsBlock(line) {
    return FENCE.test(line) || HEADING.test(line) || RULE.test(line) || QUOTE.test(line) || ITEM.test(line);
  }

  function cells(line) {
    return line.trim().replace(/^\|/, '').replace(/\|$/, '').replace(/\\\|/g, '\u0001')
      .split('|').map((c) => c.trim().replace(/\u0001/g, '|'));
  }

  function table(lines, i) {
    const head = cells(lines[i]);
    const align = cells(lines[i + 1]).map((c) => c.startsWith(':') && c.endsWith(':') ? 'center' : c.endsWith(':') ? 'right' : '');
    const attr = (k) => align[k] ? ` style="text-align:${align[k]}"` : '';
    let html = '<div class="table"><table><thead><tr>' + head.map((c, k) => `<th${attr(k)}>${inline(c)}</th>`).join('') + '</tr></thead><tbody>';
    i += 2;
    while (i < lines.length && lines[i].includes('|') && lines[i].trim()) {
      const row = cells(lines[i]);
      html += '<tr>' + head.map((_, k) => `<td${attr(k)}>${inline(row[k] || '')}</td>`).join('') + '</tr>';
      i++;
    }
    return [html + '</tbody></table></div>', i];
  }

  function list(lines, i) {
    const first = lines[i].match(ITEM);
    const base = indentOf(first[1]);
    const ordered = /\d/.test(first[2]);
    const items = [];
    let item = null;
    let offset = 0;
    let loose = false;
    for (; i < lines.length; i++) {
      const line = lines[i];
      const m = line.match(ITEM);
      if (m && indentOf(m[1]) <= base + 1 && /\d/.test(m[2]) === ordered) {
        item = [m[3]];
        items.push(item);
        offset = indentOf(m[1]) + m[2].length + 1;
        continue;
      }
      if (!line.trim()) {
        // A blank line ends the list unless more of it follows.
        const next = lines[i + 1];
        if (next === undefined || !(indentOf(next) > base || (ITEM.test(next) && indentOf(next.match(ITEM)[1]) <= base + 1))) break;
        if (indentOf(next) <= base) loose = true;
        item.push('');
        continue;
      }
      if (indentOf(line) > base) {
        item.push(line.replace(new RegExp('^ {0,' + offset + '}'), ''));
        continue;
      }
      if (startsBlock(line)) break;
      item.push(line); // a lazy continuation of the item's paragraph
    }
    const start = ordered ? parseInt(first[2], 10) : 1;
    let html = ordered ? `<ol${start !== 1 ? ` start="${start}"` : ''}>` : '<ul>';
    for (const lines of items) {
      let body = lines.join('\n');
      let box = '';
      const task = body.match(/^\[([ xX])\]\s+/);
      if (task) {
        box = `<span class="task${task[1] === ' ' ? '' : ' done'}"></span>`;
        body = body.slice(task[0].length);
      }
      let inner = render(body);
      if (!loose) inner = inner.replace(/^<p>([\s\S]*?)<\/p>/, '$1');
      html += `<li${task ? ' class="task-item"' : ''}>${box}${inner}</li>`;
    }
    return [html + (ordered ? '</ol>' : '</ul>'), i];
  }

  function render(src) {
    const lines = String(src || '').replace(/\r\n?/g, '\n').split('\n');
    let html = '';
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];
      let m;
      if ((m = line.match(FENCE))) {
        const fence = m[1];
        const indent = indentOf(line);
        const body = [];
        for (i++; i < lines.length && !lines[i].trim().startsWith(fence); i++) {
          body.push(lines[i].replace(new RegExp('^ {0,' + indent + '}'), ''));
        }
        i++;
        html += code(body.join('\n'), m[2]);
        continue;
      }
      if (!line.trim()) { i++; continue; }
      if ((m = line.match(HEADING))) {
        const n = m[1].length;
        html += `<h${n}>${inline(m[2])}</h${n}>`;
        i++;
        continue;
      }
      if (RULE.test(line)) { html += '<hr>'; i++; continue; }
      if (QUOTE.test(line)) {
        const body = [];
        for (; i < lines.length && QUOTE.test(lines[i]); i++) body.push(lines[i].replace(/^\s{0,3}> ?/, ''));
        html += '<blockquote>' + render(body.join('\n')) + '</blockquote>';
        continue;
      }
      if (line.includes('|') && i + 1 < lines.length && TABLE_RULE.test(lines[i + 1]) && lines[i + 1].includes('-')) {
        [m, i] = table(lines, i);
        html += m;
        continue;
      }
      if (ITEM.test(line)) {
        [m, i] = list(lines, i);
        html += m;
        continue;
      }
      const para = [line.trim()];
      for (i++; i < lines.length && lines[i].trim() && !startsBlock(lines[i]); i++) {
        if (lines[i].includes('|') && i + 1 < lines.length && TABLE_RULE.test(lines[i + 1])) break;
        para.push(lines[i].trim());
      }
      html += '<p>' + inline(para.join('\n')).replace(/ {2,}\n|\\\n/g, '<br>') + '</p>';
    }
    return html;
  }

  return { render, inline, esc };
})();
