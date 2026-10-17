// Markdown for model output. Results come from LLMs and are untrusted, so this
// never produces HTML strings: it parses into a small tree and builds DOM
// nodes, with all text set as text. Raw HTML in the source stays literal text,
// links are limited to http(s) and mailto, and images become links (nothing
// is fetched). Single newlines are kept as line breaks because merged
// results ("list", "concat") are often one item per line.

// ---------- block parsing ----------

const FENCE = /^ {0,3}(`{3,}|~{3,})\s*([\w+#.-]*)/;
const HEADING = /^ {0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
const HR = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
const QUOTE = /^ {0,3}> ?(.*)$/;
const ITEM = /^( *)([-*+]|\d{1,9}[.)])\s+(.*)$/;
const TABLE_SEP = /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/;

export function parseMarkdown(src) {
  const lines = String(src ?? "").replace(/\r\n?/g, "\n").replace(/\t/g, "    ").split("\n");
  return parseBlocks(lines);
}

function parseBlocks(lines) {
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }

    let m = line.match(FENCE);
    if (m) {
      const fence = m[1], body = [];
      i++;
      while (i < lines.length && !lines[i].trimStart().startsWith(fence)) body.push(lines[i++]);
      i++; // closing fence (or end of input)
      out.push({ type: "code", lang: m[2] || "", text: body.join("\n") });
      continue;
    }
    if ((m = line.match(HEADING))) {
      out.push({ type: "heading", level: m[1].length, children: parseInline(m[2]) });
      i++;
      continue;
    }
    if (HR.test(line)) { out.push({ type: "hr" }); i++; continue; }
    if (line.includes("|") && i + 1 < lines.length && TABLE_SEP.test(lines[i + 1]) && lines[i + 1].includes("-")) {
      const head = splitRow(line);
      const align = splitRow(lines[i + 1]).map((c) => (c.startsWith(":") && c.endsWith(":") ? "center" : c.endsWith(":") ? "right" : c.startsWith(":") ? "left" : ""));
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim()) rows.push(splitRow(lines[i++]));
      out.push({
        type: "table", align,
        head: head.map((c) => parseInline(c)),
        rows: rows.map((r) => head.map((_, j) => parseInline(r[j] ?? ""))),
      });
      continue;
    }
    if (QUOTE.test(line)) {
      const body = [];
      while (i < lines.length && lines[i].trim() && (m = lines[i].match(QUOTE))) { body.push(m[1]); i++; }
      out.push({ type: "quote", children: parseBlocks(body) });
      continue;
    }
    if ((m = line.match(ITEM))) {
      const { node, next } = parseList(lines, i, m[1].length);
      out.push(node);
      i = next;
      continue;
    }
    // Paragraph: consecutive lines until a blank line or another block starts.
    const para = [];
    while (i < lines.length && lines[i].trim() && !startsBlock(lines, i)) para.push(lines[i++].trim());
    if (!para.length) para.push(lines[i++].trim()); // a lone line that matched no block rule
    out.push({ type: "paragraph", children: parseInline(para.join("\n")) });
  }
  return out;
}

function startsBlock(lines, i) {
  const l = lines[i];
  return FENCE.test(l) || HEADING.test(l) || HR.test(l) || QUOTE.test(l) || ITEM.test(l) ||
    (l.includes("|") && i + 1 < lines.length && TABLE_SEP.test(lines[i + 1]) && lines[i + 1].includes("-"));
}

// parseList reads list items at one indent. An item's content is its first
// line plus following lines indented past the marker (or blank lines followed
// by such lines); that content is parsed as blocks, so nested lists work.
function parseList(lines, start, indent) {
  const first = lines[start].match(ITEM);
  const ordered = /\d/.test(first[2]);
  const node = { type: "list", ordered, start: ordered ? parseInt(first[2], 10) : 1, items: [] };
  let i = start, loose = false;
  while (i < lines.length) {
    const m = lines[i].match(ITEM);
    if (!m || m[1].length !== indent || /\d/.test(m[2]) !== ordered) break;
    const contentIndent = m[1].length + m[2].length + 1;
    const body = [m[3]];
    i++;
    while (i < lines.length) {
      const l = lines[i];
      if (!l.trim()) {
        const nextIndented = i + 1 < lines.length && lines[i + 1].trim() && indentOf(lines[i + 1]) > indent;
        if (!nextIndented) break;
        loose = true;
        body.push("");
        i++;
        continue;
      }
      if (indentOf(l) <= indent && (ITEM.test(l) || startsBlock(lines, i))) break;
      if (indentOf(l) <= indent && body[body.length - 1] === "") break;
      body.push(l.slice(Math.min(indentOf(l), contentIndent)));
      i++;
    }
    node.items.push(parseBlocks(body));
    // A blank line between items makes the list loose.
    if (i < lines.length && !lines[i].trim() && i + 1 < lines.length) {
      const n = lines[i + 1].match(ITEM);
      if (n && n[1].length === indent && /\d/.test(n[2]) === ordered) { loose = true; i++; }
    }
  }
  node.loose = loose;
  return { node, next: i };
}

function indentOf(l) { return l.length - l.trimStart().length; }

function splitRow(line) {
  let s = line.trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
  const cells = [];
  let cur = "";
  for (let k = 0; k < s.length; k++) {
    if (s[k] === "\\" && s[k + 1] === "|") { cur += "|"; k++; continue; }
    if (s[k] === "|") { cells.push(cur.trim()); cur = ""; continue; }
    cur += s[k];
  }
  cells.push(cur.trim());
  return cells;
}

// ---------- inline parsing ----------

// Earliest match wins; on a tie the order below decides.
const INLINE = [
  ["code", /(`+)([\s\S]*?[^`])\1(?!`)/],
  // Link targets may hold one level of parentheses (e.g. Wikipedia URLs).
  ["image", /!\[([^\]]*)\]\(\s*((?:[^()\s]|\([^()\s]*\))+)(?:\s+"[^"]*")?\s*\)/],
  ["link", /\[([^\]]+)\]\(\s*((?:[^()\s]|\([^()\s]*\))+)(?:\s+"[^"]*")?\s*\)/],
  ["autolink", /<((?:https?:\/\/|mailto:)[^>\s]+)>/],
  ["url", /\bhttps?:\/\/[^\s<>()]*[^\s<>().,;:!?'"*_~]/],
  ["strong", /\*\*(?=\S)([\s\S]*?\S)\*\*|__(?=\S)([\s\S]*?\S)__(?!\w)/],
  ["del", /~~(?=\S)([\s\S]*?\S)~~/],
  ["em", /\*(?=[^\s*])([^*]*?[^\s*])\*|(?<![\w])_(?=[^\s_])([^_]*?[^\s_])_(?![\w])/],
  ["br", /\n/],
];

// Backslash-escaped punctuation is swapped for private-use placeholders
// before matching, so \* never starts emphasis, then restored: as the plain
// character in text, and with its backslash inside code spans.
const ESCAPABLE = "\\`*_{}[]()#+-.!|~>";
const PH = 0xe000;
const encodeEscapes = (s) => s.replace(/\\([\\`*_{}[\]()#+\-.!|~>])/g, (_, c) => String.fromCharCode(PH + ESCAPABLE.indexOf(c)));
const restore = (s, keepSlash) => s.replace(/[\ue000-\ue013]/g, (c) => (keepSlash ? "\\" : "") + ESCAPABLE[c.charCodeAt(0) - PH]);

export function parseInline(text) {
  return inline(encodeEscapes(String(text)));
}

function inline(text) {
  const out = [];
  let s = text;
  while (s) {
    let best = null;
    for (const [type, re] of INLINE) {
      const m = re.exec(s);
      if (m && (!best || m.index < best.m.index)) best = { type, m };
    }
    if (!best) { pushText(out, s); break; }
    const { type, m } = best;
    if (m.index > 0) pushText(out, s.slice(0, m.index));
    switch (type) {
      case "code": out.push({ type: "code", text: restore(m[2].replace(/^ (.*) $/, "$1"), true) }); break;
      case "image": out.push(linkNode(restore(m[2]), [{ type: "text", text: restore(m[1] || m[2]) }])); break;
      case "link": out.push(linkNode(restore(m[2]), inline(m[1]))); break;
      case "autolink":
      case "url": { const url = restore(m[1] ?? m[0]); out.push(linkNode(url, [{ type: "text", text: url.replace(/^mailto:/, "") }])); break; }
      case "strong": out.push({ type: "strong", children: inline(m[1] ?? m[2]) }); break;
      case "del": out.push({ type: "del", children: inline(m[1]) }); break;
      case "em": out.push({ type: "em", children: inline(m[1] ?? m[2]) }); break;
      case "br": out.push({ type: "br" }); break;
    }
    s = s.slice(m.index + m[0].length);
  }
  return out;
}

function pushText(out, text) {
  text = restore(text, false);
  const last = out[out.length - 1];
  if (last?.type === "text") last.text += text;
  else if (text) out.push({ type: "text", text });
}

// safeHref allows only web and mail links; anything else (javascript:,
// data:, relative paths) is shown as plain text.
export function safeHref(url) {
  try {
    const u = new URL(url);
    return ["http:", "https:", "mailto:"].includes(u.protocol) ? u.href : null;
  } catch {
    return null;
  }
}

function linkNode(url, children) {
  const href = safeHref(url);
  return href ? { type: "link", href, children } : { type: "span", children };
}

// ---------- DOM ----------

export function renderMarkdown(src, doc = document) {
  const frag = doc.createDocumentFragment();
  for (const b of parseMarkdown(src)) frag.append(block(b, doc));
  return frag;
}

function el(doc, tag, kids = [], cls) {
  const e = doc.createElement(tag);
  if (cls) e.className = cls;
  for (const k of kids) e.append(k);
  return e;
}

function block(b, doc) {
  switch (b.type) {
    case "heading": return el(doc, `h${b.level}`, inlines(b.children, doc));
    case "paragraph": return el(doc, "p", inlines(b.children, doc));
    case "hr": return el(doc, "hr");
    case "code": {
      const code = el(doc, "code", [doc.createTextNode(b.text)], b.lang ? `lang-${b.lang.replace(/[^\w+#.-]/g, "")}` : undefined);
      return el(doc, "pre", [code]);
    }
    case "quote": return el(doc, "blockquote", b.children.map((c) => block(c, doc)));
    case "table": {
      const cell = (tag, kids, j) => { const c = el(doc, tag, inlines(kids, doc)); if (b.align[j]) c.style.textAlign = b.align[j]; return c; };
      const thead = el(doc, "thead", [el(doc, "tr", b.head.map((c, j) => cell("th", c, j)))]);
      const tbody = el(doc, "tbody", b.rows.map((r) => el(doc, "tr", r.map((c, j) => cell("td", c, j)))));
      return el(doc, "div", [el(doc, "table", [thead, tbody])], "md-table");
    }
    case "list": {
      const list = el(doc, b.ordered ? "ol" : "ul", b.items.map((blocks) => {
        // Tight lists render a lone paragraph inline, without <p> margins.
        const kids = !b.loose && blocks[0]?.type === "paragraph"
          ? [...inlines(blocks[0].children, doc), ...blocks.slice(1).map((x) => block(x, doc))]
          : blocks.map((x) => block(x, doc));
        return el(doc, "li", kids);
      }));
      if (b.ordered && b.start !== 1) list.setAttribute("start", String(b.start));
      return list;
    }
  }
  return doc.createTextNode("");
}

function inlines(nodes, doc) {
  return nodes.map((n) => {
    switch (n.type) {
      case "text": return doc.createTextNode(n.text);
      case "br": return el(doc, "br");
      case "code": return el(doc, "code", [doc.createTextNode(n.text)]);
      case "strong": return el(doc, "strong", inlines(n.children, doc));
      case "em": return el(doc, "em", inlines(n.children, doc));
      case "del": return el(doc, "del", inlines(n.children, doc));
      case "span": return el(doc, "span", inlines(n.children, doc));
      case "link": {
        const a = el(doc, "a", inlines(n.children, doc));
        a.href = n.href;
        a.target = "_blank";
        a.rel = "noopener noreferrer nofollow";
        return a;
      }
    }
    return doc.createTextNode("");
  });
}
