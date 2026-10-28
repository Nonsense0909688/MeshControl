"use strict";
/* MeshControl shared code: page shell, API access, polling, formatting and small
   DOM helpers. All dynamic text is inserted with textContent, never as HTML. */
const $ = s => document.querySelector(s);
const el = (t, c, x) => { const e = document.createElement(t); if (c) e.className = c; if (x != null) e.textContent = x; return e };
const add = (p, ...k) => { k.forEach(x => x != null && p.append(x)); return p };
const clear = n => { while (n.firstChild) n.removeChild(n.firstChild) };
const T = s => document.createTextNode(s);
const enc = encodeURIComponent, nodeUrl = n => "/nodes/" + enc(n), modelUrl = m => "/models/" + enc(m);
const A = (h, t, c) => { const a = el("a", c, t); a.href = h; return a };
const store = {
  get: k => { try { return sessionStorage.getItem(k) } catch (e) { return null } },
  set: (k, v) => { try { sessionStorage.setItem(k, v) } catch (e) {} },
};
const lstore = {
  get: k => { try { return localStorage.getItem(k) } catch (e) { return null } },
  set: (k, v) => { try { localStorage.setItem(k, v) } catch (e) {} },
};

/* ---------- state ---------- */
const MC = {
  nodes: [], models: [], pulls: [], ok: false, loaded: false, q: "", render: null,
  token: store.get("mc_token") || "",
  hist: [], nodeHist: {}, events: [], prev: null,
};
try { Object.assign(MC, JSON.parse(store.get("mc_state") || "{}")) } catch (e) {}
if (MC.prev && !Array.isArray(MC.prev)) MC.prev = null;
const hdr = x => Object.assign(MC.token ? { Authorization: "Bearer " + MC.token } : {}, x || {});

async function api(path, opts) {
  opts = opts || {};
  const r = await fetch(path, Object.assign({}, opts, { headers: hdr(opts.headers) }));
  if (r.status === 401) { const d = $("#tokd"); if (d && !d.open) d.showModal(); throw new Error("Unauthorized: enter the admin token") }
  if (!r.ok) {
    let m = null; try { m = (await r.json()).error } catch (e) {}
    const e = new Error(m || "HTTP " + r.status); e.status = r.status; throw e;
  }
  return r.json();
}

/* ---------- node / model status ---------- */
const STATES = {
  online: { cls: "ok", dot: "on-d", label: "Online" },
  provider_unreachable: { cls: "orange", dot: "or-d", label: "Provider unreachable" },
  offline: { cls: "bad", dot: "off-d", label: "Offline" },
};
const nodeState = n => n.status || (!n.online ? "offline" : n.ollama_lan_reachable ? "online" : "provider_unreachable");
const pill = (cls, dot, text) => add(el("span", "pill " + cls), el("i", "dot " + dot), T(text));
const statePill = s => { const d = STATES[s] || STATES.offline; return pill(d.cls, d.dot, d.label) };
const nodePill = n => statePill(nodeState(n));
const getNode = name => MC.nodes.find(n => n.node === name);

function modelStatePill(g) {
  if (g.ok) return pill("ok", "on-d", "Available");
  const hosts = g.nodes.map(getNode).filter(Boolean);
  return hosts.some(n => nodeState(n) === "provider_unreachable") ? statePill("provider_unreachable") : statePill("offline");
}

/* One entry per distinct model name, with the facts reported by its hosts. */
function modelGroups() {
  const m = new Map();
  for (const e of MC.models) {
    const g = m.get(e.model) || { name: e.model, copies: [], nodes: [], ok: 0, size: null, modified_at: null, family: null, parameter_size: null, quantization_level: null, format: null, digest: null };
    g.copies.push(e); g.nodes.push(e.node); if (e.available) g.ok++;
    for (const k of ["size", "family", "parameter_size", "quantization_level", "format", "digest"]) if (g[k] == null && e[k] != null) g[k] = e[k];
    if (e.modified_at != null && (g.modified_at == null || e.modified_at > g.modified_at)) g.modified_at = e.modified_at;
    m.set(e.model, g);
  }
  return [...m.values()].sort((a, b) => a.name.localeCompare(b.name));
}

/* ---------- formatting ---------- */
const pct = v => v == null ? "–" : Math.round(v) + "%";
const bytes = n => n == null ? "–" : n < 1e3 ? n + " B" : n < 1e6 ? Math.round(n / 1e3) + " KB" : n < 1e9 ? Math.round(n / 1e6) + " MB" : (n / 1e9).toFixed(n < 1e11 ? 1 : 0) + " GB";
const ago = t => {
  const s = Math.max(0, Date.now() / 1000 - t);
  return s < 60 ? Math.floor(s) + "s ago" : s < 3600 ? Math.floor(s / 60) + "m ago" : s < 86400 ? Math.floor(s / 3600) + "h ago"
    : s < 2592000 ? Math.floor(s / 86400) + "d ago" : s < 31536000 ? Math.floor(s / 2592000) + "mo ago" : Math.floor(s / 31536000) + "y ago";
};
const dateFmt = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });
const absDate = t => dateFmt.format(new Date(t * 1000));
const dateCell = t => t == null ? T("–") : add(el("span"), T(absDate(t)), el("small", "", ago(t)));
const match = s => !MC.q || s.toLowerCase().includes(MC.q);
const shortDigest = d => d ? d.replace(/^sha256:/, "").slice(0, 12) : "–";
const sumSize = list => list.reduce((a, m) => a + (m.size || 0), 0);
const nodeModels = n => n.models.map(name => Object.assign({ name }, n.model_details && n.model_details[name] || {}));

/* ---------- DOM components ---------- */
const stat = (i, bg, v, l, s) => { const c = el("div", "card stat"), ic = el("div", "ico", i); ic.style.background = bg; return add(c, ic, add(el("div"), el("b", "", v), el("span", "", l), s ? el("span", "", s) : null)) };
const grid = (c, ...k) => add(el("div", "grid " + c), ...k);
const card = (title, right, body) => { const h = el("h2", "", title); if (right) h.append(right); return add(el("section", "card"), h, body) };
const dotted = (cls, text) => add(el("span"), el("i", "dot " + cls), T(text));
const legend = (...p) => { const s = el("span", "legend"); p.forEach(([c, t]) => { const i = el("i", "dot " + (c.startsWith("--") ? "" : c)); if (c.startsWith("--")) i.style.background = "var(" + c + ")"; s.append(add(el("span"), i, T(t))) }); return s };
function tbl(heads, rows, empty) {
  const w = el("div", "tw"), t = el("table"), h = el("tr"), b = el("tbody");
  heads.forEach(x => h.append(add(el("th"), x instanceof Node ? x : T(x))));
  if (!rows.length) { const c = el("td", "empty", empty || "Nothing here yet"); c.colSpan = heads.length; b.append(add(el("tr"), c)) }
  rows.forEach(r => { const tr = el("tr"); r.forEach(c => tr.append(add(el("td"), c instanceof Node ? c : T(String(c))))); b.append(tr) });
  return add(w, add(t, add(el("thead"), h), b));
}
function hd(title, sub, actions, badge) {
  const h1 = el("h1", "", title); if (badge) h1.append(badge);
  const l = add(el("div"), h1, sub ? el("p", "", sub) : null), d = el("div", "acts"); (actions || []).forEach(a => d.append(a));
  return add(el("div", "hd"), l, d);
}
const crumb = (href, label, cur) => add(el("div", "crumb"), A(href, label), T(" / " + cur));
const kv = pairs => { const d = el("dl", "kv"); pairs.forEach(([k, v]) => d.append(el("dt", "", k), add(el("dd"), v instanceof Node ? v : T(String(v))))); return d };
const banner = (cls, icon, title, text) => add(el("div", "banner " + cls), el("i", "", icon), add(el("div"), el("b", "", title), el("span", "", text)));
const btn = (label, cls, onclick) => { const b = el("button", cls || "", label); b.type = "button"; if (onclick) b.onclick = onclick; return b };
const notFound = (what, name, href, back) => { clear(MC.view); add(MC.view, card(what + " not found", null, add(el("p", "empty"), T("“" + name + "” is not registered (it may have been purged after a long silence). "), A(href, "Back to " + back)))) };
function lineChart(series) {
  const W = 600, H = 170, P = 28, NS = "http://www.w3.org/2000/svg", svg = document.createElementNS(NS, "svg");
  svg.setAttribute("viewBox", "0 0 " + W + " " + H);
  const mk = (t, a, x) => { const e = document.createElementNS(NS, t); for (const k in a) (k === "stroke" || k === "fill") ? e.style[k] = a[k] : e.setAttribute(k, a[k]); if (x != null) e.textContent = x; svg.append(e) };
  for (const v of [0, 25, 50, 75, 100]) { const y = H - P / 2 - v / 100 * (H - P); mk("line", { x1: P, x2: W, y1: y, y2: y, stroke: "var(--line)" }); mk("text", { x: 0, y: y + 4, fill: "var(--mut)", "font-size": 10 }, v) }
  const n = Math.max(0, ...series.map(s => s.d.length));
  if (n < 2) mk("text", { x: P + 10, y: H / 2, fill: "var(--mut)", "font-size": 12 }, "Collecting samples (every 5 s)…");
  else series.forEach(s => mk("polyline", { points: s.d.map((v, i) => (P + i * (W - P) / (n - 1)).toFixed(1) + "," + (H - P / 2 - v / 100 * (H - P)).toFixed(1)).join(" "), fill: "none", stroke: s.c, "stroke-width": 2 }));
  return add(el("div", "chart"), svg);
}
const cpuRam = h => lineChart([{ d: h.map(x => x.cpu), c: "var(--blue)" }, { d: h.map(x => x.ram), c: "var(--warn)" }]);
const cpuRamLegend = () => legend(["--blue", "CPU %"], ["--warn", "RAM %"]);

/* ---------- polling ---------- */
function persist() { store.set("mc_state", JSON.stringify({ hist: MC.hist, nodeHist: MC.nodeHist, events: MC.events, prev: MC.prev })) }
function diff() {
  const cur = new Map(MC.nodes.map(n => [n.node, { s: nodeState(n), m: new Set(n.models) }]));
  const prev = MC.prev ? new Map(MC.prev.map(([k, v]) => [k, { s: v.s, m: new Set(v.m) }])) : null;
  if (prev) {
    const ev = (k, t, s) => { MC.events.unshift({ k, t, s, at: Date.now() / 1000 }); MC.events = MC.events.slice(0, 8) };
    for (const [name, c] of cur) {
      const p = prev.get(name);
      if (!p) { ev("on", "Node joined", name); continue }
      if (p.s !== c.s) {
        if (c.s === "offline") ev("off", "Node offline", name + " is not responding");
        else if (c.s === "provider_unreachable") ev("warn", "Provider unreachable", "Ollama on " + name + " cannot be reached");
        else ev("on", "Node online", name + " is online");
      } else if (c.s === "online") {
        for (const x of c.m) if (!p.m.has(x)) ev("mod", "Model added", x + " on " + name);
        for (const x of p.m) if (!c.m.has(x)) ev("rm", "Model removed", x + " from " + name);
      }
    }
    for (const name of prev.keys()) if (!cur.has(name)) ev("rm", "Node removed", name);
  }
  MC.prev = [...cur].map(([k, v]) => [k, { s: v.s, m: [...v.m] }]);
}
function sample() {
  const on = MC.nodes.filter(n => n.online && n.cpu != null), avg = k => on.length ? on.reduce((a, n) => a + n[k], 0) / on.length : 0;
  MC.hist.push({ cpu: avg("cpu"), ram: avg("ram") }); if (MC.hist.length > 60) MC.hist.shift();
  for (const n of MC.nodes) { const h = MC.nodeHist[n.node] = MC.nodeHist[n.node] || []; h.push({ cpu: n.online ? n.cpu || 0 : 0, ram: n.online ? n.ram || 0 : 0 }); if (h.length > 60) h.shift() }
}
async function loadPulls() {
  try { MC.pulls = (await api("/api/pulls")).pulls } catch (e) { if (e.message.startsWith("Unauthorized")) throw e }
  gwStatus();
}
async function refresh() {
  try {
    const [n, m] = await Promise.all([api("/api/nodes"), api("/api/models")]);
    MC.nodes = n.nodes; MC.models = m.models; MC.ok = true; MC.loaded = true; diff(); sample(); persist();
    await loadPulls();
  } catch (e) { MC.ok = false }
  gwStatus(); if (MC.render) MC.render();
}
function gwStatus() {
  const on = MC.nodes.filter(n => nodeState(n) === "online").length;
  $("#gwdot").className = "dot " + (MC.ok ? "on-d" : "off-d");
  $("#gwtxt").textContent = MC.ok ? "Controller online" : "Controller unreachable";
  $("#gwsub").textContent = MC.ok ? on + " of " + MC.nodes.length + " nodes online" : "Retrying…";
  const run = MC.pulls.filter(p => p.state === "running"), d = $("#dl");
  d.classList.toggle("show", run.length > 0);
  if (run.length) {
    const p = run[0], f = p.total ? " · " + Math.floor(100 * p.completed / p.total) + "%" : "";
    d.firstChild.textContent = "⇩ " + run.length + (run.length > 1 ? " downloads" : " download");
    d.lastChild.textContent = p.model + f;
  }
}
MC.refresh = refresh; MC.loadPulls = loadPulls;
MC.start = render => { MC.render = render; refresh(); setInterval(refresh, 5000) };

/* ---------- shell (sidebar, top bar, dialogs) ---------- */
function buildShell() {
  const view = $("#view"), nav = document.body.dataset.nav || "", search = document.body.dataset.search;
  MC.view = view;
  const aside = el("aside");
  aside.innerHTML = '<div class="logo"><i>&#x2726;</i>MeshControl</div><nav>' +
    [["/", "dash", "Dashboard"], ["/nodes", "nodes", "Nodes"], ["/models", "models", "Models"], ["/chat", "chat", "Chat"]]
      .map(([h, k, l]) => '<a href="' + h + '"' + (k === nav ? ' class="on"' : "") + ">" + l + "</a>").join("") +
    '</nav><a class="dl" id="dl" href="/models"><span></span><small></small></a>' +
    '<div class="gw"><span class="dot" id="gwdot"></span><span id="gwtxt">Connecting…</span><small id="gwsub"></small></div>';
  const main = el("main"), top = el("header", "top");
  if (search) { const q = el("input"); q.id = "q"; q.type = "search"; q.placeholder = search; q.setAttribute("aria-label", "Filter"); top.append(q) } else top.append(el("span", "sp"));
  const th = btn("", "", null); th.id = "theme"; th.setAttribute("aria-label", "Toggle light or dark mode");
  const tk = btn("Token", "", null); tk.id = "tokbtn";
  const ad = btn("+ Add Node", "pri", null); ad.id = "addbtn";
  add(top, th, tk, ad);
  main.append(top); document.body.insertBefore(aside, view); document.body.insertBefore(main, view); main.append(view);
  const dlg = el("div");
  dlg.innerHTML = '<dialog id="tokd"><form method="dialog"><h3>Admin token</h3><p class="empty">Required to access the controller API. Kept in this tab only.</p>' +
    '<input id="tokin" type="password" autocomplete="off" placeholder="ADMIN_TOKEN"><div class="row"><button value="x">Cancel</button><button class="pri" id="toksave" value="ok">Save</button></div></form></dialog>' +
    '<dialog id="addd"><form method="dialog"><h3>Add a node</h3><p class="empty">Run this on the machine that hosts Ollama, with the same AGENT_TOKEN as the controller:</p>' +
    '<pre id="addcmd"></pre><div class="row"><span></span><button class="pri" value="x">Close</button></div></form></dialog>';
  document.body.append(dlg);
  const label = () => { th.textContent = document.documentElement.dataset.theme === "dark" ? "☀ Light mode" : "☾ Dark mode" };
  th.onclick = () => { const t = document.documentElement.dataset.theme === "dark" ? "light" : "dark"; document.documentElement.dataset.theme = t; lstore.set("mc_theme", t); label() };
  label();
  tk.onclick = () => $("#tokd").showModal();
  $("#toksave").onclick = () => { MC.token = $("#tokin").value.trim(); store.set("mc_token", MC.token); $("#tokin").value = ""; setTimeout(refresh, 50) };
  ad.onclick = () => { $("#addcmd").textContent = "export CONTROLLER_URL=" + location.origin + "\nexport AGENT_TOKEN=<your agent token>\npython agent.py"; $("#addd").showModal() };
  const q = $("#q"); if (q) q.addEventListener("input", e => { MC.q = e.target.value.toLowerCase(); if (MC.render) MC.render() });
}
buildShell();
