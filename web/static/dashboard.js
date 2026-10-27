"use strict";
/* Dashboard: overview only. */
const ICONS = { on: ["⏻", "rgba(22,163,74,.15)"], off: ["!", "rgba(239,68,68,.15)"], warn: ["!", "rgba(234,88,12,.18)"], mod: ["+", "rgba(59,130,246,.15)"], rm: ["−", "rgba(107,114,128,.2)"] };

function clusterEl() {
  const box = el("div", "cluster");
  if (!MC.nodes.length) { box.style.display = "block"; return add(box, el("p", "empty", "No nodes registered yet. Use “+ Add Node” to connect one.")) }
  const L = el("div", "col l"), R = el("div", "col r"), hub = add(el("div", "hub", "MeshControl"), el("small", "", "Controller"));
  MC.nodes.slice(0, 8).forEach((n, i) => {
    const s = nodeState(n), c = el("div", "nc" + (s === "provider_unreachable" ? " warn" : ""));
    const dot = s === "offline" ? "off-d" : s === "provider_unreachable" ? "or-d" : n.cpu > 80 ? "hi-d" : "on-d";
    const t = add(el("span"), el("i", "dot " + dot), A(nodeUrl(n.node), n.node, "b"));
    const right = s === "online" ? el("span", "", pct(n.cpu) + " CPU") : s === "provider_unreachable" ? el("span", "txt-or", "Provider unreachable") : el("span", "", "Offline");
    add(c, add(el("div", "r"), t, right));
    if (s === "online") { const f = el("i"); f.style.width = (n.cpu || 0) + "%"; if (n.cpu > 80) f.style.background = "var(--warn)"; add(c, add(el("div", "bar"), f), el("small", "", n.models.length + " models")) }
    else if (s === "provider_unreachable") add(c, el("small", "", "Ollama not reachable · " + pct(n.cpu) + " CPU"));
    else add(c, el("small", "", "Last seen " + ago(n.last_seen)));
    (i % 2 ? R : L).append(c);
  });
  return add(box, L, hub, R);
}

function actsEl() {
  const box = el("div");
  if (!MC.events.length) return add(box, el("p", "empty", "Waiting for activity. Node and model changes appear here while you browse."));
  MC.events.forEach(e => { const [i, bg] = ICONS[e.k] || ICONS.on, ic = el("div", "ico", i); ic.style.background = bg; box.append(add(el("div", "act"), ic, add(el("div"), el("b", "", e.t), el("small", "", e.s)), el("small", "", ago(e.at)))) });
  return box;
}

function render() {
  if (!MC.loaded) return;
  const by = k => MC.nodes.filter(n => nodeState(n) === k), on = by("online"), pu = by("provider_unreachable"), off = by("offline");
  const mm = modelGroups(), live = MC.nodes.filter(n => n.online && n.cpu != null), avg = k => live.length ? live.reduce((a, n) => a + n[k], 0) / live.length : null;

  const al = $("#alert"); clear(al);
  if (pu.length) al.append(banner("orange", "!", "Provider unreachable on " + pu.length + " node" + (pu.length === 1 ? "" : "s"),
    pu.map(n => n.node).join(", ") + (pu.length === 1 ? " is" : " are") + " reporting in, but Ollama cannot be reached. Open the node for details."));

  const st = $("#stats"); clear(st);
  const parts = [MC.nodes.length + " total"]; if (pu.length) parts.push(pu.length + " provider unreachable"); if (off.length) parts.push(off.length + " offline");
  add(st,
    stat("▣", "rgba(22,163,74,.15)", on.length, "Online Nodes", parts.join(" · ")),
    stat("⬢", "rgba(59,130,246,.15)", mm.filter(g => g.ok).length, "Available Models", mm.length + " distinct · " + MC.models.length + " copies"),
    stat("◔", "rgba(234,179,8,.18)", pct(avg("cpu")), "Average CPU", "across reporting nodes"),
    stat("◕", "rgba(22,163,74,.15)", pct(avg("ram")), "Average RAM", "across reporting nodes"));

  const cl = $("#clegend"); clear(cl); cl.append(legend(["on-d", "Online"], ["or-d", "Provider unreachable"], ["off-d", "Offline"], ["hi-d", "High load"]));
  const c = $("#cluster"); clear(c); c.append(clusterEl());
  const a = $("#acts"); clear(a); a.append(actsEl());

  const nt = $("#nodes"); clear(nt);
  nt.append(tbl(["Name", "Status", "CPU", "RAM", "Models"], MC.nodes.slice(0, 5).map(n => [A(nodeUrl(n.node), n.node, "b"), nodePill(n), n.online ? pct(n.cpu) : "–", n.online ? pct(n.ram) : "–", String(n.models.length)]), "No nodes registered yet"));
  const mt = $("#models"); clear(mt);
  mt.append(tbl(["Name", "Size", "Nodes", "Status"], mm.slice(0, 5).map(g => [A(modelUrl(g.name), g.name, "b"), bytes(g.size), String(g.nodes.length), modelStatePill(g)]), "No models yet"));

  const hl = $("#chlegend"); clear(hl); hl.append(cpuRamLegend());
  const ch = $("#chart"); clear(ch); ch.append(cpuRam(MC.hist));
}
MC.start(render);
