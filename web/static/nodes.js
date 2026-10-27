"use strict";
/* Nodes page: every registered node with its status. */
let filter = "all";
const FILTERS = [["all", "All"], ["online", "Online"], ["provider_unreachable", "Provider unreachable"], ["offline", "Offline"]];

function render() {
  if (!MC.loaded) { $("#sub").textContent = MC.ok ? "Loading…" : "Waiting for the controller… (enter the admin token if prompted)"; return }
  const count = k => k === "all" ? MC.nodes.length : MC.nodes.filter(n => nodeState(n) === k).length;
  $("#sub").textContent = MC.nodes.length + " registered";
  const lg = $("#legend"); clear(lg);
  lg.append(legend(["on-d", "Online"], ["or-d", "Provider unreachable"], ["off-d", "Offline"]));
  const chips = $("#chips"); clear(chips);
  FILTERS.forEach(([k, l]) => { const b = btn(l + " (" + count(k) + ")", k === filter ? "on" : "", () => { filter = k; render() }); b.setAttribute("aria-pressed", String(k === filter)); chips.append(b) });
  const rows = MC.nodes.filter(n => (filter === "all" || nodeState(n) === filter) && match(n.node + " " + n.ollama_url)).map(n => {
    const s = nodeState(n), mods = nodeModels(n);
    return [A(nodeUrl(n.node), n.node, "b"), nodePill(n), n.ollama_url, n.online ? pct(n.cpu) : "–", n.online ? pct(n.ram) : "–",
      String(n.models.length), mods.length ? bytes(sumSize(mods)) : "–", ago(n.last_seen)];
  });
  const t = $("#table"); clear(t);
  t.append(tbl(["Name", "Status", "Ollama URL", "CPU", "RAM", "Models", "Disk used", "Last seen"], rows, MC.q || filter !== "all" ? "No matching nodes" : "No nodes registered yet. Use “+ Add Node” to connect one."));
}
MC.start(render);
