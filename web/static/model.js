"use strict";
/* Model detail: facts about one model and where each copy lives. */
const MODEL = decodeURIComponent(location.pathname.replace(/^\/models\//, "").replace(/\/+$/, ""));
document.title = "MeshControl · " + MODEL;
const withTag = s => s.includes(":") ? s : s + ":latest";

function renderPullForm(g) {
  const sel = $("#pn"), targets = Pull.targets(), want = sel.value;
  const sig = MC.nodes.map(n => n.node + ":" + nodeState(n) + ":" + g.nodes.includes(n.node)).join("|");
  if (sel.dataset.sig !== sig) {
    sel.dataset.sig = sig; clear(sel);
    MC.nodes.forEach(n => {
      const s = nodeState(n), has = g.nodes.includes(n.node);
      const o = new Option(n.node + (s !== "online" ? " — " + STATES[s].label.toLowerCase() : has ? " — installed" : ""), n.node);
      o.disabled = s !== "online"; sel.append(o);
    });
    const first = targets.find(n => !g.nodes.includes(n.node)) || targets[0];
    sel.value = targets.some(n => n.node === want) ? want : first ? first.node : "";
  }
  $("#pgo").disabled = !targets.length;
}

function render() {
  if (!MC.loaded) return;
  const g = modelGroups().find(x => x.name === MODEL), nf = $("#nf"), content = $("#content");
  const running = MC.pulls.filter(p => withTag(p.model) === withTag(MODEL));
  if (!g && !running.length) {
    content.hidden = true; nf.hidden = false; clear(nf);
    add(nf, card("Model not found", null, add(el("p", "empty"), T("“" + MODEL + "” is not installed on any node. "), A("/models?pull=1&model=" + enc(MODEL), "Pull it"), T(" or "), A("/models", "go back to models"), T("."))));
    return;
  }
  nf.hidden = true; content.hidden = false;
  const grp = g || { name: MODEL, copies: [], nodes: [], ok: 0, size: null, family: null, parameter_size: null, quantization_level: null, format: null, digest: null, modified_at: null };

  $("#crumbname").textContent = MODEL; $("#title").textContent = MODEL;
  const badge = $("#badge"); clear(badge); badge.append(g ? modelStatePill(grp) : pill("mut", "hi-d", "Downloading"));
  $("#sub").textContent = g ? "Installed on " + grp.nodes.length + " node" + (grp.nodes.length === 1 ? "" : "s") : "Being downloaded; not installed anywhere yet";
  $("#chatlink").href = "/chat?model=" + enc(MODEL); $("#chatlink").hidden = !grp.ok;

  const st = $("#stats"); clear(st);
  add(st,
    stat("#", "rgba(59,130,246,.15)", grp.parameter_size || "–", "Parameters", grp.family ? grp.family + " family" : "family unknown"),
    stat("◇", "rgba(234,179,8,.18)", grp.quantization_level || "–", "Quantization", grp.format ? grp.format.toUpperCase() + " format" : ""),
    stat("▣", "rgba(22,163,74,.15)", bytes(grp.size), "Size on disk", "per copy"),
    stat("⏻", "rgba(107,114,128,.2)", grp.ok + " / " + grp.nodes.length, "Routable copies", "online and reachable"));

  const rows = grp.copies.map(c => {
    const n = getNode(c.node);
    return [A(nodeUrl(c.node), c.node, "b"), n ? nodePill(n) : "–", bytes(c.size), dateCell(c.modified_at),
      add(el("span", "mono"), T(shortDigest(c.digest))), c.available ? "Yes" : "No",
      c.available ? A("/chat?model=" + enc(MODEL) + "&node=" + enc(c.node), "Test on this node") : T("–")];
  });
  const copies = $("#copies"); clear(copies);
  copies.append(tbl(["Node", "Node status", "Size", "Downloaded", "Digest", "Routable", ""], rows, "Not installed on any node yet."));
  const digests = new Set(grp.copies.map(c => c.digest).filter(Boolean));
  $("#digestnote").hidden = digests.size < 2;
  $("#digestnote").textContent = "These copies have different digests, so some nodes hold a different version of this tag. Pull again on the older ones to update them.";

  renderPullForm(grp);
  Pull.list($("#downloads"), running);

  const d = $("#details"); clear(d);
  d.append(kv([["Name", MODEL], ["Family", grp.family || "–"], ["Parameters", grp.parameter_size || "–"], ["Quantization", grp.quantization_level || "–"],
    ["Format", grp.format || "–"], ["Size", grp.size == null ? "–" : bytes(grp.size) + " (" + grp.size.toLocaleString() + " bytes)"], ["Digest", grp.digest || "–"],
    ["Last downloaded", grp.modified_at == null ? "–" : absDate(grp.modified_at) + " (" + ago(grp.modified_at) + ")"]]));

  const auth = MC.token ? ' -H "Authorization: Bearer $ADMIN_TOKEN"' : "";
  $("#curl").textContent = "curl " + location.origin + "/api/ollama/api/generate" + auth + " \\\n  -d '" + JSON.stringify({ model: MODEL, prompt: "Hello" }) + "'";
}

$("#pullform").addEventListener("submit", async e => {
  e.preventDefault();
  const node = $("#pn").value, err = $("#perr"); err.hidden = true;
  if (!node) return;
  $("#pgo").disabled = true;
  try { await Pull.start(node, MODEL); await MC.loadPulls(); render() }
  catch (ex) { err.textContent = ex.message; err.hidden = false }
  finally { $("#pgo").disabled = !Pull.targets().length }
});
Pull.watch(render);
MC.start(render);
