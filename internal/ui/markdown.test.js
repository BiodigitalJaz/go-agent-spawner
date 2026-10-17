// Run: node --test internal/ui/markdown.test.js
import { test } from "node:test";
import assert from "node:assert/strict";
import { renderMarkdown, safeHref } from "./static/markdown.js";

// A minimal DOM: enough for renderMarkdown, serialized with escaping so the
// tests see exactly what the browser would build.
const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
class Node_ {
  constructor(tag) { this.tag = tag; this.kids = []; this.attrs = {}; this.style = {}; }
  append(...k) { this.kids.push(...k); }
  setAttribute(k, v) { this.attrs[k] = v; }
  set className(v) { this.attrs.class = v; }
  set href(v) { this.attrs.href = v; }
  set target(v) { this.attrs.target = v; }
  set rel(v) { this.attrs.rel = v; }
  html() {
    const inner = this.kids.map((k) => k.html()).join("");
    if (!this.tag) return inner;
    const style = this.style.textAlign ? ` style="text-align:${this.style.textAlign}"` : "";
    const attrs = Object.entries(this.attrs).map(([k, v]) => ` ${k}="${esc(v)}"`).join("") + style;
    return ["br", "hr"].includes(this.tag) ? `<${this.tag}${attrs}>` : `<${this.tag}${attrs}>${inner}</${this.tag}>`;
  }
}
const doc = {
  createElement: (t) => new Node_(t),
  createDocumentFragment: () => new Node_(null),
  createTextNode: (s) => ({ html: () => esc(s) }),
};
const md = (s) => renderMarkdown(s, doc).html();

test("headings, emphasis, code", () => {
  assert.equal(md("## Title **bold** *it* ~~no~~ `x<y`"), "<h2>Title <strong>bold</strong> <em>it</em> <del>no</del> <code>x&lt;y</code></h2>");
  assert.equal(md("```go\nfunc main() {}\n<b>\n```"), '<pre><code class="lang-go">func main() {}\n&lt;b&gt;</code></pre>');
});

test("single newlines are line breaks, blank lines split paragraphs", () => {
  assert.equal(md("Title — Author\nOther — Writer\n\nNext"), "<p>Title — Author<br>Other — Writer</p><p>Next</p>");
});

test("lists: tight, nested, ordered with start", () => {
  assert.equal(md("- a\n- b **c**\n  - nested\n- d"), "<ul><li>a</li><li>b <strong>c</strong><ul><li>nested</li></ul></li><li>d</li></ul>");
  assert.equal(md("3. three\n4. four"), '<ol start="3"><li>three</li><li>four</li></ol>');
  assert.equal(md("* one\n\n* two"), "<ul><li><p>one</p></li><li><p>two</p></li></ul>");
});

test("tables with alignment and escaped pipes", () => {
  assert.equal(md("| a | b |\n|:--|--:|\n| 1 | x \\| y |"),
    '<div class="md-table"><table><thead><tr><th style="text-align:left">a</th><th style="text-align:right">b</th></tr></thead>' +
    '<tbody><tr><td style="text-align:left">1</td><td style="text-align:right">x | y</td></tr></tbody></table></div>');
});

test("quotes and rules", () => {
  assert.equal(md("> quoted *text*\n> more\n\n---"), "<blockquote><p>quoted <em>text</em><br>more</p></blockquote><hr>");
});

test("raw HTML stays text", () => {
  assert.equal(md('<script>alert(1)</script> <img src=x onerror="y">'),
    "<p>&lt;script&gt;alert(1)&lt;/script&gt; &lt;img src=x onerror=&quot;y&quot;&gt;</p>");
});

test("links are limited to http(s) and mailto; images become links", () => {
  assert.equal(md("[ok](https://example.com/a?b=1) [bad](javascript:alert(1))"),
    '<p><a href="https://example.com/a?b=1" target="_blank" rel="noopener noreferrer nofollow">ok</a> <span>bad</span></p>');
  assert.equal(md("[Go](https://en.wikipedia.org/wiki/Go_(programming_language))"),
    '<p><a href="https://en.wikipedia.org/wiki/Go_(programming_language)" target="_blank" rel="noopener noreferrer nofollow">Go</a></p>');
  assert.equal(md("![chart](https://example.com/c.png)"),
    '<p><a href="https://example.com/c.png" target="_blank" rel="noopener noreferrer nofollow">chart</a></p>');
  assert.equal(md("see https://example.com/x."), '<p>see <a href="https://example.com/x" target="_blank" rel="noopener noreferrer nofollow">https://example.com/x</a>.</p>');
  assert.equal(safeHref("data:text/html,x"), null);
  assert.equal(safeHref("/relative"), null);
});

test("underscores inside words are not emphasis; escapes work", () => {
  assert.equal(md("snake_case_name and \\*literal\\*"), "<p>snake_case_name and *literal*</p>");
});

test("backslashes inside code spans are kept", () => {
  assert.equal(md("use `a\\*b` here"), "<p>use <code>a\\*b</code> here</p>");
});

test("empty and odd input does not throw", () => {
  assert.equal(md(""), "");
  assert.equal(md(null), "");
  assert.equal(md("```\nunclosed"), "<pre><code>unclosed</code></pre>");
  assert.equal(md("|not a table"), "<p>|not a table</p>");
});
