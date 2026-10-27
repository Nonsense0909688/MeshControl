"use strict";
/* Node detail: status, metrics, installed models with size and download date. */
const NODE = decodeURIComponent(location.pathname.replace(/^\/nodes\//, "").replace(/\/+$/, ""));
document.title = "MeshControl · " + NODE;

function render() {
  if (!MC.loaded) return;
  const n = getNode(NODE), nf = $("#nf"), content = $("#content");
  if (!n) {
    content.hidden = true; nf.hidden = false; clear(nf);
    add(nf, card("Node not found", null, add(el("p", "empty"), T("“" + NODE + "” is not registered (it may have been purged after a long silence). "), A("/nodes", "Back to nodes"))));
    return;
  }
  nf.hidden = true; content.hidden = false;
  const s = nodeState(n), mods = nodeModels(n);
  $("#crumbname").textContent = NODE; $("#title").textContent = NODE; $("#sub").textContent = n.ollama_url;
  const badge = $("#badge"); clear(badge); badge.append(nodePill(n));
  $("#chatlink").href = "/chat?node=" + enc(NODE); $("#chatlink").hidden = s !== "online";
  $("#pulllink").href = "/models?pull=1&node=" + enc(NODE); $("#pulllink").hidden = s !== "online";

  const al = $("#alert"); clear(al);
  if (s === "provider_unreachable") al.append(banner("orange", "!", "Provider unreachable",
    "The agent on " + NODE + " is reporting in, but Ollama cannot be reached over the network. Check that Ollama is running and listens on the LAN (OLLAMA_HOST=0.0.0.0:11434) and that port 11434 is open in the firewall. This node is skipped by routing until it recovers."));
  else if (s === "offline") al.append(banner("bad", "×", "Node offline",
    "No heartbeat for " + ago(n.last_seen).replace(" ago", "") + ". Check that the agent is running on this machine and can reach the controller."));

  const st = $("#stats"); clear(st);
  add(st,
    stat("◔", "rgba(234,179,8,.18)", n.online ? pct(n.cpu) : "–", "CPU", "reported by agent"),
    stat("◕", "rgba(22,163,74,.15)", n.online ? pct(n.ram) : "–", "RAM", "reported by agent"),
    stat("⬢", "rgba(59,130,246,.15)", n.models.length, "Models", mods.length ? bytes(sumSize(mods)) + " on disk" : "installed on this node"),
    stat("◷", "rgba(107,114,128,.2)", ago(n.last_seen), "Last heartbeat", new Date(n.last_seen * 1000).toLocaleTimeString()));

  const cl = $("#clegend"); clear(cl); cl.append(cpuRamLegend());
  const ch = $("#chart"); clear(ch); ch.append(cpuRam(MC.nodeHist[NODE] || []));

  const rows = mods.sort((a, b) => a.name.localeCompare(b.name)).map(m => [
    A(modelUrl(m.name), m.name, "b"), m.family || "–", m.parameter_size || "–", m.quantization_level || "–", bytes(m.size), dateCell(m.modified_at),
    s === "online" ? pill("ok", "on-d", "Routable") : statePill(s)]);
  const mt = $("#models"); clear(mt);
  mt.append(tbl(["Model", "Family", "Parameters", "Quantization", "Size", "Downloaded", "Status"], rows,
    s === "provider_unreachable" ? "Ollama cannot be reached, so the model list is unavailable." : "No models installed. Use “Pull a model” to download one."));

  const d = $("#details"); clear(d);
  d.append(kv([["Status", nodePill(n)], ["Ollama URL", n.ollama_url], ["Ollama reachable from LAN", n.ollama_lan_reachable ? "Yes (as reported by the agent)" : "No (as reported by the agent)"],
    ["Last seen", new Date(n.last_seen * 1000).toLocaleString()]]));
  $("#raw").textContent = JSON.stringify(n, null, 2);
}
MC.start(render);
