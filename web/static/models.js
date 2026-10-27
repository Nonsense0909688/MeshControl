"use strict";
/* Models page: cluster-wide inventory with size, parameters and download date, plus pulling new models. */
const params = new URLSearchParams(location.search);
const sort = { key: "name", dir: 1 };
const paramNum = s => { const m = /^([\d.]+)\s*([KMBT]?)/i.exec(s || ""); return m ? parseFloat(m[1]) * ({ K: 1e3, M: 1e6, B: 1e9, T: 1e12 }[m[2].toUpperCase()] || 1) : -1 };
const SORTS = {
  name: g => g.name.toLowerCase(), family: g => (g.family || "").toLowerCase(), params: g => paramNum(g.parameter_size),
  quant: g => g.quantization_level || "", size: g => g.size == null ? -1 : g.size, modified: g => g.modified_at == null ? -1 : g.modified_at,
  nodes: g => g.nodes.length,
};
const COLS = [["name", "Model"], ["family", "Family"], ["params", "Parameters"], ["quant", "Quantization"], ["size", "Size"], ["modified", "Downloaded"], ["nodes", "Nodes"]];

function head(key, label) {
  const b = btn(label + (sort.key === key ? (sort.dir > 0 ? " ▲" : " ▼") : ""), "", () => { sort.dir = sort.key === key ? -sort.dir : 1; sort.key = key; render() });
  b.setAttribute("aria-label", "Sort by " + label);
  return b;
}

function renderTable() {
  const f = SORTS[sort.key];
  const groups = modelGroups().filter(g => match([g.name, g.family, g.parameter_size, g.quantization_level].join(" ")))
    .sort((a, b) => (f(a) > f(b) ? 1 : f(a) < f(b) ? -1 : a.name.localeCompare(b.name)) * sort.dir);
  const rows = groups.map(g => {
    const when = dateCell(g.modified_at);
    if (g.copies.length > 1) when.title = g.copies.map(c => c.node + ": " + (c.modified_at ? absDate(c.modified_at) : "unknown")).join("\n");
    const nodes = add(el("span"), T(String(g.nodes.length)), el("small", "", g.nodes.join(", ")));
    return [A(modelUrl(g.name), g.name, "b"), g.family || "–", g.parameter_size || "–", g.quantization_level || "–", bytes(g.size), when, nodes, modelStatePill(g)];
  });
  const heads = COLS.map(([k, l]) => head(k, l)).concat("Status");
  const t = $("#table"); clear(t);
  t.append(tbl(heads, rows, MC.q ? "No matching models" : "No models yet. Pull one above to get started."));
}

function renderPullForm() {
  const sel = $("#pn"), targets = Pull.targets(), want = sel.value || params.get("node") || "";
  const sig = MC.nodes.map(n => n.node + ":" + nodeState(n)).join("|");
  if (sel.dataset.sig !== sig) {
    sel.dataset.sig = sig; clear(sel);
    if (!MC.nodes.length) sel.append(new Option("No nodes registered", ""));
    MC.nodes.forEach(n => {
      const s = nodeState(n), o = new Option(n.node + (s === "online" ? "" : " — " + STATES[s].label.toLowerCase()), n.node);
      o.disabled = s !== "online"; sel.append(o);
    });
    if (targets.some(n => n.node === want)) sel.value = want; else if (targets.length) sel.value = targets[0].node;
  }
  $("#pgo").disabled = !targets.length;
  if (MC.loaded && !targets.length && MC.nodes.length) { $("#perr").textContent = "No node can accept a download right now: every node is offline or its provider is unreachable."; $("#perr").hidden = false }
  else if ($("#perr").dataset.kind !== "submit") $("#perr").hidden = true;
}

function renderDownloads() {
  const shown = Pull.list($("#downloads"), MC.pulls);
  $("#dlcard").hidden = !shown;
}

function render() {
  if (!MC.loaded) { $("#sub").textContent = MC.ok ? "Loading…" : "Waiting for the controller… (enter the admin token if prompted)"; return }
  const groups = modelGroups(), copies = MC.models.length, hosts = new Set(MC.models.map(m => m.node)).size;
  $("#sub").textContent = groups.length + " distinct across " + hosts + " node" + (hosts === 1 ? "" : "s");
  const st = $("#stats"); clear(st);
  add(st,
    stat("⬢", "rgba(59,130,246,.15)", groups.length, "Distinct models", copies + " copies in total"),
    stat("◔", "rgba(22,163,74,.15)", groups.filter(g => g.ok).length, "Available now", "on a reachable node"),
    stat("▣", "rgba(234,179,8,.18)", bytes(sumSize(MC.models)), "Disk used", "across all nodes"),
    stat("⇩", "rgba(107,114,128,.2)", Pull.running().length, "Downloading", "pulls in progress"));
  renderPullForm(); renderDownloads(); renderTable();
}

$("#pullform").addEventListener("submit", async e => {
  e.preventDefault();
  const model = $("#pm").value.trim(), node = $("#pn").value, err = $("#perr");
  err.hidden = true; err.dataset.kind = "";
  if (!model || !node) return;
  $("#pgo").disabled = true;
  try { await Pull.start(node, model); $("#pm").value = ""; await MC.loadPulls(); renderDownloads() }
  catch (ex) { err.textContent = ex.message; err.dataset.kind = "submit"; err.hidden = false }
  finally { renderPullForm() }
});
$("#pm").addEventListener("input", () => { $("#perr").dataset.kind = "" });
if (params.get("model")) $("#pm").value = params.get("model");
if (params.has("pull")) $("#pm").focus();
Pull.watch(render);
MC.start(render);
