// Agent Spawner console. No framework: a small element helper, hash routing,
// and per-node DOM that is updated in place so streaming text never resets
// scroll position or expanded state.

import { renderMarkdown } from "./markdown.js";

// ---------- helpers ----------
const $ = (sel, root = document) => root.querySelector(sel);

function h(tag, props = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "style") el.style.cssText = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "value") el.value = v; // textarea/select need the property, not the attribute
    else if (k in el && typeof v !== "string") el[k] = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401 && path !== "/v1/login") {
    showLogin("Your session has ended. Sign in again.");
    throw Object.assign(new Error("Sign in required"), { status: 401 });
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || `${res.status} ${res.statusText}`);
    err.status = res.status;
    throw err;
  }
  return data;
}

let toastTimer;
function toast(msg, error = false) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "show" + (error ? " error" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.className = ""), error ? 6000 : 2600);
}

const ms = (t) => (t ? Date.parse(t) : null);
function fmtDur(msec) {
  if (msec == null || msec < 0) return "–";
  if (msec < 1000) return `${Math.round(msec)}ms`;
  const s = msec / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`;
  const m = Math.floor(s / 60);
  return `${m}m ${String(Math.round(s % 60)).padStart(2, "0")}s`;
}
function fmtAgo(t) {
  const s = (Date.now() - t) / 1000;
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return new Date(t).toLocaleDateString();
}
const TERMINAL = new Set(["done", "failed", "cancelled"]);
const STATUS_ORDER = ["done", "running", "reducing", "waiting", "queued", "failed", "cancelled"];
const STATUS_COLOR = {
  done: "var(--c-done)", running: "var(--c-exec)", reducing: "var(--c-reduce)", waiting: "var(--c-waiting)",
  queued: "var(--c-queued)", failed: "var(--c-failed)", cancelled: "var(--c-cancelled)",
};
const pill = (status, live) => h("span", { class: `pill s-${status}${live ? " live" : ""}` }, status);

// Elements users click must survive re-renders: a click only fires when
// mousedown and mouseup land on the same element, and live views repaint
// every 150 ms. These helpers update existing nodes instead of replacing them.
function setText(el, text) { if (el.textContent !== text) el.textContent = text; }
function setPill(el, status, live) {
  const cls = `pill s-${status}${live ? " live" : ""}`;
  if (el.className !== cls) el.className = cls;
  setText(el, status);
}
// once runs fn only when key differs from the last key stored under name in
// memo. Live views repaint every 150 ms; skipping unchanged writes keeps the
// browser from re-laying out the page (which made typing lag during runs).
function once(memo, name, key, fn) { if (memo[name] !== key) { memo[name] = key; fn(); } }
// patchChildren updates parent's children in place from specs
// ({tag, cls, style, title, text}), reusing elements instead of rebuilding.
function patchChildren(parent, specs) {
  while (parent.children.length > specs.length) parent.lastElementChild.remove();
  specs.forEach((sp, i) => {
    let el = parent.children[i];
    if (!el || el.tagName !== sp.tag.toUpperCase()) {
      const fresh = document.createElement(sp.tag);
      if (el) el.replaceWith(fresh); else parent.append(fresh);
      el = fresh;
    }
    if (el.className !== sp.cls) el.className = sp.cls;
    if (el.getAttribute("style") !== sp.style) el.setAttribute("style", sp.style);
    if ((sp.title ?? "") !== el.title) el.title = sp.title ?? "";
    if (sp.text !== undefined && el.textContent !== sp.text) el.textContent = sp.text;
  });
}
// Reorders children to match `want` without touching ones already in place,
// since moving an element would also cancel a click in progress.
function syncOrder(parent, want) {
  want.forEach((el, i) => { if (parent.children[i] !== el) parent.insertBefore(el, parent.children[i] || null); });
  while (parent.children.length > want.length) parent.lastElementChild.remove();
}

// ---------- global state ----------
const app = {
  view: null,        // current view controller with destroy()
  trees: [],
  models: [],
  workers: [],
  settings: null,    // {version, settings, defaults}
};

// ---------- theme ----------
(function initTheme() {
  let saved = null;
  try { saved = localStorage.getItem("theme"); } catch {}
  if (saved) document.documentElement.dataset.theme = saved;
  $("#theme-toggle").addEventListener("click", () => {
    const dark = document.documentElement.dataset.theme
      ? document.documentElement.dataset.theme === "dark"
      : matchMedia("(prefers-color-scheme: dark)").matches;
    const next = dark ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem("theme", next); } catch {}
  });
})();

// ---------- global feeds: tree list, models/workers, lifecycle SSE ----------
let refreshTimer;
function scheduleTreeRefresh(delay = 400) {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(refreshTrees, delay);
}
async function refreshTrees() {
  try {
    app.trees = (await api("GET", "/v1/trees?limit=100")).trees;
    updateVitals();
    app.view?.onTrees?.();
  } catch (e) { /* transient; next event or poll retries */ }
}
async function refreshModels() {
  try {
    const d = await api("GET", "/v1/models");
    app.models = d.models;
    app.workers = d.workers;
    updateVitals();
    app.view?.onModels?.();
  } catch {}
}
async function loadSettings() {
  app.settings = await api("GET", "/v1/settings");
  app.searchEnabled = !!app.settings.search_enabled;
  app.view?.onModels?.(); // views that show search controls follow the setting
  return app.settings;
}

function updateVitals() {
  let agents = 0, active = 0;
  for (const t of app.trees) {
    agents += (t.counts.running || 0) + (t.counts.reducing || 0);
    if (!TERMINAL.has(t.status)) active++;
  }
  $("#v-agents").textContent = agents;
  $("#v-trees").textContent = active;
  $("#v-workers").textContent = app.workers.length;
}

function connectLifecycle() {
  const es = new EventSource("/v1/events");
  const conn = $("#v-conn");
  es.onopen = () => { conn.textContent = "live"; conn.classList.add("live"); };
  es.onerror = async () => {
    conn.textContent = "reconnecting";
    conn.classList.remove("live");
    // An expired session looks like a dropped stream: check before retrying.
    if (!(await checkSessionQuiet())) { es.close(); showLogin("Your session has ended. Sign in again."); }
  };
  const onAny = () => scheduleTreeRefresh();
  for (const t of ["tree.created", "node.started", "node.split", "node.done", "node.failed", "tree.done", "tree.failed", "tree.cancelled"]) {
    es.addEventListener(t, onAny);
  }
}

// ---------- routing ----------
function route() {
  const [, tab = "runs", id] = (location.hash || "#/runs").split("/");
  for (const a of document.querySelectorAll(".tabs a")) a.classList.toggle("active", a.dataset.tab === tab);
  const root = $("#app");
  if ((tab === "runs" || tab === "chat") && app.view?.name === tab) {
    app.view.select(id || null);
    return;
  }
  app.view?.destroy?.();
  root.replaceChildren();
  if (tab === "chat") app.view = chatView(root, id || null);
  else if (tab === "settings") app.view = settingsView(root);
  else if (tab === "models") app.view = modelsView(root);
  else app.view = runsView(root, id || null);
}

// =====================================================================
// Runs view
// =====================================================================
function runsView(root, initialId) {
  const listEl = h("div", { class: "tree-list", role: "list" });
  const detailEl = h("section", { class: "detail" });
  const composer = buildComposer();
  root.append(h("div", { class: "runs" }, h("aside", { class: "sidebar" }, composer.el, listEl), detailEl));

  let selected = null;
  let tree = null; // live tree controller

  const items = new Map(); // tree id -> {a, prompt, pill, count, time, prog, progKey}
  const emptyEl = h("div", { class: "empty" }, "No runs yet. Submit a prompt above.");

  function listItem(id) {
    let it = items.get(id);
    if (!it) {
      it = { prompt: h("div", { class: "prompt" }), pill: h("span"), count: h("span"), time: h("span"), prog: h("div", { class: "progress" }), progKey: "" };
      it.a = h("a", { class: "tree-item", href: `#/runs/${id}`, role: "listitem" },
        it.prompt, h("div", { class: "meta" }, it.pill, it.count, h("span", { class: "grow" }), it.time), it.prog);
      items.set(id, it);
    }
    return it;
  }

  function renderList() {
    if (!app.trees.length) { syncOrder(listEl, [emptyEl]); return; }
    const want = app.trees.map((t) => {
      const it = listItem(t.id);
      const live = !TERMINAL.has(t.status);
      it.a.classList.toggle("selected", t.id === selected);
      // The server sends a preview; cut here too so an old server's full
      // prompts never fill the sidebar the composer shares.
      once(it, "promptKey", t.prompt, () => { it.prompt.textContent = t.prompt.length > 400 ? t.prompt.slice(0, 400) + "…" : t.prompt; });
      setPill(it.pill, t.status, live);
      setText(it.count, `${t.total} agent${t.total === 1 ? "" : "s"}`);
      const created = ms(t.created_at);
      const end = t.finished_at ? ms(t.finished_at) : Date.now();
      setText(it.time, live ? fmtDur(end - created) : fmtAgo(created));
      it.time.title = new Date(created).toLocaleString();
      const parts = STATUS_ORDER.filter((s) => t.counts[s]);
      const key = parts.map((s) => `${s}${t.counts[s]}`).join() + t.total;
      if (key !== it.progKey) {
        it.progKey = key;
        it.prog.title = parts.map((s) => `${t.counts[s]} ${s}`).join(", ");
        it.prog.replaceChildren(...parts.map((s) => h("span", { style: `width:${(100 * t.counts[s]) / t.total}%;background:${STATUS_COLOR[s]}` })));
      }
      return it.a;
    });
    const live = new Set(app.trees.map((t) => t.id));
    for (const id of items.keys()) if (!live.has(id)) items.delete(id);
    syncOrder(listEl, want);
  }

  function select(id) {
    if (id === selected && tree) return;
    selected = id;
    tree?.destroy();
    tree = null;
    renderList();
    if (!id) {
      detailEl.replaceChildren(h("div", { class: "empty" },
        h("p", {}, "Select a run to watch its agents, or start a new one."),
        h("p", { class: "faint" }, "Each run plans, splits into parallel agents, and merges their results.")));
      return;
    }
    tree = treeView(detailEl, id);
  }

  const listTick = setInterval(() => app.trees.some((t) => !TERMINAL.has(t.status)) && renderList(), 1000);
  const poll = setInterval(refreshTrees, 10000);
  renderList();
  select(initialId);

  return {
    name: "runs",
    select,
    onTrees: renderList,
    onModels: () => composer.refreshModels(),
    destroy() { clearInterval(listTick); clearInterval(poll); tree?.destroy(); },
  };
}

function buildComposer() {
  const prompt = h("textarea", { placeholder: "Describe the task for the agent pool…  (Ctrl+Enter to run)", "aria-label": "Prompt" });
  const model = h("select", { "aria-label": "Worker model" });
  const depth = h("input", { type: "number", min: 0, max: 4, placeholder: "depth", "aria-label": "Max split depth" });
  const children = h("input", { type: "number", min: 2, max: 16, placeholder: "fan-out", "aria-label": "Max children per split" });
  const priority = h("select", { "aria-label": "Priority" },
    h("option", { value: 1 }, "P1"), h("option", { value: 2 }, "P2"), h("option", { value: 3 }, "P3"), h("option", { value: 4 }, "P4"));
  const run = h("button", { class: "btn primary", onclick: submit }, "Run");
  const merge = h("select", { "aria-label": "Merge strategy", title: "How split results are combined" },
    h("option", { value: "" }, "Merge: default"),
    h("option", { value: "llm" }, "Merge: LLM (synthesize)"),
    h("option", { value: "list" }, "Merge: list (code, dedupe lines)"),
    h("option", { value: "concat" }, "Merge: concat (code, verbatim)"));
  const doc = h("textarea", { class: "mono doc", placeholder: "Paste a long document. The prompt above becomes the instruction applied to it.", "aria-label": "Document" });
  // Empty = auto: the server reads documents up to ~6k tokens with one agent
  // (splitting a small document adds a merge that doubles the writing).
  const chunks = h("input", { type: "number", min: 1, max: 64, placeholder: "auto", "aria-label": "Chunks", title: "Leave empty for auto (one agent up to about 6k tokens, more chunks above). 1 = whole document in one agent; more = split across parallel agents" });
  const docInfo = h("span", { class: "faint" });
  doc.addEventListener("input", () => setText(docInfo, doc.value ? `≈${Math.round(doc.value.length / 4).toLocaleString()} tokens` : ""));
  const reference = h("textarea", { class: "mono doc", placeholder: "Reference material every agent sees: a page's text, rules the answer must respect (e.g. a brand guide's don'ts).", "aria-label": "Reference" });
  const refInfo = h("span", { class: "faint" });
  reference.addEventListener("input", () => setText(refInfo, reference.value ? `${reference.value.length.toLocaleString()} / 24,000 characters` : ""));
  const docBox = h("details", { class: "docbox" },
    h("summary", {}, "Document & reference (optional)"),
    h("div", { class: "doc-label" }, "Document ", h("span", { class: "faint" }, "split across agents")),
    doc,
    h("div", { class: "doc-opts" }, h("label", {}, "Chunks ", chunks), docInfo),
    h("div", { class: "doc-label" }, "Reference ", h("span", { class: "faint" }, "shared with every agent")),
    reference,
    h("div", { class: "doc-opts" }, refInfo));
  // Agents cannot browse; linked pages are fetched into the reference.
  const fetchUrls = h("input", { type: "checkbox", checked: true, "aria-label": "Fetch linked pages" });
  const fetchLabel = h("label", { class: "check", title: "Download the web pages linked in the prompt and give their text to every agent. Agents cannot open links themselves." }, fetchUrls, " Fetch linked pages");
  // Rules check of the final answer against the reference (default from settings).
  const checkAnswer = h("input", { type: "checkbox", checked: app.settings?.settings?.check_answer !== false, "aria-label": "Rules check" });
  // Web search before planning (only offered when the server has a backend).
  const searchWeb = h("input", { type: "checkbox", "aria-label": "Search the web" });
  const searchLabel = h("label", { class: "check", hidden: true, title: "Search the web first (SearXNG) and give every agent the results with their addresses. Agents cannot search on their own." }, searchWeb, " Search the web");
  let checkTouched = false;
  checkAnswer.addEventListener("change", () => { checkTouched = true; });
  const checkLabel = h("label", { class: "check", title: "When there is reference material, check the final answer against it: recommendations that conflict with it are removed or fixed, and the changes are listed." }, checkAnswer, " Rules check");

  function refreshModels() {
    const s = app.settings?.settings;
    const current = model.value;
    model.replaceChildren(
      h("option", { value: "" }, s ? `Worker model: ${s.worker.model} (default)` : "Worker model: default"),
      ...app.models.map((m) => h("option", { value: m.name }, `Worker model: ${m.name} — ${m.model}`)));
    model.value = current;
    searchLabel.hidden = !app.searchEnabled;
    if (s) {
      if (!checkTouched) checkAnswer.checked = s.check_answer !== false;
      depth.placeholder = `depth ${s.max_depth}`;
      children.placeholder = `fan-out ${s.max_children}`;
    }
  }

  // retry writes the last reply again with search on, in place.
  async function retry(b) {
    if (!current || send.disabled) return;
    send.disabled = true;
    try {
      const res = await api("POST", `/v1/chats/${current}/retry`, { search_mode: "on", deep: deepBox.checked, source_check: checkBox.checked });
      b.el.classList.remove("error");
      b.text = "";
      b.body.replaceChildren();
      b.body.classList.remove("out", "md");
      b.foot.replaceChildren();
      follow(res.tree_id, b);
    } catch (e) {
      toast(e.message, true);
      send.disabled = false;
    }
  }

  async function submit() {
    const text = prompt.value.trim();
    if (!text) { prompt.focus(); return; }
    const body = { prompt: text, priority: Number(priority.value) };
    if (model.value) body.model = model.value;
    if (depth.value !== "") body.max_depth = Number(depth.value);
    if (children.value !== "") body.max_children = Number(children.value);
    if (doc.value.trim()) { body.document = doc.value; if (chunks.value !== "") body.chunks = Number(chunks.value); }
    if (merge.value) body.merge = merge.value;
    if (reference.value.trim()) body.context = reference.value;
    body.fetch_urls = fetchUrls.checked;
    body.check = checkAnswer.checked;
    if (searchWeb.checked) body.search = true;
    run.disabled = true;
    try {
      const res = await api("POST", "/v1/trees", body);
      prompt.value = "";
      doc.value = "";
      reference.value = "";
      setText(docInfo, "");
      setText(refInfo, "");
      if (res.fetched?.length) toast(`Fetched ${res.fetched.map((p) => p.title || p.url).join(", ")}${res.fetched.some((p) => p.truncated) ? " (truncated to fit)" : ""}`);
      location.hash = `#/runs/${res.tree_id}`;
      scheduleTreeRefresh(50);
    } catch (e) {
      toast(e.message, true);
    } finally {
      run.disabled = false;
    }
  }
  prompt.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); submit(); }
  });
  refreshModels();

  const el = h("div", { class: "composer" },
    prompt,
    h("div", { class: "opts" }, model, depth, children, priority, merge),
    docBox,
    // Options on one line, then a full-width Run: the sidebar is narrow.
    h("div", { class: "actions" },
      h("div", { class: "checks" }, fetchLabel, checkLabel, searchLabel),
      run,
      h("span", { class: "faint overrides" }, "Overrides apply to this run only")));
  return { el, refreshModels };
}

// ---------- live tree ----------
function treeView(root, id) {
  const nodes = new Map();
  const t = { id, prompt: "", status: "queued", created: null, settings: null, final: null, finalError: null };
  let es = null;
  let dirty = true;
  let destroyed = false;

  root.replaceChildren(h("div", { class: "empty" }, "Loading…"));

  function node(nid) {
    let n = nodes.get(nid);
    if (!n) {
      n = {
        id: nid, parent: null, idx: 0, depth: 0, title: "", prompt: "", role: "", model: "", status: "queued",
        created: null, started: null, execStarted: null, split: null, reduceStarted: null, finished: null,
        firstThink: null, firstDelta: null, reduceFirstDelta: null,
        text: "", think: "", reason: "", error: "", stats: null, frozen: false, children: [], phase: null,
      };
      nodes.set(nid, n);
    }
    return n;
  }

  function link(n) {
    if (!n.parent) return;
    const p = node(n.parent);
    if (!p.children.includes(n.id)) {
      p.children.push(n.id);
      p.children.sort((a, b) => node(a).idx - node(b).idx);
    }
  }

  function loadSnapshot(d) {
    t.prompt = d.prompt;
    t.status = d.status;
    t.created = ms(d.created_at);
    t.settings = d.settings;
    t.context = d.context || "";
    for (const r of d.nodes) {
      const n = node(r.id);
      Object.assign(n, {
        parent: r.parent_id || null, idx: r.idx, depth: r.depth, title: r.title, prompt: r.prompt,
        role: r.role, model: r.model, status: r.status, created: ms(r.created_at),
        started: ms(r.started_at), split: ms(r.split_at), reduceStarted: ms(r.reduce_started_at), finished: ms(r.finished_at),
        stats: r.stats || null, error: r.error || "",
      });
      // Terminal nodes already have their full output; ignore replayed deltas.
      if (TERMINAL.has(r.status)) { n.text = r.output || ""; n.frozen = true; }
      link(n);
      if (r.id === id) {
        if (r.status === "done") t.final = r.output || "";
        if (r.status === "failed") t.finalError = r.error || "failed";
      }
    }
  }

  function apply(type, e) {
    const at = ms(e.time) || Date.now();
    if (type.startsWith("tree.")) {
      if (type === "tree.done") {
        t.status = "done";
        t.final = e.text;
        // Large results arrive cut in the event; the full text is in the API.
        if (e.text_truncated) api("GET", `/v1/trees/${id}`).then((d) => {
          const r = d.nodes.find((n) => n.id === id);
          if (r?.output) { t.final = r.output; dirty = true; }
        }).catch(() => {});
      }
      if (type === "tree.failed") { t.status = "failed"; t.finalError = e.text; }
      if (type === "tree.cancelled") {
        t.status = "cancelled";
        for (const n of nodes.values()) if (!TERMINAL.has(n.status)) { n.status = "cancelled"; n.finished ??= at; }
      }
      dirty = true;
      return;
    }
    const n = node(e.node_id);
    // Replayed history must not move a finished node back to an earlier status.
    const setStatus = (st) => { if (!n.frozen) n.status = st; };
    if (e.parent_id && !n.parent) { n.parent = e.parent_id; link(n); }
    if (e.title && !n.title) n.title = e.title;
    switch (type) {
      case "node.started":
        // The same phase starting again is a retry (e.g. after a shared
        // context overflowed): drop the abandoned attempt's partial text.
        if (!n.frozen && n.phase === e.role && (e.role === "exec" || e.role === "reduce")) { n.text = ""; n.think = ""; }
        if (e.role === "reduce") { n.reduceStarted ??= at; setStatus("reducing"); n.phase = "reduce"; }
        else {
          n.started ??= at;
          if (e.role === "exec" && n.role === "plan") n.execStarted ??= at;
          setStatus("running");
          n.phase = e.role;
          n.role ||= e.role;
        }
        if (t.status === "queued") t.status = "running";
        break;
      case "thinking":
        // Reasoning is never stored in the DB, so replaying it is not a duplicate.
        n.think += e.text;
        n.firstThink ??= at;
        break;
      case "delta":
        // Finished nodes already hold their full output from the snapshot; keep
        // only the timing so the timeline can still draw the thinking phase.
        if (n.phase === "reduce") n.reduceFirstDelta ??= at; else n.firstDelta ??= at;
        if (!n.frozen) n.text += e.text;
        break;
      case "node.split":
        n.split ??= at;
        setStatus("waiting");
        n.reason = e.text || "";
        (e.children || []).forEach((c, i) => {
          const k = node(c.id);
          k.parent = n.id; k.idx = i; k.depth = n.depth + 1; k.title ||= c.title; k.role ||= c.role; k.prompt ||= c.prompt || ""; k.created ??= at;
          link(k);
        });
        break;
      case "node.done":
        n.status = "done"; n.finished ??= at; n.stats = e.stats || n.stats; n.frozen = true;
        break;
      case "node.failed":
        n.status = "failed"; n.finished ??= at; n.error = e.text || "failed"; n.frozen = true;
        break;
    }
    dirty = true;
  }

  // ---- DOM ----
  const head = h("div", { class: "detail-head" });
  const chips = h("div", { class: "chips" });
  const tlRows = h("div", { class: "tl-body" });
  const tlAxis = h("div", { class: "ticks" });
  const concSvg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  const finalBlock = h("div", { class: "block" });
  const graph = h("div", { class: "graph" });
  const cards = new Map(); // node id -> {wrap, card, refs}

  // Reference material the agents were given: collapsed, filled on open, so a
  // long page never sits in the DOM uninvited.
  const refBlock = h("details", { class: "refbox" });
  refBlock.addEventListener("toggle", () => {
    if (refBlock.open && !refBlock.querySelector("pre")) refBlock.append(h("pre", { class: "mono" }, t.context));
  });

  function buildLayout() {
    if (t.context) refBlock.append(h("summary", {}, `Reference material given to every agent (${t.context.length.toLocaleString()} characters)`));
    root.replaceChildren(
      head,
      chips,
      ...(t.context ? [refBlock] : []),
      finalBlock,
      h("div", { class: "block" },
        h("h3", { class: "section-title" }, "Timeline", h("span", { class: "hint" }, "who ran when, and how many in parallel")),
        h("div", { class: "panel timeline" },
          tlRows,
          h("div", { class: "tl-axis" }, h("div"), tlAxis),
          h("div", { class: "tl-conc" }, h("div", { class: "lbl" }, "Parallel agents"), concSvg),
          h("div", { class: "legend" },
            ...[["Queued", "var(--c-queued)"], ["Planning", "var(--c-plan)"], ["Working", "var(--c-exec)"],
              ["Thinking (striped)", "var(--c-thinking)"], ["Waiting on children", "var(--c-waiting)"], ["Merging", "var(--c-reduce)"]]
              .map(([l, c]) => h("span", {}, h("i", { style: `background:${c}` }), l))))),
      h("div", { class: "block" },
        h("h3", { class: "section-title" }, "Agents", h("span", { class: "hint" }, "live output of every spawned agent")),
        graph));
  }

  const headMemo = {};
  const headPrompt = h("div", { class: "prompt", title: "Click to show all" });
  // Document runs carry the whole document in the prompt: show a preview
  // until clicked, so a huge text never sits in the page uninvited.
  let headOpen = false;
  const headText = () => (headOpen || t.prompt.length <= 2000 ? t.prompt : `${t.prompt.slice(0, 2000)}… (${t.prompt.length.toLocaleString()} characters, click to show all)`);
  headPrompt.addEventListener("click", () => { if (t.prompt.length > 2000) { headOpen = !headOpen; headPrompt.textContent = headText(); } });
  const headPill = h("span");
  const headElapsed = h("span", { class: "elapsed" });
  const cancelBtn = h("button", { class: "btn small danger", onclick: cancelTree }, "Cancel run");
  const deleteBtn = h("button", { class: "btn small danger", onclick: deleteTree, title: "Delete this run and every record of it" }, "Delete");
  head.append(headPrompt, h("div", { class: "side" }, headPill, headElapsed, cancelBtn, deleteBtn));

  function renderHead() {
    const live = !TERMINAL.has(t.status);
    const end = live ? Date.now() : Math.max(...[...nodes.values()].map((n) => n.finished || 0), t.created || 0);
    once(headMemo, "prompt", t.prompt, () => { headPrompt.textContent = headText(); });
    setPill(headPill, t.status, live);
    setText(headElapsed, fmtDur(end - t.created));
    cancelBtn.hidden = !live;
    if (!deleteBtn.disabled) setText(deleteBtn, live ? "Cancel & delete" : "Delete");

    const counts = {};
    for (const n of nodes.values()) counts[n.status] = (counts[n.status] || 0) + 1;
    const s = t.settings;
    chips.replaceChildren(...[
      h("span", { class: "chip" }, h("b", {}, nodes.size), " agents"),
      ...STATUS_ORDER.filter((k) => counts[k]).map((k) => h("span", { class: "chip" }, h("b", {}, counts[k]), ` ${k}`)),
      s && h("span", { class: "chip", title: "Settings snapshot this run uses" },
        "planner ", h("b", {}, s.planner.model), " · worker ", h("b", {}, s.worker.model), " · reducer ", h("b", {}, s.reducer.model),
        ` · depth ${s.max_depth} · fan-out ${s.max_children} · merge ${s.merge || "llm"}`),
    ].filter(Boolean));
  }

  let finalKey = null;
  function renderFinal() {
    const key = JSON.stringify([t.final, t.finalError]);
    if (key === finalKey) return;
    finalKey = key;
    if (t.final != null) {
      // Results are Markdown from the models: rendered by default (as DOM,
      // never as HTML), with the raw text one click away. Copy copies raw.
      const out = h("div", { class: "out md" }, renderMarkdown(t.final));
      let raw = false;
      const toggle = h("button", { class: "btn small", title: "Show the raw Markdown", onclick: () => {
        raw = !raw;
        out.classList.toggle("md", !raw);
        out.replaceChildren(raw ? document.createTextNode(t.final) : renderMarkdown(t.final));
        toggle.textContent = raw ? "Rendered" : "Raw";
      } }, "Raw");
      const copy = h("button", { class: "btn small", onclick: () => navigator.clipboard?.writeText(t.final).then(() => toast("Copied")) }, "Copy");
      finalBlock.replaceChildren(
        h("h3", { class: "section-title" }, "Result", h("span", { class: "title-actions" }, toggle, copy)),
        h("div", { class: "panel final" }, out));
    } else if (t.finalError) {
      finalBlock.replaceChildren(h("h3", { class: "section-title" }, "Result"), h("div", { class: "out err" }, t.finalError));
    } else {
      finalBlock.replaceChildren();
    }
  }

  // DFS order: parents before children, children by index.
  function ordered() {
    const out = [];
    const walk = (nid) => { const n = node(nid); out.push(n); n.children.forEach(walk); };
    if (nodes.has(id)) walk(id);
    for (const n of nodes.values()) if (!out.includes(n)) out.push(n); // orphans while events race
    return out;
  }

  function segments(n, now) {
    const segs = [];
    const add = (cls, a, b, title) => { if (a != null) segs.push({ cls, a, b: b ?? now, title }); };
    const end = n.finished ?? (TERMINAL.has(n.status) ? n.started ?? n.created : null);
    add("queued", n.created, n.started ?? (n.status === "queued" ? null : n.created), "queued");
    if (n.started) {
      const firstEnd = n.split ?? (n.reduceStarted ? n.reduceStarted : end);
      if (n.role === "plan" && n.execStarted) {
        add("plan", n.started, n.execStarted, "planning");
        add("exec", n.execStarted, firstEnd, "answering directly");
        if (n.firstDelta && n.firstDelta > n.execStarted) add("think", n.execStarted, n.firstDelta, "thinking");
      } else {
        const cls = n.role === "plan" ? "plan" : "exec";
        add(cls, n.started, firstEnd, cls === "plan" ? "planning" : "working");
        if (cls === "exec" && n.firstDelta && n.firstDelta > n.started) add("think", n.started, n.firstDelta, "thinking");
        if (cls === "exec" && !n.firstDelta && n.firstThink && !end) add("think", n.started, null, "thinking");
      }
    }
    if (n.split) add("waiting", n.split, n.reduceStarted ?? end, "waiting on children");
    if (n.reduceStarted) {
      add("reduce", n.reduceStarted, end, "merging results");
      if (n.reduceFirstDelta) add("think", n.reduceStarted, n.reduceFirstDelta, "thinking");
    }
    return segs;
  }

  const tlRowEls = new Map();
  const nowLine = h("div", { class: "now-line" });
  function tlRow(nid) {
    let r = tlRowEls.get(nid);
    if (!r) {
      r = { role: h("span", { class: "role" }), title: h("span", { class: "t" }), track: h("div", { class: "tl-track" }) };
      r.label = h("div", { class: "tl-label", onclick: () => focusCard(nid) }, r.role, r.title);
      r.row = h("div", { class: "tl-row" }, r.label, r.track);
      tlRowEls.set(nid, r);
    }
    return r;
  }

  function niceStep(span) {
    const steps = [500, 1e3, 2e3, 5e3, 1e4, 15e3, 3e4, 6e4, 12e4, 3e5, 6e5, 9e5, 18e5, 36e5];
    return steps.find((s) => span / s <= 7) || 36e5;
  }

  function renderTimeline() {
    const live = !TERMINAL.has(t.status);
    const now = Date.now();
    const list = ordered();
    const t0 = t.created ?? Math.min(...list.map((n) => n.created || now));
    let t1 = live ? now : Math.max(...list.map((n) => n.finished || n.started || n.created || t0));
    if (t1 - t0 < 2000) t1 = t0 + 2000;
    const span = (t1 - t0) * 1.02;
    const pct = (x) => `${(100 * (x - t0)) / span}%`;
    const wpct = (a, b) => `${(100 * Math.max(b - a, 0)) / span}%`;

    const want = list.map((n) => {
      const r = tlRow(n.id);
      const specs = segments(n, now).map((s) => ({ tag: "div", cls: `seg ${s.cls}${!s.b || (live && s.b === now) ? " live" : ""}`,
        style: `left:${pct(s.a)};width:${wpct(s.a, s.b)}`, title: `${s.title}: ${fmtDur(s.b - s.a)}` }));
      if (n.finished && TERMINAL.has(n.status)) {
        specs.push({ tag: "span", cls: `cap ${n.status}`, style: `left:${pct(n.finished)}`, text: n.status === "done" ? "✓" : n.status === "failed" ? "✕" : "–" });
      }
      patchChildren(r.track, specs);
      const role = n.id === id ? (n.split ? "plan" : n.role || "plan") : n.role || "exec";
      if (r.role.className !== `role role-${role}`) r.role.className = `role role-${role}`;
      setText(r.role, role === "exec" ? "work" : role);
      setText(r.title, n.id === id ? "Root task" : n.title || "…");
      once(r, "labelKey", `${n.title}|${n.depth}`, () => {
        r.label.title = n.title || "Root task";
        r.label.style.paddingLeft = `${n.depth * 14}px`;
      });
      return r.row;
    });
    if (live) {
      nowLine.style.left = `calc(210px + (100% - 210px) * ${(now - t0) / span})`;
      want.push(nowLine);
    }
    syncOrder(tlRows, want);

    const step = niceStep(span);
    const ticks = [];
    for (let x = 0; x <= span; x += step) ticks.push({ tag: "span", cls: "tick", style: `left:${(100 * x) / span}%`, text: x === 0 ? "0" : fmtDur(x) });
    patchChildren(tlAxis, ticks);

    renderConcurrency(list, t0, span, now);
  }

  // Step chart of how many agents were generating (not queued or waiting).
  function renderConcurrency(list, t0, span, now) {
    if (!Number.isFinite(t0) || !(span > 0)) return;
    const pts = [];
    for (const n of list) for (const s of segments(n, now)) {
      // A segment can lack a timestamp while events are still arriving.
      if ((s.cls === "plan" || s.cls === "exec" || s.cls === "reduce") && Number.isFinite(s.a) && Number.isFinite(s.b)) { pts.push([s.a, 1], [s.b, -1]); }
    }
    pts.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
    let cur = 0, peak = 1;
    const steps = [[t0, 0]];
    for (const [x, d] of pts) { cur += d; peak = Math.max(peak, cur); steps.push([x, cur]); }
    steps.push([t0 + span, cur]);
    const W = 1000, H = 38;
    const X = (x) => ((x - t0) / span) * W;
    const Y = (v) => H - 2 - (v / peak) * (H - 8);
    let d = `M0 ${H} L0 ${Y(0)}`;
    let prev = 0;
    for (const [x, v] of steps) { d += ` L${X(x).toFixed(1)} ${Y(prev).toFixed(1)} L${X(x).toFixed(1)} ${Y(v).toFixed(1)}`; prev = v; }
    d += ` L${W} ${H} Z`;
    concSvg.setAttribute("viewBox", `0 0 ${W} ${H}`);
    concSvg.setAttribute("preserveAspectRatio", "none");
    concSvg.innerHTML = `<path d="${d}" fill="color-mix(in srgb, var(--c-exec) 22%, transparent)" stroke="var(--c-exec)" stroke-width="1.2" vector-effect="non-scaling-stroke"/>` +
      `<text x="${W - 4}" y="10" text-anchor="end" font-size="10" fill="var(--muted)" style="font:10px system-ui">peak ${peak}</text>`;
  }

  function cardFor(n) {
    let c = cards.get(n.id);
    if (c) return c;
    const refs = {
      role: h("span", { class: "role" }),
      title: h("span", { class: "title" }),
      status: h("span"),
      meta: h("div", { class: "card-meta" }),
      task: h("div", { class: "task", title: "Click to expand" }),
      reason: h("div", { class: "reason" }),
      thinkSum: h("span", { class: "lbl" }),
      thinkText: h("div", { class: "think-text" }),
      out: h("div", { class: "out" }),
      err: h("div", { class: "out err" }),
    };
    refs.think = h("details", { class: "thinking", style: "display:none" }, h("summary", {}, refs.thinkSum), refs.thinkText);
    const expand = h("button", { class: "link", onclick: () => { refs.out.classList.toggle("expanded"); expand.textContent = refs.out.classList.contains("expanded") ? "collapse" : "expand"; } }, "expand");
    refs.foot = h("div", { class: "card-foot" }, expand);
    const card = h("article", { class: "panel card", id: `card-${n.id}` },
      h("div", { class: "card-head" }, refs.role, refs.title, refs.status),
      refs.meta, refs.task, refs.reason, refs.think, refs.out, refs.err, refs.foot);
    const kids = h("div", { class: "children" });
    const wrap = h("div", { class: "node-wrap" }, card, kids);
    c = { wrap, card, kids, refs, lastText: -1, lastThink: -1, memo: {}, stickOut: true, stickThink: true, prompt: "" };
    // A part's prompt can hold a whole file. Keep only a preview in the page
    // until it is opened: laying out every card's full input made the page
    // slow to respond (typing in the composer lagged during big runs).
    refs.task.addEventListener("click", () => {
      const open = refs.task.classList.toggle("open");
      refs.task.textContent = open ? c.prompt : taskPreview(c.prompt);
    });
    // Track "following the bottom" from scroll events instead of measuring on
    // every update: measuring forces a layout per streaming card per frame.
    refs.out.addEventListener("scroll", () => { c.stickOut = refs.out.scrollHeight - refs.out.scrollTop - refs.out.clientHeight < 30; }, { passive: true });
    refs.thinkText.addEventListener("scroll", () => { c.stickThink = refs.thinkText.scrollHeight - refs.thinkText.scrollTop - refs.thinkText.clientHeight < 30; }, { passive: true });
    cards.set(n.id, c);
    return c;
  }

  // streamInto appends only the new part of a growing text (streams only
  // grow), rewriting the node when the text was replaced.
  function streamInto(el, text, last) {
    if (last >= 0 && last <= text.length && el.firstChild?.nodeType === Node.TEXT_NODE && el.childNodes.length === 1 && el.firstChild.length === last) {
      el.firstChild.appendData(text.slice(last));
    } else {
      el.textContent = text;
    }
  }

  function taskPreview(p) { return p.length > 600 ? p.slice(0, 600) + "…" : p; }

  function renderCards() {
    const now = Date.now();
    const follow = []; // scrolled after all writes: one layout, not one per card
    for (const n of ordered()) {
      const c = cardFor(n);
      const parentKids = n.parent && cards.has(n.parent) ? cards.get(n.parent).kids : graph;
      if (c.wrap.parentElement !== parentKids) {
        // Insert in index order among siblings.
        const sib = [...parentKids.children].find((el) => (nodes.get(el.dataset.id)?.idx ?? 1e9) > n.idx);
        c.wrap.dataset.id = n.id;
        parentKids.insertBefore(c.wrap, sib || null);
      }
      const r = c.refs;
      const m = c.memo;
      const isRoot = n.id === id;
      const role = n.phase || (n.split ? "plan" : n.role || "exec");
      once(m, "role", role, () => {
        r.role.className = `role role-${role}`;
        r.role.textContent = role === "exec" ? "work" : role === "reduce" ? "merge" : role;
      });
      setText(r.title, isRoot ? "Root task" : n.title || "…");
      const liveNode = !TERMINAL.has(n.status) && n.status !== "queued" && n.status !== "waiting";
      const shown = n.status === "running" && !n.text && n.think ? "thinking" : n.status;
      once(m, "status", `${shown}|${liveNode}`, () => {
        r.status.replaceChildren(pill(shown, liveNode));
        c.card.classList.toggle("active", liveNode);
      });

      const st = n.stats;
      const startedFor = n.phase === "reduce" ? n.reduceStarted : n.started;
      const codeMerge = st?.model?.startsWith("merge:");
      const running = !st && liveNode && startedFor ? fmtDur(now - startedFor) : "";
      once(m, "meta", `${n.model}|${running}|${st ? JSON.stringify(st) : ""}`, () => r.meta.replaceChildren(...[
        codeMerge ? h("span", { title: "Combined in code, no model call" }, "merged in code (", h("b", {}, st.model.slice(6)), ")")
          : n.model && h("span", {}, "model ", h("b", {}, st?.model || n.model)),
        st && !codeMerge ? h("span", {}, "TTFT ", h("b", {}, fmtDur(st.ttft_ms))) : null,
        st && !codeMerge ? h("span", {}, h("b", {}, (st.tokens_per_second || 0).toFixed(1)), " tok/s") : null,
        st && !codeMerge ? h("span", {}, h("b", {}, st.tokens), " tok") : null,
        st ? h("span", {}, h("b", {}, fmtDur(st.duration_ms))) : liveNode && startedFor ? h("span", {}, "running ", h("b", {}, fmtDur(now - startedFor))) : null,
        st?.endpoint && h("span", { class: "faint", title: "Backend endpoint" }, st.endpoint.replace(/^https?:\/\//, "")),
        st?.merge_batches ? h("span", { title: "Results exceeded the merge context, so they were consolidated in parallel batches first" },
          "merged via ", h("b", {}, st.merge_batches), ` batches / ${st.merge_passes} pass${st.merge_passes === 1 ? "" : "es"}`) : null,
        st?.truncated ? h("span", { class: "warn-text", title: "Inputs that alone exceeded the merge context were cut (marked in the text)" }, `⚠ ${st.truncated} truncated`) : null,
        st?.checked ? h("span", { title: "The final answer was checked against the reference material; see the Rules check section" }, "✓ rules-checked") : null,
        st?.source_checked ? h("span", { title: "The answer was compared with the web pages it came from; unsupported specifics were removed or marked unverified" }, "✓ sources-checked") : null,
        st?.check_error ? h("span", { class: "warn-text", title: st.check_error }, "⚠ rules check failed") : null,
      ].filter(Boolean)));

      // Compare against the last value written, never read textContent
      // back: prompts can be tens of thousands of characters per card.
      const prompt = isRoot ? "" : n.prompt;
      once(m, "prompt", prompt, () => {
        c.prompt = prompt;
        r.task.textContent = r.task.classList.contains("open") ? prompt : taskPreview(prompt);
        r.task.style.display = prompt ? "" : "none";
      });
      once(m, "reason", n.reason, () => { r.reason.textContent = n.reason ? `Split: ${n.reason}` : ""; });

      if (n.think.length !== c.lastThink) {
        once(m, "thinkShown", !!n.think, () => { r.think.style.display = n.think ? "" : "none"; });
        streamInto(r.thinkText, n.think, c.lastThink);
        c.lastThink = n.think.length;
        if (c.stickThink) follow.push(r.thinkText);
      }
      const thinkingNow = liveNode && n.think && !(n.phase === "reduce" ? n.reduceFirstDelta : n.firstDelta);
      once(m, "thinkSum", `${!!thinkingNow}|${n.think.length}`, () => {
        r.think.classList.toggle("live", !!thinkingNow);
        r.thinkSum.textContent = `${thinkingNow ? "thinking" : "reasoning"} · ${n.think.length.toLocaleString()} chars`;
      });

      // Root output duplicates the Result panel once done; show it only while streaming.
      const text = isRoot && n.status === "done" ? "" : n.text;
      if (n.status === "done" && text) {
        // Finished output is rendered as Markdown; streaming stays plain text.
        once(m, "outMd", text, () => {
          r.out.replaceChildren(renderMarkdown(text));
          r.out.classList.add("md");
          c.lastText = text.length;
        });
      } else {
        if (m.outMd !== undefined) { // restarted after being rendered
          m.outMd = undefined;
          r.out.classList.remove("md");
          c.lastText = -1;
        }
        if (text.length !== c.lastText) {
          streamInto(r.out, text, c.lastText);
          c.lastText = text.length;
          if (c.stickOut) follow.push(r.out);
        }
      }
      once(m, "err", n.error, () => { r.err.textContent = n.error; });
      once(m, "foot", text.length > 400, () => { r.foot.style.display = text.length > 400 ? "" : "none"; });
    }
    for (const el of follow) el.scrollTop = 1e9; // clamped to the bottom
  }

  function focusCard(nid) {
    const c = cards.get(nid);
    if (!c) return;
    c.card.scrollIntoView({ behavior: "smooth", block: "center" });
    c.card.classList.remove("flash");
    void c.card.offsetWidth;
    c.card.classList.add("flash");
  }

  async function cancelTree() {
    if (cancelBtn.disabled || !confirm("Cancel this run? Running agents stop immediately.")) return;
    cancelBtn.disabled = true;
    setText(cancelBtn, "Cancelling…");
    try {
      const r = await api("DELETE", `/v1/trees/${id}`);
      toast(`Cancelled ${r.cancelled_jobs} job(s)`);
    } catch (e) {
      toast(e.message, true);
    } finally {
      cancelBtn.disabled = false;
      setText(cancelBtn, "Cancel run");
    }
  }

  // deleteTree removes every record of the run (prompt, agent outputs,
  // reference material, queue jobs, live events). An active run is cancelled
  // first; the purge is retried while its agents finish stopping.
  async function deleteTree() {
    const live = !TERMINAL.has(t.status);
    const what = "its prompt, every agent's output, the reference material and its events";
    if (deleteBtn.disabled || !confirm(live
      ? `Cancel this run and delete it with ${what}? This cannot be undone.`
      : `Delete this run with ${what}? This cannot be undone.`)) return;
    deleteBtn.disabled = true;
    setText(deleteBtn, "Deleting…");
    try {
      if (live) await api("DELETE", `/v1/trees/${id}`);
      for (let attempt = 0; ; attempt++) {
        try {
          await api("DELETE", `/v1/trees/${id}?purge=1`);
          break;
        } catch (e) {
          if (e.status !== 409 || attempt >= 20) throw e;
          await new Promise((r) => setTimeout(r, 1000)); // agents still stopping
        }
      }
      toast("Run deleted");
      app.trees = app.trees.filter((x) => x.id !== id);
      location.hash = "#/runs";
      scheduleTreeRefresh(50);
    } catch (e) {
      toast(e.message, true);
      deleteBtn.disabled = false;
      setText(deleteBtn, live ? "Cancel & delete" : "Delete");
    }
  }

  function render() {
    if (destroyed) return;
    renderHead();
    renderFinal();
    renderTimeline();
    renderCards();
    dirty = false;
  }

  // Coalesce bursts of events into one paint; keep the clock moving while live.
  let raf = 0;
  const pump = setInterval(() => {
    if (dirty || !TERMINAL.has(t.status)) { cancelAnimationFrame(raf); raf = requestAnimationFrame(render); }
  }, 150);

  (async () => {
    try {
      loadSnapshot(await api("GET", `/v1/trees/${id}`));
    } catch (e) {
      root.replaceChildren(h("div", { class: "empty" }, e.status === 404 ? "Run not found." : `Could not load run: ${e.message}`));
      return;
    }
    if (destroyed) return;
    buildLayout();
    render();
    es = new EventSource(`/v1/trees/${id}/events`);
    const types = ["node.started", "thinking", "delta", "node.split", "node.done", "node.failed", "tree.done", "tree.failed", "tree.cancelled"];
    for (const type of types) {
      es.addEventListener(type, (m) => {
        try { apply(type, JSON.parse(m.data)); } catch {}
        if (type.startsWith("tree.")) { es.close(); scheduleTreeRefresh(50); }
      });
    }
  })();

  return {
    destroy() { destroyed = true; clearInterval(pump); cancelAnimationFrame(raf); es?.close(); },
  };
}

// =====================================================================
// Settings view: role models, generation params, directives, limits
// =====================================================================
const ROLES = [
  { key: "planner", name: "Planner", role: "plan", color: "var(--c-plan)",
    desc: "Decides whether a task is answered directly or split into parallel subtasks.",
    note: "Rules the split machinery depends on (subtask count, self-contained prompts, JSON output) are appended automatically." },
  { key: "worker", name: "Worker", role: "exec", color: "var(--c-exec)",
    desc: "Executes each subtask (or the whole task when not split) and streams its answer.",
    note: "Receives only its subtask prompt; it cannot see siblings or the original task." },
  { key: "reducer", name: "Reducer", role: "reduce", color: "var(--c-reduce)",
    desc: "Merges the workers' results into one answer for the parent task.",
    note: "Receives the original task plus every child's result or failure." },
];

// =====================================================================
// Chat view: a conversational agent. Each reply is a run whose root agent
// sees the conversation so far; it streams here like any run.
// =====================================================================
function chatView(root, initialId) {
  const listEl = h("div", { class: "chat-list", role: "list" });
  const main = h("section", { class: "chat-main" });
  const newBtn = h("button", { class: "btn primary chat-new", onclick: () => { location.hash = "#/chat"; } }, "+ New chat");
  root.append(h("div", { class: "chat" }, h("aside", { class: "sidebar" }, h("div", { class: "chat-side-head" }, newBtn), listEl), main));

  let chats = [];
  let current = null;   // conversation id
  let stream = null;    // {es, tree, bubble} of the reply being written
  let destroyed = false;

  async function loadList() {
    try { chats = (await api("GET", "/v1/chats")).chats; } catch (e) { toast(e.message, true); }
    renderList();
  }
  function renderList() {
    if (!chats.length) { listEl.replaceChildren(h("div", { class: "empty" }, "No conversations yet.")); return; }
    listEl.replaceChildren(...chats.map((c) => h("a", { class: `chat-item${c.id === current ? " selected" : ""}`, href: `#/chat/${c.id}`, role: "listitem" },
      h("div", { class: "title" }, c.title || "New chat"),
      h("div", { class: "faint" }, fmtAgo(new Date(c.updated_at).getTime())))));
  }

  // ---- thread ----
  const thread = h("div", { class: "chat-thread", "aria-live": "polite" });
  const input = h("textarea", { placeholder: "Message the agent…  (Enter to send, Shift+Enter for a new line)", "aria-label": "Message", rows: 3 });
  // Per-viewer preferences (best effort: storage may be blocked).
  const pref = (k, d) => { try { return localStorage.getItem(`chat.${k}`) ?? d; } catch { return d; } };
  const savePref = (k, v) => { try { localStorage.setItem(`chat.${k}`, v); } catch {} };
  // Search: Auto (search only when the question needs current or local
  // facts), On, Off. Links in a message are always read.
  let searchMode = pref("search", "auto");
  const modeBtns = ["auto", "on", "off"].map((m) => h("button", { type: "button", class: "mode-btn", "data-mode": m,
    "aria-pressed": String(m === searchMode), onclick: () => setMode(m) }, m === "auto" ? "Auto" : m === "on" ? "On" : "Off"));
  function setMode(m) {
    searchMode = m;
    savePref("search", m);
    for (const b of modeBtns) b.setAttribute("aria-pressed", String(b.dataset.mode === m));
  }
  const searchCtl = h("div", { class: "mode", role: "group", "aria-label": "Web search", hidden: !app.searchEnabled,
    title: "Web search. Auto: only when the question needs current or local facts (laws, prices, schedules, recent events). On: always. Off: never." },
    h("span", { class: "mode-label" }, "Search"), ...modeBtns);
  const deepBox = h("input", { type: "checkbox", "aria-label": "Agent pool", checked: pref("deep", "0") === "1",
    onchange: (e) => savePref("deep", e.target.checked ? "1" : "0") });
  const checkBox = h("input", { type: "checkbox", "aria-label": "Check sources", checked: pref("check", "0") === "1",
    onchange: (e) => savePref("check", e.target.checked ? "1" : "0") });
  const checkLabel = h("label", { class: "check", hidden: !app.searchEnabled,
    title: "After a reply written from web search, compare it with the pages: unsupported fees, dates and rules are removed or marked unverified. The answer is replaced when the check finishes." }, checkBox, " Check sources");
  const send = h("button", { class: "btn primary", onclick: () => submit() }, "Send");
  const stop = h("button", { class: "btn danger", hidden: true, onclick: () => stopReply() }, "Stop");
  const title = h("h2", { class: "chat-title" });
  const del = h("button", { class: "btn small danger", title: "Delete this conversation and its runs", onclick: () => deleteChat() }, "Delete");
  const head = h("div", { class: "chat-head" }, title, del);
  input.addEventListener("keydown", (e) => { if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); submit(); } });
  main.append(head, thread, h("div", { class: "chat-compose" }, input,
    h("div", { class: "chat-opts" },
      searchCtl,
      h("label", { class: "check", title: "Split the reply across parallel agents: slower, for big requests (long reports, many parts)" }, deepBox, " Agent pool"),
      checkLabel,
      h("span", { class: "grow" }), stop, send)));

  function bubble(role) {
    const body = h("div", { class: "bubble-body" });
    const status = h("div", { class: "bubble-status faint" });
    const foot = h("div", { class: "bubble-foot" });
    const el = h("div", { class: `bubble ${role}` }, body, status, foot);
    thread.append(el);
    return { el, body, status, foot, text: "" };
  }
  // showReply renders a finished reply. The last one can be written again:
  // with search when it was answered without (the model can misjudge
  // whether a question needs current facts).
  function showReply(b, m, last) {
    b.el.classList.remove("error");
    if (m.status === "failed") { b.el.classList.add("error"); b.body.textContent = m.error || "The reply failed."; }
    else if (m.status === "cancelled") { b.body.textContent = m.content || ""; setText(b.status, "Stopped."); }
    else { b.body.replaceChildren(renderMarkdown(m.content || "")); b.body.classList.add("out", "md"); }
    if (!m.tree_id) return;
    const items = [h("a", { href: `#/runs/${m.tree_id}`, class: "faint" }, "View run"),
      h("button", { class: "link", onclick: () => navigator.clipboard?.writeText(m.content || "").then(() => toast("Copied")) }, "Copy")];
    if (m.status === "done" && app.searchEnabled) items.push(h("span", { class: "faint", title: m.searched ? "This reply used web search results" : "This reply was written without searching the web" }, m.searched ? "Searched the web" : "No web search"));
    if (last && app.searchEnabled) items.push(h("button", { class: "link retry", onclick: () => retry(b) }, m.searched ? "Answer again" : "Search and answer again"));
    b.foot.replaceChildren(...items);
  }
  function scrollDown() { thread.scrollTop = thread.scrollHeight; }

  async function open(id) {
    closeStream();
    current = id;
    renderList();
    thread.replaceChildren();
    title.textContent = "New chat";
    del.hidden = !id;
    if (!id) { focusInput(); return; }
    let d;
    try { d = await api("GET", `/v1/chats/${id}`); } catch (e) {
      thread.replaceChildren(h("div", { class: "empty" }, e.status === 404 ? "Conversation not found." : e.message));
      return;
    }
    if (destroyed || current !== id) return;
    title.textContent = d.chat.title || "New chat";
    d.messages.forEach((m, i) => {
      const b = bubble(m.role);
      if (m.role === "user") { b.body.textContent = m.content; return; }
      if (m.status && !TERMINAL.has(m.status)) follow(m.tree_id, b);
      else showReply(b, m, i === d.messages.length - 1);
    });
    scrollDown();
    focusInput();
  }
  // On touch devices focusing would scroll the page and pop up the keyboard.
  function focusInput() { if (!matchMedia("(pointer: coarse)").matches) input.focus(); }

  // follow streams a reply: deltas of the root agent, plus status notes.
  function follow(tree, b) {
    closeStream();
    stop.hidden = false;
    send.disabled = true;
    setText(b.status, "Thinking…");
    const es = new EventSource(`/v1/trees/${tree}/events`);
    stream = { es, tree, b };
    let agents = 0, finished = 0, phase = "", searched = false;
    const root = (e) => e.node_id === tree;
    es.addEventListener("node.started", (m) => {
      const e = JSON.parse(m.data);
      if (!root(e)) return;
      if (phase === e.role) { b.text = ""; b.body.textContent = ""; } // a retry or the rules check rewrites the reply
      phase = e.role;
      if (e.role === "reduce") setText(b.status, "Combining the agents' results…");
    });
    es.addEventListener("thinking", (m) => {
      const e = JSON.parse(m.data);
      if (!root(e)) return;
      if (/Queries \(/.test(e.text || "")) searched = true;
      const line = (e.text || "").trim().split("\n").filter(Boolean).pop();
      if (line && /search|query|quer|read|check/i.test(line)) setText(b.status, line);
    });
    es.addEventListener("node.split", (m) => {
      const e = JSON.parse(m.data);
      agents = (e.children || []).length;
      setText(b.status, `${agents} agents working…`);
    });
    es.addEventListener("node.done", (m) => {
      const e = JSON.parse(m.data);
      if (!root(e) && agents) setText(b.status, `${++finished} of ${agents} agents finished…`);
    });
    es.addEventListener("delta", (m) => {
      const e = JSON.parse(m.data);
      if (!root(e)) return;
      b.text += e.text || "";
      b.body.textContent = b.text;
      if (!/Combining/.test(b.status.textContent)) setText(b.status, "");
      scrollDown();
    });
    const finish = async (status, text, truncated) => {
      closeStream();
      let content = text;
      if (truncated) { try { content = (await api("GET", `/v1/trees/${tree}`)).nodes.find((n) => n.id === tree)?.output || text; } catch {} }
      setText(b.status, "");
      showReply(b, { status, content, error: text, tree_id: tree, searched }, true);
      scrollDown();
      loadList();
    };
    es.addEventListener("tree.done", (m) => { const e = JSON.parse(m.data); finish("done", e.text, e.text_truncated); });
    es.addEventListener("tree.failed", (m) => finish("failed", JSON.parse(m.data).text));
    es.addEventListener("tree.cancelled", () => finish("cancelled", b.text));
  }
  function closeStream() {
    if (stream) { stream.es.close(); stream = null; }
    stop.hidden = true;
    send.disabled = false;
  }
  async function stopReply() {
    if (!stream) return;
    try { await api("DELETE", `/v1/trees/${stream.tree}`); } catch (e) { toast(e.message, true); }
  }

  async function submit() {
    const content = input.value.trim();
    if (!content || send.disabled) return;
    send.disabled = true;
    try {
      let id = current;
      if (!id) {
        id = (await api("POST", "/v1/chats", {})).id;
        current = id;
        history.replaceState(null, "", `#/chat/${id}`); // no re-render: stay in this thread
        del.hidden = false;
      }
      const res = await api("POST", `/v1/chats/${id}/messages`, { content, search_mode: app.searchEnabled ? searchMode : "off", deep: deepBox.checked, source_check: checkBox.checked });
      input.value = "";
      for (const r of thread.querySelectorAll(".retry")) r.remove(); // only the last reply can be written again
      const ub = bubble("user");
      ub.body.textContent = content;
      if (res.history_note) toast(res.history_note);
      const dead = (res.fetched || []).filter((p) => p.error);
      if (dead.length) toast(`Couldn't open ${dead.map((p) => p.url).join(", ")}; the agent was told.`, true);
      if (title.textContent === "New chat") title.textContent = content.length > 60 ? content.slice(0, 60) + "…" : content;
      const ab = bubble("assistant");
      scrollDown();
      follow(res.tree_id, ab);
      loadList();
    } catch (e) {
      toast(e.message, true);
      send.disabled = false;
    }
  }

  async function deleteChat() {
    if (!current || !confirm("Delete this conversation and the runs of its replies? This cannot be undone.")) return;
    try {
      await api("DELETE", `/v1/chats/${current}`);
      toast("Conversation deleted");
      location.hash = "#/chat";
      loadList();
    } catch (e) { toast(e.message, true); }
  }

  loadList();
  open(initialId);
  return {
    name: "chat",
    select: (id) => { if (id !== current) open(id); },
    onModels: () => { searchCtl.hidden = checkLabel.hidden = !app.searchEnabled; },
    destroy() { destroyed = true; closeStream(); },
  };
}

function settingsView(root) {
  let loaded = null;   // {version, settings, defaults}
  let form = null;     // working copy of settings
  const banner = h("div");
  const body = h("div");
  const status = h("span", { class: "muted" });
  const save = h("button", { class: "btn primary", onclick: doSave }, "Save settings");
  const discard = h("button", { class: "btn", onclick: () => { form = structuredClone(loaded.settings); build(); } }, "Discard changes");

  root.append(h("div", { class: "page" },
    h("div", { class: "page-head" },
      h("div", { class: "grow" },
        h("h1", {}, "Agents & directives"),
        h("p", {}, "Choose the model and generation settings for each agent role and edit its directive (system prompt). New runs take a snapshot of these settings when they start; runs already in progress are not affected."))),
    banner, body,
    h("div", { class: "savebar" }, status, h("span", { class: "grow" }), discard, save)));

  const isDirty = () => loaded && JSON.stringify(form) !== JSON.stringify(loaded.settings);
  function updateBar() {
    const d = isDirty();
    save.disabled = !d;
    discard.disabled = !d;
    status.textContent = d ? "Unsaved changes"
      : loaded.version ? `Version ${loaded.version} · saved ${fmtAgo(ms(loaded.updated_at))}` : "Using built-in defaults (never saved)";
  }
  const beforeUnload = (e) => { if (isDirty()) { e.preventDefault(); e.returnValue = ""; } };
  addEventListener("beforeunload", beforeUnload);

  function modelSelect(value, onchange) {
    const opts = app.models.map((m) => h("option", { value: m.name }, `${m.name} — ${m.model}`));
    if (!app.models.some((m) => m.name === value)) opts.unshift(h("option", { value }, `${value} (not configured)`));
    const sel = h("select", { onchange: (e) => onchange(e.target.value) }, ...opts);
    sel.value = value;
    return sel;
  }

  function roleCard(def) {
    const r = form[def.key];
    const dflt = loaded.defaults[def.key];
    const tempOn = r.temperature != null;
    const range = h("input", { type: "range", min: 0, max: 2, step: 0.05, value: r.temperature ?? 0.7, disabled: !tempOn,
      oninput: (e) => { r.temperature = Number(e.target.value); num.value = r.temperature; updateBar(); } });
    const num = h("input", { type: "number", min: 0, max: 2, step: 0.05, value: r.temperature ?? "", placeholder: "auto", disabled: !tempOn,
      oninput: (e) => { if (e.target.value !== "") { r.temperature = Number(e.target.value); range.value = r.temperature; } updateBar(); } });
    const toggle = h("input", { type: "checkbox", checked: tempOn, title: "Set temperature (otherwise the model's default)",
      onchange: (e) => {
        r.temperature = e.target.checked ? Number(range.value) : null;
        if (r.temperature == null) delete r.temperature;
        range.disabled = num.disabled = !e.target.checked;
        num.value = e.target.checked ? range.value : "";
        updateBar();
      } });
    const directive = h("textarea", { spellcheck: false, value: r.directive, "aria-label": `${def.name} directive`,
      oninput: (e) => { r.directive = e.target.value; counter.textContent = `${e.target.value.length.toLocaleString()} chars`; updateBar(); } });
    const counter = h("span", { class: "note" }, `${r.directive.length.toLocaleString()} chars`);
    const reset = h("button", { class: "link", onclick: () => { r.directive = dflt.directive; directive.value = r.directive; counter.textContent = `${r.directive.length.toLocaleString()} chars`; updateBar(); } }, "Reset to default");

    return h("section", { class: "panel role-card", style: `--rc:${def.color}` },
      h("h2", {}, h("span", { class: `role role-${def.role}` }, def.role === "exec" ? "work" : def.role === "reduce" ? "merge" : def.role), def.name),
      h("p", { class: "desc" }, def.desc),
      h("label", { class: "field" }, "Model", modelSelect(r.model, (v) => { r.model = v; updateBar(); })),
      h("div", { class: "grid2" },
        h("label", { class: "field" }, h("span", {}, toggle, " Temperature"), h("div", { class: "temp" }, range, num)),
        h("label", { class: "field" }, "Max tokens (0 = no limit)",
          h("input", { type: "number", min: 0, step: 64, value: r.max_tokens || 0,
            oninput: (e) => { const v = Number(e.target.value) || 0; if (v) r.max_tokens = v; else delete r.max_tokens; updateBar(); } }))),
      h("label", { class: "field" }, h("span", { style: "display:flex;justify-content:space-between" }, "Directive", h("span", {}, counter, " · ", reset)), directive),
      h("div", { class: "note" }, def.note));
  }

  function build() {
    body.replaceChildren(
      h("div", { class: "roles" }, ...ROLES.map(roleCard)),
      h("section", { class: "panel limits" },
        h("label", { class: "field" }, "Max split depth",
          h("input", { type: "number", min: 0, max: 4, value: form.max_depth, oninput: (e) => { form.max_depth = Number(e.target.value); updateBar(); } })),
        h("label", { class: "field" }, "Max children per split",
          h("input", { type: "number", min: 2, max: 16, value: form.max_children, oninput: (e) => { form.max_children = Number(e.target.value); updateBar(); } })),
        h("label", { class: "field" }, "Merge strategy",
          (() => {
            const sel = h("select", { onchange: (e) => { form.merge = e.target.value; if (!form.merge) delete form.merge; updateBar(); } },
              h("option", { value: "" }, "LLM (synthesize) — default"),
              h("option", { value: "list" }, "List (code: union of lines)"),
              h("option", { value: "concat" }, "Concat (code: verbatim)"));
            sel.value = form.merge || "";
            return sel;
          })()),
        h("label", { class: "field check-field" },
          h("span", {}, h("input", { type: "checkbox", checked: !!form.allow_internal_fetch,
            onchange: (e) => { if (e.target.checked) form.allow_internal_fetch = true; else delete form.allow_internal_fetch; updateBar(); } }),
            " Allow fetching internal network addresses"),
          h("span", { class: "note" }, "Off by default. When on, “Fetch linked pages” may reach private, loopback, link-local and cluster addresses (internal wikis, dashboards, services). Every attempt on an internal address is logged by the API, allowed or blocked.")),
        app.settings?.search_budget ? h("div", { class: "field check-field" },
          h("span", {}, "Web search budget"),
          h("span", { class: "note" }, (() => {
            const b = app.settings.search_budget;
            return `Exa used $${b.spent_usd.toFixed(3)} of $${b.limit_usd.toFixed(2)} in ${b.month}. Search stays on the free tier: at the cap Exa stops and search continues on SearXNG until the 1st.`;
          })())) : null,
        h("label", { class: "field check-field" },
          h("span", {}, h("input", { type: "checkbox", checked: form.check_answer !== false,
            onchange: (e) => { form.check_answer = e.target.checked; updateBar(); } }),
            " Check the final answer against the reference"),
          h("span", { class: "note" }, "On by default. When a run has reference material, its final answer gets one more pass: recommendations that conflict with the reference are removed or fixed, and a “Rules check” section lists each change. Runs can turn it off with the composer’s “Rules check” box.")),
        h("div", { class: "explain" },
          h("b", {}, "Merge:"), " LLM writes a combined answer (any task, +1 model call). ",
          h("b", {}, "List"), " and ", h("b", {}, "concat"), " combine results in code with no model call — for list-shaped or independent outputs. ",
          h("b", {}, "Depth 0"), " skips planning (one worker answers). ",
          h("b", {}, "Depth 1"), " lets the root split once. ",
          h("b", {}, "Depth 2+"), " lets complex subtasks split again. Fan-out caps how many agents one split can spawn.")));
    updateBar();
  }

  async function load() {
    try {
      loaded = await loadSettings();
      if (!app.models.length) await refreshModels();
      form = structuredClone(loaded.settings);
      banner.replaceChildren();
      build();
    } catch (e) {
      banner.replaceChildren(h("div", { class: "banner error" }, `Could not load settings: ${e.message}`));
    }
  }

  async function doSave() {
    save.disabled = true;
    try {
      const res = await api("PUT", "/v1/settings", { version: loaded.version, settings: form });
      loaded = { ...loaded, ...res };
      app.settings = loaded;
      form = structuredClone(loaded.settings);
      banner.replaceChildren();
      updateBar();
      toast(`Saved · version ${res.version}`);
    } catch (e) {
      if (e.status === 409) {
        banner.replaceChildren(h("div", { class: "banner warn" },
          "Someone else saved settings since you loaded this page. ",
          h("button", { class: "link", onclick: load }, "Reload latest"), " (your unsaved edits will be lost)."));
      } else {
        banner.replaceChildren(h("div", { class: "banner error" }, e.message));
      }
      updateBar();
    }
  }

  load();
  return { destroy() { removeEventListener("beforeunload", beforeUnload); } };
}

// =====================================================================
// Models view: configured backends and live per-worker slot usage
// =====================================================================
function modelsView(root) {
  const poolGrid = h("div", { class: "models" });
  const discBody = h("div", { class: "disc-hosts" });
  const hostInput = h("input", { type: "text", placeholder: "http://host:1234", "aria-label": "Backend host URL" });
  const refreshBtn = h("button", { class: "btn", onclick: () => discover() }, "Refresh");
  const editors = new Map(); // pool model name -> open edit form element
  const bench = benchSection(() => disc);

  root.append(h("div", { class: "page" },
    h("div", { class: "page-head" },
      h("div", { class: "grow" },
        h("h1", {}, "Models & capacity"),
        h("p", {}, "Models the pool routes to, with each worker's learned concurrency per endpoint. Add more from the models your backends offer below. Models from backends.json are read-only; models added here can be edited or removed, and workers pick changes up within seconds."))),
    h("h3", { class: "section-title" }, "In the pool"),
    poolGrid,
    h("h3", { class: "section-title", style: "margin-top:28px" }, "Discover", h("span", { class: "hint" }, "models available on your backends")),
    h("div", { class: "disc-bar" },
      hostInput,
      h("button", { class: "btn", onclick: addHost }, "Add host"),
      h("span", { class: "grow" }),
      refreshBtn),
    discBody,
    bench.el));

  // ---- pool cards: rebuilt only when the model set changes; gauges in place ----
  let poolKey = "";
  const gauges = new Map(); // model name -> element holding live endpoint rows

  function renderPool() {
    const s = app.settings?.settings;
    const key = JSON.stringify([app.models, s && [s.planner.model, s.worker.model, s.reducer.model]]);
    if (key !== poolKey) {
      poolKey = key;
      gauges.clear();
      if (!app.models.length) poolGrid.replaceChildren(h("div", { class: "empty" }, "No models configured."));
      else poolGrid.replaceChildren(...app.models.map(poolCard));
    }
    for (const [name, el] of gauges) el.replaceChildren(...liveRows(name));
  }

  function poolCard(m) {
    const s = app.settings?.settings;
    const uses = s ? ROLES.filter((r) => s[r.key].model === m.name) : [];
    const live = h("div", { class: "ep-rows" });
    gauges.set(m.name, live);
    const editable = m.source === "ui";
    const formSlot = h("div");
    const actions = editable ? h("span", { class: "card-actions" },
      h("button", { class: "btn small", onclick: () => toggleEdit(m, formSlot) }, "Edit"),
      h("button", { class: "btn small danger", onclick: () => removeModel(m) }, "Remove")) : null;
    const reasoning = m.extra?.reasoning_effort;
    return h("section", { class: "panel model-card" },
      h("h2", {}, m.name,
        h("span", { class: "uses" }, ...uses.map((r) => h("span", { class: `role role-${r.role}` }, r.name))),
        h("span", { class: "grow" }),
        h("span", { class: `src src-${m.source}`, title: editable ? "Added in the console" : "Defined in backends.json (read-only)" }, editable ? "console" : "backends.json"),
        actions),
      h("dl", { class: "kv" },
        h("dt", {}, "API model"), h("dd", { class: "mono" }, m.model),
        h("dt", {}, "Endpoints"), h("dd", { class: "mono" }, m.endpoints.join("\n")),
        h("dt", {}, "Limit bounds"), h("dd", {}, `start ${m.initial_limit || 1}, max ${m.max_limit || 64} per worker`),
        h("dt", {}, "Server slots"), h("dd", {}, m.slots ? `${m.slots} shared by all workers` : "no shared cap"),
        h("dt", {}, "Context"), h("dd", {}, m.context_length ? `${m.context_length.toLocaleString()} tokens${m.shared_context ? " shared by all slots" : " per request"}` : "unknown (merges assume 8k)"),
        reasoning && h("dt", {}, "Reasoning"), reasoning && h("dd", {}, reasoning === "none" ? "off" : reasoning),
        m.extra && h("dt", {}, "Extra params"), m.extra && h("dd", { class: "mono" }, JSON.stringify(m.extra))),
      formSlot,
      h("div", { class: "ep" },
        h("div", { class: "section-title", style: "margin:0" }, "Live endpoints", h("span", { class: "hint" }, "in flight / learned limit · baseline TTFT")),
        live));
  }

  function liveRows(name) {
    const rows = [];
    for (const w of app.workers) {
      for (const ep of (w.backends || {})[name] || []) {
        const lim = ep.limiter;
        const cap = Math.max(Math.floor(lim.limit), 1);
        rows.push(h("div", { class: "ep-row" },
          h("div", {}, h("div", { class: "mono" }, ep.url.replace(/^https?:\/\//, "")), h("div", { class: "faint", style: "font-size:11px" }, w.worker)),
          h("div", { title: `${lim.inflight} in flight / limit ${lim.limit.toFixed(2)}` },
            h("div", { class: `gauge${lim.inflight >= cap ? " full" : ""}` }, h("span", { style: `width:${Math.min(100, (100 * lim.inflight) / cap)}%` })),
            h("div", { class: "faint", style: "font-size:11px;margin-top:2px" }, `${lim.inflight} / ${lim.limit.toFixed(1)} slots`)),
          h("div", { style: "text-align:right" },
            ep.ejected ? h("span", { class: "ejected" }, "ejected")
              : h("span", { title: "Baseline time-to-first-token" }, lim.baseline_ttft_ns ? fmtDur(lim.baseline_ttft_ns / 1e6) : "–"))));
      }
    }
    if (rows.length) return rows;
    return [h("div", { class: "faint", style: "font-size:12.5px" }, app.workers.length ? "Waiting for a worker to pick this model up…" : "No workers reporting.")];
  }

  function toggleEdit(m, slot) {
    if (editors.has(m.name)) { editors.get(m.name).remove(); editors.delete(m.name); return; }
    const form = modelForm({
      mode: "edit", name: m.name, model: m.model, endpoints: m.endpoints, initial: m.initial_limit, max: m.max_limit, extra: m.extra,
      context: m.context_length, slots: m.slots, shared: m.shared_context,
      reasoningOptions: discoveredReasoning(m.model),
      onCancel: () => toggleEdit(m, slot),
      onSubmit: async (body) => {
        await api("PUT", `/v1/models/${encodeURIComponent(m.name)}`, body);
        toast(`Updated ${m.name}`);
        editors.delete(m.name);
        await refreshModels();
      },
    });
    editors.set(m.name, form);
    slot.append(form);
  }

  async function removeModel(m) {
    if (!confirm(`Remove ${m.name} from the pool? Workers stop serving it within seconds.`)) return;
    try {
      await api("DELETE", `/v1/models/${encodeURIComponent(m.name)}`);
      toast(`Removed ${m.name}`);
      await refreshModels();
      renderDiscovery();
    } catch (e) { toast(e.message, true); }
  }

  // ---- discovery ----
  let disc = null; // {hosts: [...]}
  let discError = "";
  const busy = new Map(); // row key -> label while a load/unload runs
  const openAdd = new Map(); // row key -> form element (kept across re-renders)
  const HOSTS_KEY = "discovery.hosts";
  let extraHosts = [];
  try { extraHosts = JSON.parse(localStorage.getItem(HOSTS_KEY) || "[]"); } catch {}
  const saveHosts = () => { try { localStorage.setItem(HOSTS_KEY, JSON.stringify(extraHosts)); } catch {} };

  function addHost() {
    const v = hostInput.value.trim();
    if (!/^https?:\/\/[^/]+/.test(v)) { toast("Enter a URL like http://192.168.1.76:1234", true); return; }
    if (!extraHosts.includes(v)) { extraHosts.push(v); saveHosts(); }
    hostInput.value = "";
    discover();
  }

  // Discovery requests can overlap (page load, Add host, Refresh, after a
  // load/unload). Only the newest response may render; an older one finishing
  // late would otherwise drop hosts added since and close open forms.
  let discSeq = 0;
  async function discover() {
    const seq = ++discSeq;
    refreshBtn.disabled = true;
    setText(refreshBtn, "Discovering…");
    let result, error = "";
    try {
      const q = extraHosts.map((u) => `url=${encodeURIComponent(u)}`).join("&");
      result = await api("GET", `/v1/discovery${q ? "?" + q : ""}`);
    } catch (e) {
      error = e.message;
    }
    if (seq !== discSeq) return; // superseded
    refreshBtn.disabled = false;
    setText(refreshBtn, "Refresh");
    if (result) disc = result;
    discError = error;
    renderDiscovery();
  }

  function discoveredReasoning(modelId) {
    for (const host of disc?.hosts || []) for (const m of host.models) if (m.id === modelId && m.reasoning) return m.reasoning;
    return null;
  }

  const gb = (b) => (b ? `${(b / 1e9).toFixed(1)} GB` : "");
  const sameHost = (endpoint, host) => endpoint.replace(/^dns\+/, "").replace(/\/+$/, "").startsWith(host);
  const inPool = (host, id) => app.models.filter((m) => m.model === id && m.endpoints.some((e) => sameHost(e, host)));

  function renderDiscovery() {
    if (discError) { discBody.replaceChildren(h("div", { class: "banner error" }, `Discovery failed: ${discError}`)); return; }
    if (!disc) { discBody.replaceChildren(h("div", { class: "empty" }, "Discovering…")); return; }
    if (!disc.hosts.length) { discBody.replaceChildren(h("div", { class: "empty" }, "No backend hosts yet. Add one above.")); return; }
    discBody.replaceChildren(...disc.hosts.map(hostPanel));
    bench.renderPublished();
  }

  function hostPanel(host) {
    const removable = extraHosts.some((u) => u.replace(/\/+$/, "") === host.url);
    const head = h("div", { class: "host-head" },
      h("span", { class: "mono" }, host.url),
      host.kind && h("span", { class: "src" }, host.kind === "lmstudio" ? "LM Studio" : "OpenAI-compatible"),
      host.ok ? h("span", { class: "faint" }, `${host.models.length} models · ${host.models.filter((m) => m.loaded).length} loaded`) : null,
      h("span", { class: "grow" }),
      removable && h("button", { class: "link", onclick: () => { extraHosts = extraHosts.filter((u) => u.replace(/\/+$/, "") !== host.url); saveHosts(); discover(); } }, "remove host"));
    if (!host.ok) return h("section", { class: "panel host" }, head, h("div", { class: "banner error" }, host.error || "unreachable"));
    const lm = host.kind === "lmstudio";
    const models = [...host.models].sort((a, b) => (b.loaded - a.loaded) || (a.type === "embedding") - (b.type === "embedding") || a.id.localeCompare(b.id));
    const rows = models.map((m) => modelRow(host, m, lm));
    return h("section", { class: "panel host" }, head,
      lm && h("div", { class: "note" }, "Loading a model uses GPU memory; on a single GPU, unload others first. Reasoning “off” is sent as reasoning_effort: \"none\" (verified with LM Studio)."),
      h("div", { class: "disc-table" }, ...rows));
  }

  function modelRow(host, m, lm) {
    const key = `${host.url}|${m.id}`;
    const pooled = inPool(host.url, m.id);
    const isEmbed = m.type === "embedding";
    const status = m.loaded
      ? h("span", { class: "pill s-done" }, "loaded")
      : h("span", { class: "pill s-queued" }, "not loaded");
    const inst = (m.instances || []).map((i) => [
      i.context_length && `ctx ${i.context_length.toLocaleString()}`,
      i.parallel && `${i.parallel} parallel`,
      i.ttl_seconds != null && `unloads in ${fmtDur(i.ttl_seconds * 1000)}`,
    ].filter(Boolean).join(" · ")).join("; ");

    const ctxSel = h("select", { class: "ctx", title: "Context length to load with", "aria-label": "Context length" },
      ...[4096, 8192, 16384, 32768].map((n) => h("option", { value: n }, `${n / 1024}k ctx`)));
    ctxSel.value = "16384";
    const b = busy.get(key);
    const actions = h("div", { class: "row-actions" },
      lm && !m.loaded && !isEmbed && ctxSel,
      lm && !m.loaded && h("button", { class: "btn small", disabled: !!b, onclick: () => loadModel(host, m, Number(ctxSel.value)) }, b || "Load"),
      lm && m.loaded && h("button", { class: "btn small", disabled: !!b, onclick: () => unloadModel(host, m) }, b || "Unload"),
      pooled.length
        ? h("span", { class: "in-pool", title: "Already routable" }, `in pool as ${pooled.map((p) => p.name).join(", ")}`)
        : null,
      h("button", { class: "btn small primary", disabled: isEmbed, title: isEmbed ? "Embedding models can't serve chat completions" : "Make this model routable",
        onclick: () => toggleAdd(host, m, key) }, pooled.length ? "Add again" : "Add to pool"));

    const row = h("div", { class: `disc-row${m.loaded ? " loaded" : ""}` },
      h("div", { class: "disc-main" },
        h("div", {}, h("b", {}, m.display_name || m.id), isEmbed && h("span", { class: "src", style: "margin-left:6px" }, "embedding")),
        h("div", { class: "mono faint" }, m.id)),
      h("div", { class: "disc-meta" },
        [m.params, m.quant, gb(m.size_bytes), m.max_context && `max ctx ${(m.max_context / 1024).toFixed(0)}k`].filter(Boolean).join(" · "),
        m.reasoning && h("div", { class: "faint" }, `reasoning: ${m.reasoning.join(" / ")}`)),
      h("div", { class: "disc-state" }, status, inst && h("div", { class: "faint" }, inst)),
      actions);
    const wrap = h("div", {}, row);
    if (openAdd.has(key)) wrap.append(openAdd.get(key));
    return wrap;
  }

  function slug(id) {
    let base = id.split("/").pop().toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 48) || "model";
    let name = base, i = 2;
    while (app.models.some((m) => m.name === name)) name = `${base}-${i++}`;
    return name;
  }

  function toggleAdd(host, m, key) {
    if (openAdd.has(key)) { openAdd.delete(key); renderDiscovery(); return; }
    const inst = (m.instances || [])[0];
    const parallel = inst?.parallel;
    const workers = Math.max(app.workers.length, 1);
    const perWorker = parallel ? Math.max(1, Math.ceil(parallel / workers)) : 4;
    const form = modelForm({
      mode: "add", name: slug(m.id), model: m.id, endpoints: [host.url], initial: perWorker, max: perWorker,
      context: inst?.context_length || 0, slots: parallel || 0, shared: host.kind === "lmstudio",
      reasoningOptions: m.reasoning,
      onCancel: () => { openAdd.delete(key); renderDiscovery(); },
      onSubmit: async (body) => {
        await api("POST", "/v1/models", body);
        toast(`Added ${body.name} to the pool`);
        openAdd.delete(key);
        await refreshModels();
        renderDiscovery();
      },
    });
    openAdd.set(key, form);
    renderDiscovery();
    form.querySelector("input")?.focus();
  }

  async function loadModel(host, m, ctx) {
    const key = `${host.url}|${m.id}`;
    busy.set(key, "Loading…");
    renderDiscovery();
    try {
      const r = await api("POST", "/v1/discovery/load", { url: host.url, model: m.id, context_length: ctx });
      toast(`Loaded ${m.id} in ${r.load_time_seconds?.toFixed(1)}s`);
    } catch (e) {
      toast(e.message, true);
    } finally {
      busy.delete(key);
      await discover();
    }
  }

  async function unloadModel(host, m) {
    const key = `${host.url}|${m.id}`;
    const inst = (m.instances || [])[0];
    if (!inst || !confirm(`Unload ${m.id}? Runs that use it will make the server load it again on demand.`)) return;
    busy.set(key, "Unloading…");
    renderDiscovery();
    try {
      await api("POST", "/v1/discovery/unload", { url: host.url, instance_id: inst.id });
      toast(`Unloaded ${m.id}`);
    } catch (e) {
      toast(e.message, true);
    } finally {
      busy.delete(key);
      await discover();
    }
  }

  const poll = setInterval(async () => { await refreshModels(); }, 1500);
  if (!app.settings) loadSettings().then(renderPool).catch(() => {});
  renderPool();
  renderDiscovery();
  discover();
  return { onModels: () => { renderPool(); bench.onModels(); }, destroy() { clearInterval(poll); bench.destroy(); } };
}

// benchSection is the Benchmarking part of the Models page: published
// scores for candidate models (with what the backends have installed), and
// replays of a chat against pool models, scored by a judge model.
function benchSection(getDisc) {
  const pubBody = h("div", { class: "bench-pub" });
  const form = h("div", { class: "panel bench-form" });
  const listEl = h("div", { class: "bench-list" });
  const detailEl = h("div", { class: "bench-detail" });
  const el = h("div", {},
    h("h3", { class: "section-title", style: "margin-top:28px" }, "Benchmarking", h("span", { class: "hint" }, "published scores, and replays of your own chats")),
    pubBody,
    h("h3", { class: "section-title", style: "margin-top:20px" }, "Replay a chat", h("span", { class: "hint" }, "every model answers the same messages from the same sources; a judge model scores them")),
    form, listEl, detailEl);

  // ---- published scores ----
  let pub = null;
  fetch("benchmarks.json", { cache: "no-cache" }).then((r) => r.json()).then((d) => { pub = d; renderPublished(); }).catch(() => {});
  function installed(m) {
    const out = [];
    for (const host of getDisc()?.hosts || []) for (const x of host.models) {
      if (m.match.some((k) => x.id.toLowerCase().includes(k))) out.push(`${x.loaded ? "loaded" : "on disk"}${x.quant ? ` (${x.quant})` : ""}`);
    }
    return out;
  }
  function renderPublished() {
    if (!pub) { pubBody.replaceChildren(h("div", { class: "empty" }, "Loading published scores…")); return; }
    const head = h("tr", {}, h("th", {}, "Model"), h("th", {}, "Size"), h("th", {}, "Fits a 16 GB GPU as"),
      ...pub.benchmarks.map((b) => h("th", { title: b.about, class: "num" }, b.name)), h("th", {}, "On your hosts"), h("th", {}, "Get"));
    const rows = pub.models.map((m) => {
      const have = installed(m);
      return h("tr", {},
        h("td", {}, h("b", {}, m.name), h("div", { class: "faint" }, m.note)),
        h("td", {}, m.params, h("div", { class: "faint" }, `${m.context} ctx · ${m.license}`)),
        h("td", {}, m.fits),
        ...pub.benchmarks.map((b) => h("td", { class: "num" }, m.scores[b.key] != null ? String(m.scores[b.key]) : "–")),
        h("td", {}, have.length ? have.join(", ") : h("span", { class: "faint" }, "not installed")),
        h("td", {}, m.lmstudio && h("a", { href: m.lmstudio, target: "_blank", rel: "noopener" }, "LM Studio"), m.lmstudio && m.gguf && " · ",
          m.gguf && h("a", { href: m.gguf, target: "_blank", rel: "noopener" }, "GGUF"),
          h("div", {}, ...m.sources.map((u, i) => h("a", { href: u, target: "_blank", rel: "noopener", class: "faint", style: "margin-right:6px" }, `source ${i + 1}`)))));
    });
    pubBody.replaceChildren(h("details", { class: "panel bench-pub-panel" },
      h("summary", {}, `Published scores (${pub.models.length} models, collected ${pub.retrieved})`),
      h("p", { class: "note" }, `${pub.source_note} ${pub.gpu_note}`),
      h("div", { class: "table-scroll" }, h("table", { class: "bench-table" }, h("thead", {}, head), h("tbody", {}, ...rows)))));
  }

  // ---- replay form ----
  const srcSel = h("select", { "aria-label": "Chat to replay", onchange: () => showSource() });
  const refreshBox = h("input", { type: "checkbox", "aria-label": "Collect fresh sources" });
  const suiteInfo = h("details", { class: "bench-suite" },
    h("summary", {}, "The 8 test messages"), h("ol", {}),
    h("label", { class: "check", title: "Answer each test message again with search on Auto to collect new sources. Off: reuse the sources from your last test-set benchmark, so results compare directly." }, refreshBox, " Collect fresh sources (otherwise reuse the last set, so results compare over time)"));
  function showSource() {
    msg.hidden = srcSel.value !== "new";
    suiteInfo.hidden = srcSel.value !== "suite";
  }
  const msg = h("textarea", { rows: 3, placeholder: "A new message to benchmark with (it is sent as a chat first, with search on Auto, to collect its sources)", "aria-label": "New message" });
  const modelsBox = h("div", { class: "bench-models" });
  const judgeSel = h("select", { "aria-label": "Judge model" });
  const parSel = h("select", { "aria-label": "Parallel streams" }, ...[1, 2, 4, 8].map((n) => h("option", { value: n }, `${n}`)));
  parSel.value = "4";
  const swapBox = h("input", { type: "checkbox", checked: true, "aria-label": "Swap models on the GPU" });
  const startBtn = h("button", { class: "btn primary", onclick: () => start() }, "Start benchmark");
  form.append(
    h("div", { class: "bench-grid" },
      h("label", { class: "field" }, "Chat", srcSel),
      h("label", { class: "field" }, "Judge model", judgeSel),
      h("label", { class: "field", title: "Answers run at the same time per model; cases repeat to fill the streams. Speed numbers are measured at this load." }, "Parallel streams", parSel)),
    msg,
    suiteInfo,
    h("div", { class: "field-label" }, "Models to compare (up to 4)"),
    modelsBox,
    h("label", { class: "check", title: "On LM Studio hosts, load each model before its turn (unloading the others), then the judge, then put back what was loaded. Chat and runs on these hosts slow down or wait meanwhile." },
      swapBox, " Swap models on the GPU automatically (other work on these hosts waits while models load)"),
    h("div", { class: "form-actions" }, startBtn));

  let chats = [];
  async function loadChats() {
    try { chats = (await api("GET", "/v1/chats")).chats; } catch { chats = []; }
    const keep = srcSel.value;
    srcSel.replaceChildren(h("option", { value: "suite" }, "Standard test set (8 messages)"), h("option", { value: "new" }, "New message…"),
      ...chats.map((c) => h("option", { value: c.id }, c.title || "Untitled chat")));
    srcSel.value = [...srcSel.options].some((o) => o.value === keep) ? keep : "suite";
    showSource();
  }
  function onModels() {
    const s = app.settings?.settings;
    const names = app.models.map((m) => m.name);
    const key = names.join(",");
    if (modelsBox.dataset.key === key) return;
    modelsBox.dataset.key = key;
    const checked = new Set([...modelsBox.querySelectorAll("input:checked")].map((i) => i.value));
    if (!checked.size && s) checked.add(s.worker.model);
    modelsBox.replaceChildren(...app.models.map((m) => h("label", { class: "check" },
      h("input", { type: "checkbox", value: m.name, checked: checked.has(m.name) }), ` ${m.name}`, h("span", { class: "faint mono" }, ` ${m.model}`))));
    const j = judgeSel.value || s?.reducer.model;
    judgeSel.replaceChildren(...names.map((n) => h("option", { value: n }, n)));
    if (j && names.includes(j)) judgeSel.value = j;
  }

  async function start() {
    const models = [...modelsBox.querySelectorAll("input:checked")].map((i) => i.value);
    if (!models.length) { toast("Choose at least one model", true); return; }
    if (models.length > 4) { toast("Choose up to 4 models", true); return; }
    const body = { models, judge: judgeSel.value, parallel: Number(parSel.value), swap: swapBox.checked };
    if (srcSel.value === "suite") {
      body.suite = true;
      body.refresh = refreshBox.checked;
    } else if (srcSel.value === "new") {
      body.message = msg.value.trim();
      if (!body.message) { toast("Write a message, or choose a chat", true); return; }
    } else body.chat_id = srcSel.value;
    startBtn.disabled = true;
    try {
      const r = await api("POST", "/v1/benchmarks", body);
      toast("Benchmark started");
      msg.value = "";
      selected = r.id;
      await loadList();
      loadChats();
    } catch (e) { toast(e.message, true); }
    finally { startBtn.disabled = false; }
  }

  // ---- results ----
  let list = [];
  let selected = null;
  let detail = null;
  const ACTIVE = new Set(["running", "cancelling"]);
  const pillClass = { running: "s-running", cancelling: "s-waiting", done: "s-done", failed: "s-failed", cancelled: "s-cancelled", interrupted: "s-failed" };

  async function loadList() {
    try {
      const d = await api("GET", "/v1/benchmarks");
      list = d.benchmarks;
      if (d.suite) suiteInfo.querySelector("ol").replaceChildren(...d.suite.map((m) => h("li", {}, m)));
    } catch { list = []; }
    if (!selected && list.length) selected = list[0].id;
    renderList();
    if (selected) await loadDetail();
  }
  function renderList() {
    if (!list.length) { listEl.replaceChildren(); return; }
    listEl.replaceChildren(h("div", { class: "bench-runs" }, ...list.map((b) => {
      const cfg = b.config || {};
      return h("button", { class: `bench-run${b.id === selected ? " selected" : ""}`, onclick: () => { selected = b.id; renderList(); loadDetail(); } },
        h("span", { class: `pill ${pillClass[b.status] || ""}${ACTIVE.has(b.status) ? " live" : ""}` }, b.status),
        h("span", {}, (cfg.models || []).join(" vs ")),
        h("span", { class: "faint" }, cfg.suite ? "test set" : "chat"),
        h("span", { class: "faint" }, fmtAgo(new Date(b.created_at).getTime())));
    })));
  }
  async function loadDetail() {
    try { detail = await api("GET", `/v1/benchmarks/${selected}`); } catch (e) { detail = null; detailEl.replaceChildren(h("div", { class: "banner error" }, e.message)); return; }
    renderDetail();
  }
  const ms = (v) => (v ? fmtDur(v) : "–");
  const num = (v, d = 1) => (v ? v.toFixed(d) : "–");
  function renderDetail() {
    const b = detail;
    if (!b) { detailEl.replaceChildren(); return; }
    const r = b.results || {};
    const active = ACTIVE.has(b.status);
    const head = h("div", { class: "bench-head" },
      h("span", { class: `pill ${pillClass[b.status] || ""}${active ? " live" : ""}` }, b.status),
      h("span", {}, active ? r.phase || "" : `judged by ${r.judge}`),
      r.chat_id && h("a", { href: `#/chat/${r.chat_id}` }, "Open the chat"),
      h("span", { class: "grow" }),
      h("button", { class: "btn small danger", onclick: () => removeBench(b, active) }, active ? "Cancel" : "Delete"));
    const models = r.models || [];
    const sumHead = h("tr", {}, h("th", {}, "Model"), h("th", {}, "Status"),
      h("th", { class: "num", title: "Answers that finished with text" }, "Answered"),
      h("th", { class: "num", title: "Answers that declined or withheld the help asked for" }, "Refusals"),
      h("th", { class: "num", title: "Fees, dates, limits or legal claims not in the sources (or stated as certain without sources)" }, "Invented specifics"),
      h("th", { class: "num", title: "Clearly wrong general knowledge" }, "Knowledge slips"),
      h("th", { class: "num", title: "Median tokens per second of one answer" }, "tok/s"),
      h("th", { class: "num", title: "All tokens of the concurrent batch divided by its wall time" }, "Total tok/s"),
      h("th", { class: "num", title: "Median time to first token" }, "TTFT"),
      h("th", { class: "num", title: "Wall time of the concurrent batch" }, "Batch time"));
    const lowest = (key) => { const v = models.filter((m) => m.judged).map((m) => m[key]); return v.length > 1 ? Math.min(...v) : null; };
    const best = { refusals: lowest("refusals"), unsupported: lowest("unsupported"), slips: lowest("slips") };
    const cell = (m, key) => h("td", { class: `num${best[key] != null && m[key] === best[key] ? " best" : ""}` }, m.judged ? String(m[key]) : "–");
    const sumRows = models.map((m) => h("tr", {},
      h("td", {}, h("b", {}, m.name), h("div", { class: "faint mono" }, m.model)),
      h("td", {}, m.status, m.error && h("div", { class: "faint" }, m.error)),
      h("td", { class: "num" }, `${m.answered || 0}/${(r.cases || []).length}`),
      cell(m, "refusals"), cell(m, "unsupported"), cell(m, "slips"),
      h("td", { class: "num" }, num(m.median_tps)),
      h("td", { class: `num${m.ran_at_once && m.ran_at_once < m.parallel ? " warn" : ""}`,
        title: m.ran_at_once && m.ran_at_once < m.parallel ? `Only ${m.ran_at_once} answer(s) ran at a time: the backend has fewer parallel slots than ${m.parallel}. Compare tok/s per answer instead.` : "" },
        m.aggregate_tps ? `${num(m.aggregate_tps)} @${m.parallel}` : "–",
        m.ran_at_once && m.ran_at_once < m.parallel ? h("div", { class: "faint" }, `ran ${m.ran_at_once} at a time`) : null),
      h("td", { class: "num" }, ms(m.median_ttft_ms)),
      h("td", { class: "num" }, ms(m.wall_ms))));
    const cases = (r.cases || []).map((c, ci) => h("section", { class: "panel bench-case" },
      h("div", { class: "bench-q" }, h("span", { class: "faint" }, `Message ${ci + 1}${c.sources ? " · with sources" : " · no sources"}`), h("div", {}, c.prompt.length > 300 ? c.prompt.slice(0, 300) + "…" : c.prompt)),
      ...models.map((m) => {
        const a = (m.answers || []).find((x) => x.case === ci);
        if (!a) return h("div", { class: "faint bench-a" }, `${m.name}: waiting…`);
        const tags = [];
        if (a.refused != null) tags.push(a.refused ? "refused" : "helped");
        if (a.unsupported) tags.push(`${a.unsupported.length} invented`);
        if (a.slips) tags.push(`${a.slips.length} slips`);
        if (a.judge_error) tags.push(a.judge_error);
        if (a.judge_dropped) tags.push(`${a.judge_dropped} judge finding(s) dropped: their figures are in the sources`);
        if (a.tokens_per_second) tags.push(`${a.tokens_per_second.toFixed(1)} tok/s`);
        return h("details", { class: "bench-a" },
          h("summary", {}, h("b", {}, m.name), " ", h("span", { class: `faint${a.refused ? " bad" : ""}` }, a.status !== "done" ? `${a.status}${a.error ? ": " + a.error : ""}` : tags.join(" · "))),
          a.refused && h("div", { class: "finding bad" }, "Judged a refusal"),
          ...(a.unsupported || []).map((x) => h("div", { class: "finding" }, "Invented: ", x)),
          ...(a.slips || []).map((x) => h("div", { class: "finding" }, "Slip: ", x)),
          a.output && h("div", { class: "out md bench-out" }, renderMarkdown(a.output)),
          h("div", { class: "bench-links" },
            a.tree_id && h("a", { href: `#/runs/${a.tree_id}`, class: "faint" }, "View run"),
            a.judge_tree && a.judge_tree !== "00000000-0000-0000-0000-000000000000" && h("a", { href: `#/runs/${a.judge_tree}`, class: "faint" }, "View judge run")));
      })));
    detailEl.replaceChildren(h("section", { class: "panel bench-summary" }, head,
      b.error && h("div", { class: "banner error" }, b.error),
      ...(r.notes || []).map((n) => h("div", { class: "banner warn" }, n)),
      h("div", { class: "table-scroll" }, h("table", { class: "bench-table" }, h("thead", {}, sumHead), h("tbody", {}, ...sumRows))),
      h("p", { class: "note" }, `Scores come from the judge model (${r.judge || "?"}) and are only as good as it is: open an answer to check its findings. Speed is measured with ${models[0]?.parallel || "?"} answers at a time.`)),
      ...cases);
  }
  async function removeBench(b, active) {
    if (!active && !confirm("Delete this benchmark and the runs it made?")) return;
    try {
      await api("DELETE", `/v1/benchmarks/${b.id}`);
      toast(active ? "Cancelling…" : "Benchmark deleted");
      if (!active) selected = null;
      await loadList();
    } catch (e) { toast(e.message, true); }
  }

  const poll = setInterval(() => { if (detail && ACTIVE.has(detail.status)) loadList(); }, 3000);
  loadChats();
  onModels();
  loadList();
  return { el, renderPublished, onModels, destroy() { clearInterval(poll); } };
}

// Shared add/edit form for a pool model. Reasoning maps to LM Studio's
// reasoning_effort ("off" is sent as "none"; "on" is the model default).
function modelForm({ mode, name, model, endpoints, initial, max, context, slots, shared, extra, reasoningOptions, onSubmit, onCancel }) {
  const extraObj = { ...(extra || {}) };
  const nameIn = h("input", { type: "text", value: name, disabled: mode === "edit", "aria-label": "Routing name" });
  const modelIn = h("input", { type: "text", value: model, class: "mono", "aria-label": "API model id" });
  const epsIn = h("input", { type: "text", value: endpoints.join(", "), class: "mono", "aria-label": "Endpoints" });
  const initIn = h("input", { type: "number", min: 1, max: 64, value: initial || 2, "aria-label": "Initial limit" });
  const maxIn = h("input", { type: "number", min: 1, max: 64, value: max || 4, "aria-label": "Max limit" });
  const ctxIn = h("input", { type: "number", min: 0, step: 1024, value: context || 0, "aria-label": "Context length" });
  const slotsIn = h("input", { type: "number", min: 0, max: 256, value: slots || 0, "aria-label": "Server slots" });
  const sharedIn = h("input", { type: "checkbox", checked: !!shared, "aria-label": "Context shared by all slots" });
  const current = extraObj.reasoning_effort === "none" ? "off" : extraObj.reasoning_effort || "";
  const opts = [...new Set([...(reasoningOptions || []), ...(current ? [current] : [])])].filter((o) => o !== "on");
  const reasonSel = h("select", { "aria-label": "Reasoning" },
    h("option", { value: "" }, "model default"),
    ...opts.map((o) => h("option", { value: o }, o === "off" ? "off (fast, no thinking)" : o)));
  reasonSel.value = current;
  delete extraObj.reasoning_effort;
  const extraIn = h("textarea", { class: "mono", rows: 2, placeholder: "{}", value: Object.keys(extraObj).length ? JSON.stringify(extraObj) : "", "aria-label": "Extra request params (JSON)" });
  const err = h("div", { class: "banner error", style: "display:none;margin:0" });
  const save = h("button", { class: "btn primary small" }, mode === "add" ? "Add to pool" : "Save");

  save.addEventListener("click", async () => {
    let extraBody;
    try { extraBody = extraIn.value.trim() ? JSON.parse(extraIn.value) : {}; }
    catch { err.textContent = "Extra params must be valid JSON."; err.style.display = ""; return; }
    if (reasonSel.value) extraBody.reasoning_effort = reasonSel.value === "off" ? "none" : reasonSel.value;
    const body = {
      name: nameIn.value.trim(), model: modelIn.value.trim(),
      endpoints: epsIn.value.split(/[\s,]+/).filter(Boolean),
      initial_limit: Number(initIn.value) || 0, max_limit: Number(maxIn.value) || 0,
      context_length: Number(ctxIn.value) || 0, slots: Number(slotsIn.value) || 0, shared_context: sharedIn.checked,
      extra: Object.keys(extraBody).length ? extraBody : null,
    };
    save.disabled = true;
    err.style.display = "none";
    try { await onSubmit(body); }
    catch (e) { err.textContent = e.message; err.style.display = ""; }
    finally { save.disabled = false; }
  });

  return h("div", { class: "model-form" },
    h("div", { class: "grid3" },
      h("label", { class: "field" }, "Routing name", nameIn),
      h("label", { class: "field" }, "API model id", modelIn),
      h("label", { class: "field" }, "Reasoning", reasonSel)),
    h("div", { class: "grid3" },
      h("label", { class: "field", style: "grid-column: span 1" }, "Endpoints (comma-separated)", epsIn),
      h("label", { class: "field" }, "Start limit", initIn),
      h("label", { class: "field" }, "Max limit (≈ server parallel slots)", maxIn)),
    h("div", { class: "grid3" },
      h("label", { class: "field", title: "Context the server loaded the model with. Merges are batched to fit it; 0 = unknown (assume 8k)." }, "Context length (tokens)", ctxIn),
      h("label", { class: "field", title: "Server parallel slots shared by all workers; 0 = no shared cap." }, "Server slots (all workers)", slotsIn),
      h("label", { class: "field", title: "LM Studio and llama.cpp share one context (KV cache) across all parallel requests. When set, requests are admitted only while their estimated tokens fit, across all workers." },
        "Context shared by all slots", h("span", { style: "display:flex;gap:6px;align-items:center;color:var(--text);font-weight:400" }, sharedIn, "budget requests by tokens"))),
    h("div", { class: "note" }, "Max limit is per worker; server slots and the shared context cap totals across all workers."),
    h("label", { class: "field" }, "Extra request params (JSON, optional)", extraIn),
    err,
    h("div", { class: "form-actions" }, h("button", { class: "btn small", onclick: onCancel }, "Cancel"), save));
}

// ---------- authentication ----------
// The API accepts a bearer token; the browser trades one for an HttpOnly
// session cookie (EventSource cannot send headers). After signing in, the
// page reloads so every live stream reconnects with the session.
let loginShown = false;
function showLogin(message) {
  if (loginShown) return;
  loginShown = true;
  // Email and password by default; an access token remains available.
  let useToken = false;
  const email = h("input", { type: "email", autocomplete: "username", placeholder: "Email", "aria-label": "Email" });
  const password = h("input", { type: "password", autocomplete: "current-password", placeholder: "Password", "aria-label": "Password" });
  const token = h("input", { type: "password", autocomplete: "off", placeholder: "Access token", "aria-label": "Access token", hidden: true });
  const hint = h("p", { class: "muted" }, "Sign in with your account.");
  const err = h("div", { class: "login-error", role: "alert" }, message || "");
  const go = h("button", { class: "btn primary", type: "submit" }, "Sign in");
  const toggle = h("button", { class: "link", type: "button", onclick: () => {
    useToken = !useToken;
    email.hidden = password.hidden = useToken;
    token.hidden = !useToken;
    setText(toggle, useToken ? "Sign in with email and password" : "Use an access token instead");
    hint.replaceChildren(...(useToken
      ? ["Enter an access token (for administrators; see the cluster secret ", h("code", {}, "spawner-auth"), ")."]
      : ["Sign in with your account."]));
    setText(err, "");
    (useToken ? token : email).focus();
  } }, "Use an access token instead");
  const form = h("form", { class: "panel login-box", onsubmit: async (e) => {
    e.preventDefault();
    go.disabled = true;
    setText(err, "");
    const body = useToken ? { token: token.value.trim() } : { email: email.value.trim(), password: password.value };
    try {
      const res = await fetch("/v1/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
      if (res.ok) { location.reload(); return; }
      const msg = (await res.json().catch(() => ({}))).error;
      setText(err, res.status === 401 ? (useToken ? "That token isn't valid." : "Wrong email or password.")
        : res.status === 429 ? (msg || "Too many attempts. Try again later.") : `Sign-in failed (${res.status}).`);
    } catch { setText(err, "Can't reach the server."); }
    go.disabled = false;
    (useToken ? token : password).select();
  } },
    h("h1", {}, "Sign in to Agent Spawner"),
    hint, email, password, token, err, go, toggle);
  document.body.append(h("div", { class: "login-overlay" }, form));
  email.focus();
}

// showChangePassword lets an account holder change their password. Other
// sessions of the account are signed out; this one continues.
function showChangePassword() {
  const current = h("input", { type: "password", autocomplete: "current-password", placeholder: "Current password", "aria-label": "Current password" });
  const next = h("input", { type: "password", autocomplete: "new-password", placeholder: "New password (12+ characters)", "aria-label": "New password", minlength: 12 });
  const again = h("input", { type: "password", autocomplete: "new-password", placeholder: "Repeat new password", "aria-label": "Repeat new password" });
  const err = h("div", { class: "login-error", role: "alert" });
  const go = h("button", { class: "btn primary", type: "submit" }, "Change password");
  const overlay = h("div", { class: "login-overlay" });
  const close = () => overlay.remove();
  const form = h("form", { class: "panel login-box", onsubmit: async (e) => {
    e.preventDefault();
    if (next.value !== again.value) { setText(err, "The new passwords don't match."); return; }
    go.disabled = true;
    setText(err, "");
    try {
      const res = await fetch("/v1/account/password", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ current: current.value, new: next.value }) });
      if (res.ok) { close(); toast("Password changed. Other sessions were signed out."); return; }
      setText(err, (await res.json().catch(() => ({}))).error || `Failed (${res.status}).`);
    } catch { setText(err, "Can't reach the server."); }
    go.disabled = false;
  } },
    h("h1", {}, "Change password"),
    h("p", { class: "muted" }, "Other sessions of this account will be signed out."),
    current, next, again, err, go,
    h("button", { class: "link", type: "button", onclick: close }, "Cancel"));
  overlay.append(form);
  overlay.addEventListener("click", (e) => { if (e.target === overlay) close(); });
  document.body.append(overlay);
  current.focus();
}

async function checkSessionQuiet() {
  try {
    const s = await (await fetch("/v1/session")).json();
    return !s.auth || s.authenticated;
  } catch {
    return true;
  }
}

async function checkSession() {
  try {
    const s = await (await fetch("/v1/session")).json();
    if (s.auth && s.authenticated) {
      const out = h("button", { class: "icon-btn signout", title: "Sign out", "aria-label": "Sign out",
        onclick: async () => { await fetch("/v1/logout", { method: "POST" }); location.reload(); } }, "⎋");
      if (s.user) {
        $("#theme-toggle").before(h("button", { class: "account", title: "Change password", onclick: showChangePassword }, s.user));
      }
      $("#theme-toggle").before(out);
    }
    return !s.auth || s.authenticated;
  } catch {
    return true; // server unreachable: let the normal error paths report it
  }
}

// ---------- boot ----------
checkSession().then((ok) => {
  if (!ok) { showLogin(); return; }
  window.addEventListener("hashchange", route);
  setInterval(refreshModels, 5000);
  connectLifecycle();
  Promise.allSettled([refreshTrees(), refreshModels(), loadSettings()]).then(() => {
    route();
    app.view?.onModels?.();
  });
});
