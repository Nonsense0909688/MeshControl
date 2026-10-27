"use strict";
/* Model pull helpers shared by the Models and Model pages. Pulls run on the
   controller as background jobs, so they continue if this page is closed. */
const Pull = {
  rate: {},
  start: (node, model) => api("/api/nodes/" + enc(node) + "/pull", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ model }) }),
  cancel: id => api("/api/pulls/" + enc(id), { method: "DELETE" }),
  running: () => MC.pulls.filter(p => p.state === "running"),
  /* Nodes a model can currently be pulled to: heartbeat fresh and Ollama reachable. */
  targets: () => MC.nodes.filter(n => nodeState(n) === "online"),
  speed(job) {
    const now = Date.now() / 1000, r = this.rate[job.id];
    if (!r) { this.rate[job.id] = { t: now, c: job.completed, v: 0 }; return 0 }
    if (now - r.t >= 1 && job.completed >= r.c) { const inst = (job.completed - r.c) / (now - r.t); r.v = r.v ? r.v * 0.6 + inst * 0.4 : inst; r.t = now; r.c = job.completed }
    return r.v;
  },
  eta(s) { return s < 60 ? Math.ceil(s) + "s" : s < 3600 ? Math.ceil(s / 60) + " min" : (s / 3600).toFixed(1) + " h" },
  row(job) {
    const done = job.state !== "running", frac = job.total ? Math.min(1, job.completed / job.total) : 0;
    const badge = { running: pill("mut", "hi-d", "Pulling"), success: pill("ok", "on-d", "Complete"), error: pill("bad", "off-d", "Failed"), cancelled: pill("mut", "", "Cancelled") }[job.state];
    const title = add(el("div", "t"), el("b", "", job.model), badge, el("span", "empty", "on " + job.node));
    const bar = el("div", "prog" + (job.state === "success" ? " ok" : job.state === "error" ? " bad" : !job.total && !done ? " ind" : "")), fill = el("i");
    fill.style.width = (job.state === "success" ? 100 : Math.round(frac * 100)) + "%"; bar.append(fill);
    let info;
    if (job.state === "running") {
      const sp = this.speed(job), parts = [job.status || "starting"];
      if (job.total) parts.push(Math.floor(frac * 100) + "% · " + bytes(job.completed) + " / " + bytes(job.total));
      if (sp > 0) parts.push(bytes(sp) + "/s" + (job.total ? " · " + this.eta((job.total - job.completed) / sp) + " left" : ""));
      info = parts.join(" · ");
    } else if (job.state === "error") info = job.error || "The pull failed";
    else if (job.state === "success") info = "Downloaded in " + this.eta(job.finished_at - job.started_at) + ". It appears in the model list after the node's next heartbeat.";
    else info = "Cancelled before completing. Partial data is resumed if you pull it again.";
    const row = add(el("div", "pull"), title);
    if (!done) {
      const c = btn("Cancel", "sm", async () => { c.disabled = true; try { await Pull.cancel(job.id); await MC.loadPulls(); if (MC.render) MC.render() } catch (e) { c.disabled = false } });
      row.append(c);
    } else row.append(el("span"));
    return add(row, bar, el("small", "", info));
  },
  list(container, jobs) {
    clear(container);
    const shown = jobs.filter(j => j.state === "running").concat(jobs.filter(j => j.state !== "running").slice(0, 5));
    shown.forEach(j => container.append(this.row(j)));
    return shown.length;
  },
  /* Poll quickly while a pull runs; refresh the model list shortly after one completes. */
  watch(onChange) {
    let seen = new Set(this.running().map(p => p.id));
    setInterval(async () => {
      if (!seen.size && !this.running().length) return;
      await MC.loadPulls();
      const now = new Set(this.running().map(p => p.id));
      for (const id of seen) if (!now.has(id)) { const j = MC.pulls.find(p => p.id === id); if (j && j.state === "success") { setTimeout(MC.refresh, 800); setTimeout(MC.refresh, 6500) } }
      seen = now; onChange();
    }, 1500);
  },
};
