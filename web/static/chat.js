"use strict";
/* Chat: streaming conversation through the cluster, auto-routed or pinned to a node. */
const chat = { msgs: [], busy: false, ctrl: null };
const sel = { model: "", node: "" };
const ui = { cm: $("#cm"), cn: $("#cn"), scroll: $("#scroll"), thread: $("#thread"), ta: $("#ta"), send: $("#send"), route: $("#route") };
const SUGGESTIONS = [
  ["Explain a concept", "Explain how DNS resolution works, step by step."],
  ["Write code", "Write a Python function that merges two sorted lists, with tests."],
  ["Summarize", "Give me the pros and cons of microservices versus a monolith."],
  ["Draft a message", "Draft a polite email declining a meeting invitation."],
];

const thinkingBtn = $("#thinking");
let thinking = false;

thinkingBtn?.addEventListener("click", () => {
  thinking = !thinking;
  thinkingBtn.setAttribute("aria-pressed", String(thinking));
  thinkingBtn.title = `Thinking: ${thinking ? "On" : "Off"}`;
});

/* ---------- persistence (per device, survives closing the tab/browser) ---------- */
const KEY = "mc_chat";
function save() {
  const msgs = chat.msgs.filter(m => !m.pending || m.content).map(m => ({ role: m.role, content: m.content, thinking: m.thinking, model: m.model, meta: m.meta, err: m.err }));
  try { localStorage.setItem(KEY, JSON.stringify({ msgs, model: sel.model, node: sel.node })) } catch (e) {}
}
try {
  /* one-time migration from the old per-tab store */
  const raw = localStorage.getItem(KEY) || (typeof store !== "undefined" && store.get ? store.get(KEY) : "") || "{}";
  const s = JSON.parse(raw);
  chat.msgs = Array.isArray(s.msgs) ? s.msgs : []; sel.model = s.model || ""; sel.node = s.node || "";
} catch (e) {}
{ const p = new URLSearchParams(location.search); if (p.get("model")) sel.model = p.get("model"); if (p.has("node")) sel.node = p.get("node") }
addEventListener("pagehide", save);

/* ---------- lightweight, safe markdown (DOM nodes only, never HTML strings) ---------- */
/* math first ($$..$$, \(..\), \[..\], $..$), then code, bold, italic, links */
/* Math is drawn by the built-in renderer below (no KaTeX needed). Some models (e.g. gemma) emit LaTeX with no delimiters at all, so BARE catches runs like \text{N/m}^2 or Y = \frac{a}{b}. */
const BRACE = String.raw`\{(?:[^{}]|\{(?:[^{}]|\{[^{}]*\})*\})*\}`;
const CMD = String.raw`\\[a-zA-Z]+(?:\s*${BRACE})*`;
const ATOM = String.raw`(?:${CMD}|(?<![A-Za-z])[A-Za-z](?![A-Za-z])|\d+(?:\.\d+)?)(?:\s*[\^_](?:${BRACE}|[A-Za-z0-9]+|${CMD}))*`;
const BARE = String.raw`(?:(?<![A-Za-z\\])[A-Za-z]\s*=\s*(?:-\s*)?)?${CMD}(?:\s*[\^_](?:${BRACE}|[A-Za-z0-9]+))*(?:\s*(?:[=+\-×·*/]\s*)?${ATOM})*`;
const INLINE = new RegExp("(" + [
  String.raw`\$\$[^\n]+?\$\$`, String.raw`\\\([^\n]+?\\\)`, String.raw`\\\[[^\n]+?\\\]`,
  String.raw`\$(?![\s\d])[^$\n]*[^$\s]\$(?!\d)`,
  String.raw`\$\d[^$\n]*\\[^$\n]*[^$\s]\$(?!\d)`,   // $2\text{ mm}$ (digit start, but has a backslash)
  String.raw`\`[^\`\n]+\``, String.raw`\*\*[^*\n]+\*\*`, String.raw`\*[^*\s][^*\n]*\*`,
  String.raw`\[[^\]\n]+\]\(https?:\/\/[^\s)]+\)`, BARE,
].join("|") + ")", "g");
/* ---------- built-in LaTeX renderer (no external library, so it works under a strict CSP) ---------- */
(() => {
  const st = document.createElement("style");
  st.textContent = `
.mj{font-family:"STIX Two Math","Cambria Math","Times New Roman",serif;font-size:1.1em;line-height:1.5}
.mj.block{display:block;text-align:center;margin:.7em 0;overflow-x:auto}
.mj .mi{font-style:italic}.mj .tx{font-style:normal}.mj .b{font-weight:700}
.mj .op{margin:0 .22em}.mj .fn{margin:0 .15em 0 .05em}
.mj .frac{display:inline-flex;flex-direction:column;vertical-align:middle;text-align:center;margin:0 .15em}
.mj .frac>.num{border-bottom:1px solid currentColor;padding:0 .25em .05em}
.mj .frac>.den{padding:.05em .25em 0}
.mj .rad{display:inline-flex;align-items:stretch}
.mj .rc{border-top:1px solid currentColor;padding:0 .15em}
.mj sup,.mj sub{font-size:.72em;line-height:0}
.mj .sp{display:inline-block;width:.2em}`;
  document.head.append(st);
})();
const SYM = { alpha:"α",beta:"β",gamma:"γ",delta:"δ",epsilon:"ε",varepsilon:"ε",zeta:"ζ",eta:"η",theta:"θ",vartheta:"ϑ",iota:"ι",kappa:"κ",lambda:"λ",mu:"μ",nu:"ν",xi:"ξ",pi:"π",rho:"ρ",sigma:"σ",tau:"τ",upsilon:"υ",phi:"φ",varphi:"φ",chi:"χ",psi:"ψ",omega:"ω",Gamma:"Γ",Delta:"Δ",Theta:"Θ",Lambda:"Λ",Xi:"Ξ",Pi:"Π",Sigma:"Σ",Phi:"Φ",Psi:"Ψ",Omega:"Ω",
  times:"×",cdot:"·",div:"÷",pm:"±",mp:"∓",leq:"≤",le:"≤",geq:"≥",ge:"≥",neq:"≠",ne:"≠",approx:"≈",propto:"∝",sim:"∼",equiv:"≡",rightarrow:"→",to:"→",leftarrow:"←",leftrightarrow:"↔",Rightarrow:"⇒",Leftarrow:"⇐",
  infty:"∞",partial:"∂",nabla:"∇",circ:"°",degree:"°",ldots:"…",dots:"…",cdots:"⋯",sum:"∑",int:"∫",prod:"∏",therefore:"∴",because:"∵",angle:"∠",perp:"⊥",parallel:"∥",in:"∈",hbar:"ħ",ell:"ℓ",prime:"′",
  checkmark:"✓",square:"☐",Box:"☐",blacksquare:"■",bullet:"•",star:"⋆",dagger:"†",implies:"⟹",Longrightarrow:"⟹",impliedby:"⟸",Longleftarrow:"⟸",iff:"⟺",longrightarrow:"⟶",longleftarrow:"⟵",mapsto:"↦",uparrow:"↑",downarrow:"↓",
  forall:"∀",exists:"∃",neg:"¬",land:"∧",lor:"∨",cap:"∩",cup:"∪",subset:"⊂",subseteq:"⊆",supset:"⊃",supseteq:"⊇",notin:"∉",emptyset:"∅",setminus:"∖",ll:"≪",gg:"≫",cong:"≅",simeq:"≃",oplus:"⊕",otimes:"⊗",mid:"∣",vert:"|",Vert:"‖",langle:"⟨",rangle:"⟩",lbrace:"{",rbrace:"}",lt:"<",gt:">",lceil:"⌈",rceil:"⌉",lfloor:"⌊",rfloor:"⌋",
  varpi:"ϖ",varrho:"ϱ",varsigma:"ς",digamma:"ϝ",aleph:"ℵ",Re:"ℜ",Im:"ℑ",Upsilon:"Υ",Omega_:"Ω" };
const REL = new Set(["times","div","pm","mp","leq","le","geq","ge","neq","ne","approx","propto","sim","equiv","rightarrow","to","leftarrow","leftrightarrow","Rightarrow","Leftarrow","implies","Longrightarrow","impliedby","Longleftarrow","iff","longrightarrow","longleftarrow","mapsto","ll","gg","cong","simeq","subset","subseteq","supset","supseteq","notin","cap","cup","land","lor"]);
const FUNCS = new Set(["sin","cos","tan","cot","sec","csc","log","ln","exp","lim","min","max","arcsin","arccos","arctan","sinh","cosh","tanh"]);
function renderTeX(src) {
  let i = 0;
  const span = (c, t) => { const e = document.createElement("span"); if (c) e.className = c; if (t != null) e.textContent = t; return e };
  const skipWs = () => { while (i < src.length && /\s/.test(src[i])) i++ };
  function glyph(c) {
    if (/[A-Za-z]/.test(c)) return span("mi", c);
    if (c === "=" || c === "+" || c === "<" || c === ">") return span("op", c);
    if (c === "-") return span("op", "−");
    if (c === "*") return span("", "∗");
    if (c === "'") return span("", "′");
    return span("", c);
  }
  function rawGroup() {                 /* raw text of {...} or one char */
    skipWs();
    if (src[i] !== "{") return src[i++] || "";
    let d = 0, s = i + 1;
    for (; i < src.length; i++) { if (src[i] === "{") d++; else if (src[i] === "}" && --d === 0) break }
    const out = src.slice(s, i); i++; return out;
  }
  function arg() {                      /* next argument: {group} or single token */
    skipWs();
    if (src[i] === "{") { i++; const n = seq("}"); i++; return n }
    if (i >= src.length) return span();
    return atom();
  }
  function atom() {
    const c = src[i];
    if (c === "{") { i++; const n = seq("}"); i++; return n }
    if (c !== "\\") { i++; return glyph(c) }
    i++;
    const nm = /^[a-zA-Z]+/.exec(src.slice(i));
    if (!nm) {
      const ch = src[i++];
      if (ch === "," || ch === ";" || ch === ":" || ch === " ") return span("sp");
      if (ch === "!" || ch === undefined) return span();
      if (ch === "\\") return span("", " ");
      return span("", ch);
    }
    const name = nm[0]; i += name.length;
    switch (name) {
      case "frac": case "dfrac": case "tfrac": { const n = span("num"), d = span("den"), f = span("frac"); n.append(arg()); d.append(arg()); f.append(n, d); return f }
      case "sqrt": {
        skipWs(); let idx = null;
        if (src[i] === "[") { const j = src.indexOf("]", i); if (j > 0) { idx = src.slice(i + 1, j); i = j + 1 } }
        const w = span("rad"), rc = span("rc"); rc.append(arg());
        if (idx) { const s = document.createElement("sup"); s.textContent = idx; w.append(s) }
        w.append(span("", "√"), rc); return w;
      }
      case "text": case "mathrm": case "textrm": case "textit": case "mbox": case "operatorname": return span("tx", rawGroup());
      case "mathbf": case "textbf": case "boldsymbol": return span("tx b", rawGroup());
      case "left": case "right": { skipWs(); let d = src[i++]; if (d === "\\") d = src[i++]; return d === "." || d === undefined ? span() : span("", d) }
      case "quad": return span("", "\u2003");
      case "qquad": return span("", "\u2003\u2003");
      case "big": case "Big": case "bigg": case "Bigg": case "displaystyle": case "limits": return span();
      case "vec": case "hat": case "bar": { const n = arg(); return span("mi", n.textContent + (name === "vec" ? "\u20D7" : name === "hat" ? "\u0302" : "\u0304")) }
      case "overline": { const s = span("rc"); s.append(arg()); return s }
      case "mathbb": case "mathcal": case "mathit": case "underline": return arg();
    }
    if (SYM[name]) return span(REL.has(name) ? "op" : "", SYM[name]);
    if (FUNCS.has(name)) return span("fn", name);
    return span("tx", "\\" + name);
  }
  function seq(end) {
    const root = span();
    while (i < src.length && src[i] !== end) {
      const c = src[i];
      if (c === "^" || c === "_") { i++; const s = document.createElement(c === "^" ? "sup" : "sub"); s.append(arg()); root.append(s); continue }
      if (/\s/.test(c)) { i++; continue }
      root.append(atom());
    }
    return root;
  }
  return seq();
}
function math(parent, tex, display) {
  const s = el(display ? "div" : "span", display ? "mj block" : "mj");
  try { s.append(renderTeX(tex)) } catch (e) { s.textContent = tex }
  parent.append(s);
}
function inline(parent, text) {
  let last = 0, m; const re = new RegExp(INLINE.source, "g");
  while ((m = re.exec(text))) {
    if (m.index > last) parent.append(T(text.slice(last, m.index)));
    const t = m[0];
    if (t.startsWith("$$")) math(parent, t.slice(2, -2), true);
    else if (t.startsWith("\\(")) math(parent, t.slice(2, -2), false);
    else if (t.startsWith("\\[")) math(parent, t.slice(2, -2), true);
    else if (t[0] === "$") math(parent, t.slice(1, -1), false);
    else if (t[0] === "`") parent.append(el("code", "", t.slice(1, -1)));
    else if (t.startsWith("**")) { const s = el("strong"); inline(s, t.slice(2, -2)); parent.append(s) }
    else if (t[0] === "*") { const s = el("em"); inline(s, t.slice(1, -1)); parent.append(s) }
    else if (t[0] === "[") { const mm = /^\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)$/.exec(t), a = el("a", "", mm[1]); a.href = mm[2]; a.target = "_blank"; a.rel = "noopener noreferrer"; parent.append(a) }
    else math(parent, t.trim(), false);   // undelimited LaTeX
    last = re.lastIndex;
  }
  if (last < text.length) parent.append(T(text.slice(last)));
}
async function copyText(text) {
  try { if (navigator.clipboard && isSecureContext) { await navigator.clipboard.writeText(text); return true } } catch (e) {}
  const ta = el("textarea"); ta.value = text; ta.style.cssText = "position:fixed;opacity:0"; document.body.append(ta); ta.select();
  let ok = false; try { ok = document.execCommand("copy") } catch (e) {} ta.remove(); return ok;
}
function copyBtn(label, getText) {
  const b = btn(label, "", async () => { if (await copyText(getText())) { b.textContent = "Copied"; setTimeout(() => { b.textContent = label }, 1500) } });
  return b;
}
function codeBlock(lang, code) {
  const head = add(el("div", "ch"), el("span", "", lang || "code"), copyBtn("Copy", () => code));
  return add(el("div", "code"), head, add(el("pre"), el("code", "", code)));
}
const cells = l => l.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map(c => c.trim());

/* nested lists: indentation decides depth; a different list type at the same indent starts a new list */
const LI = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;
const isOrd = tok => /\d/.test(tok[0]);
function parseList(parent, lines, i, ctx) {
  const m0 = LI.exec(lines[i]), base = m0[1].length, ordered = isOrd(m0[2]), top = parent.classList && parent.classList.contains("md");
  const list = el(ordered ? "ol" : "ul"); let count = 0;
  if (ordered) {
    let start = parseInt(m0[2]);
    /* models often write "1." for every item with bullets in between; keep numbering continuous */
    if (top && ctx && ctx.next && start === 1) start = ctx.next;
    if (start !== 1) list.start = start;
    list._start = start;
  }
  while (i < lines.length) {
    const m = LI.exec(lines[i]);
    if (!m || m[1].length < base) break;
    if (m[1].length > base) { i = parseList(list.lastElementChild || list, lines, i, null); continue }
    if (isOrd(m[2]) !== ordered) break;
    const li = el("li"); inline(li, m[3].replace(/^\[ \]\s/, "☐ ").replace(/^\[[xX]\]\s/, "☑ ")); list.append(li); count++; i++;
    if (!lines[i]?.trim() && LI.test(lines[i + 1] || "")) i++;   // blank line between items
  }
  if (top && ctx) ctx.next = ordered ? list._start + count : ctx.next;
  parent.append(list); return i;
}

function md(text) {
  const root = el("div", "md"), lines = text.replace(/\r\n/g, "\n").split("\n"); let i = 0, para = [];
  const ctx = { next: 0 };
  const lines2p = (parent, ls) => ls.forEach((l, k) => { if (k) parent.append(el("br")); inline(parent, l) });
  const flush = () => { if (para.length) { const p = el("p"); lines2p(p, para); root.append(p); para = [] } };
  while (i < lines.length) {
    const line = lines[i]; let m;
    if (line.trim() && !LI.test(line)) ctx.next = 0;
    if ((m = /^```\s*([\w+#.-]*)\s*$/.exec(line))) { flush(); const code = []; i++; while (i < lines.length && !/^```\s*$/.test(lines[i])) code.push(lines[i++]); i++; root.append(codeBlock(m[1], code.join("\n"))); continue }
    if (!line.trim()) { flush(); i++; continue }
    /* multi-line display math: $$ ... $$ or \[ ... \] on their own lines */
    if ((m = /^\s*(\$\$|\\\[)\s*$/.exec(line))) {
      flush(); const close = m[1] === "$$" ? /^\s*\$\$\s*$/ : /^\s*\\\]\s*$/, buf = []; i++;
      while (i < lines.length && !close.test(lines[i])) buf.push(lines[i++]); i++;
      math(root, buf.join("\n"), true); continue;
    }
    if ((m = /^(#{1,6})\s+(.*)$/.exec(line))) { flush(); const h = el("h" + Math.min(m[1].length, 4)); inline(h, m[2]); root.append(h); i++; continue }
    if (/^(-{3,}|\*{3,})\s*$/.test(line)) { flush(); root.append(el("hr")); i++; continue }
    if (/^\s*\|.*\|\s*$/.test(line) && i + 1 < lines.length && /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/.test(lines[i + 1])) {
      flush(); const heads = cells(line); i += 2; const body = [];
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) body.push(cells(lines[i++]));
      const t = el("table"), tr = el("tr"), tb = el("tbody");
      heads.forEach(h => { const th = el("th"); inline(th, h); tr.append(th) });
      body.forEach(r => { const row = el("tr"); heads.forEach((_, k) => { const td = el("td"); inline(td, r[k] || ""); row.append(td) }); tb.append(row) });
      root.append(add(el("div", "tw"), add(t, add(el("thead"), tr), tb))); continue;
    }
    if (LI.test(line)) { flush(); i = parseList(root, lines, i, ctx); continue }
    if (/^>\s?/.test(line)) { flush(); const buf = []; while (i < lines.length && /^>\s?/.test(lines[i])) buf.push(lines[i++].replace(/^>\s?/, "")); const p = el("p"); lines2p(p, buf); root.append(add(el("blockquote"), p)); continue }
    para.push(line); i++;
  }
  flush(); return root;
}
/* Reasoning models may wrap their thinking in <think> tags. */
function splitThink(m) {
  const c = m.content || "", t = /^\s*<think>([\s\S]*?)(<\/think>|$)/.exec(c);
  let think = m.thinking || "", answer = c, open = false;
  if (t) { think = (think + "\n" + t[1]).trim(); open = !t[2]; answer = c.slice(t[0].length).replace(/^\s+/, "") }
  return { think, answer, open: open || (!!m.thinking && !c && !!m.pending) };
}

/* ---------- rendering ---------- */
function messageEl(m, idx) {
  if (m.role === "user") return add(el("div", "msg user"), el("div", "body", m.content));
  const body = el("div", "body"), { think, answer, open } = splitThink(m);
  body.append(el("div", "who", m.model || "Assistant"));
  if (m.err) {
    const e = add(el("div", "err"), el("b", "", "Couldn’t get a response"), T(m.content));
    e.append(add(el("div", "tools"), btn("Try again", "", () => { chat.msgs.splice(idx, 1); generate() })));
    body.append(e);
  } else {
    if (think) {
      const d = el("details", "think"); d.append(el("summary", "", open ? "Thinking…" : "Thought process"), el("div", "", think));
      d.open = m.thinkOpen != null ? m.thinkOpen : open; d.addEventListener("toggle", () => { m.thinkOpen = d.open }); body.append(d);
    }
    if (answer) body.append(md(answer));
    else if (m.pending && !think) body.append(add(el("div", "typing"), el("i"), el("i"), el("i")));
    if (!m.pending) {
      const tools = el("div", "tools");
      if (answer) tools.append(copyBtn("Copy", () => answer));
      if (idx === chat.msgs.length - 1 && !chat.busy) tools.append(btn("Regenerate", "", () => { chat.msgs.pop(); generate() }));
      if (m.meta) tools.append(el("span", "meta", m.meta));
      if (tools.childNodes.length) body.append(tools);
    }
  }
  return add(el("div", "msg bot"), el("div", "av", "✦"), body);
}
function emptyEl() {
  const box = el("div", "chat-empty"), groups = modelGroups().filter(g => g.ok);
  box.append(el("div", "mark", "✦"));
  if (MC.loaded && !groups.length) {
    const bad = MC.nodes.filter(n => nodeState(n) === "provider_unreachable");
    box.append(el("h2", "", "No model is available"));
    box.append(el("p", "", bad.length ? "Provider unreachable on " + bad.map(n => n.node).join(", ") + ". Ollama cannot be reached, so nothing can answer yet."
      : MC.nodes.length ? "Every node is offline or has no models installed." : "No nodes are registered yet."));
    const p = el("p"); p.append(A("/models?pull=1", "Pull a model"), T(" or "), A("/nodes", "check your nodes"), T(".")); box.append(p);
    return box;
  }
  box.append(el("h2", "", "How can I help today?"));
  box.append(el("p", "", sel.model ? "Using " + sel.model + (sel.node ? " on " + sel.node : "") : "Choose a model above to begin"));
  const g = el("div", "sugg");
  SUGGESTIONS.forEach(([t, prompt]) => { const b = btn("", "", () => { ui.ta.value = prompt; grow(); send() }); b.append(T(t), el("small", "", prompt)); g.append(b) });
  box.append(g); return box;
}
const nearBottom = () => ui.scroll.scrollHeight - ui.scroll.scrollTop - ui.scroll.clientHeight < 140;
const toBottom = () => { ui.scroll.scrollTop = ui.scroll.scrollHeight };
function paint(force) {
  const stick = force || nearBottom();
  clear(ui.thread);
  if (!chat.msgs.length) ui.thread.append(emptyEl());
  else chat.msgs.forEach((m, i) => ui.thread.append(messageEl(m, i)));
  if (stick) toBottom();
  controls();
}
let raf = 0;
function paintLast() {
  if (raf) return;
  raf = requestAnimationFrame(() => {
    raf = 0; const stick = nearBottom(), i = chat.msgs.length - 1, last = ui.thread.lastElementChild;
    if (last && i >= 0) ui.thread.replaceChild(messageEl(chat.msgs[i], i), last);
    if (stick) toBottom();
  });
}
function controls() {
  ui.send.textContent = chat.busy ? "■" : "↑";
  ui.send.setAttribute("aria-label", chat.busy ? "Stop generating" : "Send message");
  ui.send.disabled = !chat.busy && (!sel.model || !ui.ta.value.trim());
  ui.route.textContent = !MC.loaded ? "Connecting…" : sel.node ? "Pinned to " + sel.node : "Auto-routed across " + MC.nodes.filter(n => nodeState(n) === "online").length + " online node(s)";
}
function grow() { ui.ta.style.height = "auto"; ui.ta.style.height = Math.min(ui.ta.scrollHeight, 200) + "px" }

/* ---------- pickers ---------- */
function pickers() {
  if (!MC.loaded) return;
  const nsig = MC.nodes.map(n => n.node + ":" + nodeState(n)).join("|");
  if (sel.node && !MC.nodes.some(n => n.node === sel.node && nodeState(n) === "online")) sel.node = "";
  if (ui.cn.dataset.sig !== nsig) {
    ui.cn.dataset.sig = nsig; clear(ui.cn); ui.cn.append(new Option("Auto-route", ""));
    MC.nodes.forEach(n => { const s = nodeState(n), o = new Option(n.node + (s === "online" ? "" : " — " + STATES[s].label.toLowerCase()), n.node); o.disabled = s !== "online"; ui.cn.append(o) });
  }
  ui.cn.value = sel.node;
  const names = (sel.node ? nodeModels(getNode(sel.node)).filter(m => MC.models.some(e => e.node === sel.node && e.model === m.name && e.available)).map(m => m.name) : modelGroups().filter(g => g.ok).map(g => g.name)).sort();
  const msig = sel.node + "@" + names.join("|");
  if (ui.cm.dataset.sig !== msig) {
    ui.cm.dataset.sig = msig; clear(ui.cm);
    if (!names.length) ui.cm.append(new Option("No model available", ""));
    names.forEach(n => ui.cm.append(new Option(n, n)));
  }
  if (!names.includes(sel.model)) sel.model = names[0] || "";
  ui.cm.value = sel.model; ui.cm.disabled = !names.length;
}

/* ---------- sending ---------- */
const plain = m => splitThink(m).answer;
async function generate() {
  if (chat.busy || !sel.model) return;
  const history = chat.msgs.filter(m => !m.err && (m.role === "user" || plain(m))).map(m => ({ role: m.role, content: m.role === "user" ? m.content : plain(m) }));
  history.unshift({ role: "system", content: "Write all math in LaTeX: inline as $...$ and standalone equations as $$...$$. Never output bare LaTeX without dollar signs." });
  const r = { role: "assistant", content: "", thinking: "", pending: true, model: sel.model }; chat.msgs.push(r);
  chat.busy = true; chat.ctrl = new AbortController(); paint(true);
  const url = sel.node ? "/api" + nodeUrl(sel.node) + "/ollama/api/chat" : "/api/ollama/api/chat", t0 = performance.now();
  let node = null, stats = null;
  try {
    const res = await fetch(url, { method: "POST", signal: chat.ctrl.signal, headers: hdr({ "Content-Type": "application/json" }), body: JSON.stringify({ model: sel.model, messages: history, stream: true, think: thinking }) });
    if (res.status === 401) { $("#tokd").showModal(); throw new Error("Unauthorized: enter the admin token") }
    if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error(j.error || "HTTP " + res.status) }
    node = res.headers.get("X-MeshControl-Node");
    const rd = res.body.getReader(), dec = new TextDecoder(); let buf = "";
    for (;;) {
      const { done, value } = await rd.read(); if (done) break;
      buf += dec.decode(value, { stream: true }); const ls = buf.split("\n"); buf = ls.pop();
      for (const l of ls) {
        if (!l.trim()) continue; let j; try { j = JSON.parse(l) } catch (e) { continue }
        if (j.error) throw new Error(j.error);
        if (j.message) { if (j.message.content) r.content += j.message.content; if (j.message.thinking) r.thinking += j.message.thinking; paintLast() }
        if (j.done) stats = j;
      }
    }
    if (!stats && !r.content) throw new Error("The node closed the connection before replying.");
  } catch (e) {
    if (e.name === "AbortError") { if (!r.content && !r.thinking) chat.msgs.splice(chat.msgs.indexOf(r), 1) }
    else { r.err = true; r.content = e instanceof TypeError ? "Can’t reach the controller." : e.message }
  }
  r.pending = false; chat.busy = false; chat.ctrl = null;
  if (!r.err) {
    const secs = (performance.now() - t0) / 1000, tps = stats && stats.eval_count && stats.eval_duration ? stats.eval_count / (stats.eval_duration / 1e9) : 0;
    r.meta = [node, tps ? tps.toFixed(1) + " tok/s" : "", secs.toFixed(1) + "s"].filter(Boolean).join(" · ");
  }
  save(); paint();
}
function send() {
  const text = ui.ta.value.trim();
  if (chat.busy) { chat.ctrl && chat.ctrl.abort(); return }
  if (!text || !sel.model) return;
  chat.msgs.push({ role: "user", content: text });
  save();   // keep the prompt even if the page closes mid-reply
  ui.ta.value = ""; grow(); generate();
}

/* ---------- wiring ---------- */
ui.send.onclick = send;
ui.ta.addEventListener("input", () => { grow(); controls() });
ui.ta.addEventListener("keydown", e => { if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); send() } });
ui.cm.addEventListener("change", () => { sel.model = ui.cm.value; save(); controls() });
ui.cn.addEventListener("change", () => { sel.node = ui.cn.value; pickers(); save(); if (!chat.msgs.length) paint() });
$("#newchat").onclick = () => { if (chat.ctrl) chat.ctrl.abort(); chat.msgs = []; chat.busy = false; save(); paint(true); ui.ta.focus() };

let emptySig = "";
MC.start(() => {
  pickers(); controls();
  if (!chat.msgs.length) { const sig = MC.loaded + "|" + sel.model + "|" + sel.node + "|" + MC.nodes.map(n => nodeState(n)).join(); if (sig !== emptySig) { emptySig = sig; paint() } }
});
chat.msgs = chat.msgs.filter(m => m.content || m.role === "user");
paint(true); ui.ta.focus();