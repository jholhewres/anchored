// anchored dashboard — vanilla SPA. All data comes from the /api/* JSON endpoints
// served by the same binary; the vendored Chart.js and vis-network are embedded.

const el = (id) => document.getElementById(id);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => (
  { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

async function fetchJSON(url) {
  const r = await fetch(url);
  if (!r.ok) {
    let msg = r.status + " " + r.statusText;
    try { const j = await r.json(); if (j.error) msg = j.error; } catch (_) {}
    throw new Error(msg);
  }
  return r.json();
}

// toast + loading helpers
let toastTimer;
function showToast(msg, kind = "") {
  const t = el("toast");
  t.textContent = msg;
  t.className = "show " + kind;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = ""; }, 2600);
}
function withLoading(p) {
  el("loading").classList.add("show");
  return Promise.resolve(p).finally(() => el("loading").classList.remove("show"));
}

// keyboard shortcuts: "/" focuses search, "Esc" closes the modal.
document.addEventListener("keydown", (e) => {
  const tag = (e.target.tagName || "").toLowerCase();
  const typing = tag === "input" || tag === "textarea" || tag === "select";
  if (e.key === "/" && !typing) {
    e.preventDefault();
    document.querySelector('nav.tabs button[data-tab="memories"]').click();
    el("mem-search").focus();
  } else if (e.key === "Escape") {
    el("modal").classList.remove("open");
  }
});

const fmtDate = (s) => {
  if (!s) return "—";
  const d = new Date(s);
  if (isNaN(d)) return s;
  return d.toLocaleString("en-US", { dateStyle: "short", timeStyle: "short" });
};
const fmtBytes = (b) => {
  if (!b) return "—";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0, n = b;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i ? 1 : 0) + " " + u[i];
};
const preview = (s, n = 160) => (s && s.length > n ? s.slice(0, n) + "…" : s || "");

// ---------------- tabs ----------------
const TAB_TITLES = {
  overview: "Overview", cockpit: "Cockpit", memories: "Memories", tasks: "Tasks", kg: "Knowledge Graph",
  projects: "Projects", system: "System", dream: "Curation", artifacts: "Artifacts", activity: "Activity",
  connections: "Connectors",
};
const tabs = document.querySelectorAll("nav.tabs button");
tabs.forEach((b) => b.addEventListener("click", () => {
  tabs.forEach((x) => x.classList.toggle("active", x === b));
  const name = b.dataset.tab;
  document.querySelectorAll("section.tab").forEach((s) => {
    s.classList.toggle("active", s.id === "tab-" + name);
  });
  const title = el("view-title");
  if (title) title.textContent = TAB_TITLES[name] || name;
  const loaders = { overview: loadOverview, cockpit: loadCockpit, memories: loadMemories, tasks: loadTasks, kg: loadKG, projects: loadProjects, system: loadSystem, dream: loadDream, artifacts: loadArtifacts, activity: loadActivity, connections: loadConnections };
  if (loaders[name] && !loaders[name].loaded) loaders[name]();
}));

// setDbMeta mirrors the corpus summary into both the sidebar footer and the
// topbar (the redesign shows it in two places).
function setDbMeta(text) {
  ["db-meta", "db-meta-top"].forEach((id) => { const n = el(id); if (n) n.textContent = text; });
}

// ---------------- overview ----------------
const charts = {};
const PALETTE = ["#58a6ff", "#3fb950", "#d29922", "#f85149", "#bc8cff", "#39c5cf", "#ff7b72", "#7ee787", "#ffa657", "#a371f7"];

async function loadOverview() {
  loadOverview.loaded = true;
  try {
    const [stats, health, tokens, live] = await Promise.all([
      fetchJSON("/api/stats"),
      fetchJSON("/api/health"),
      fetchJSON("/api/tokens").catch(() => null),
      fetchJSON("/api/sessions/live").catch(() => null),
    ]);
    setDbMeta(`${stats.total_memories} memories · ${fmtBytes(health.db_bytes)}`);
    renderOverviewCards(stats, health, tokens, live);
    renderCategoryChart(stats.by_category || {});
    renderProjectChart(stats.by_project || {});
    setDbMeta(`${stats.total_memories} memories · ${fmtBytes(health.db_bytes)} · ${health.embedding_coverage?.toFixed(0)}% embed`);
  } catch (e) { el("overview-cards").innerHTML = `<p class="empty">error: ${esc(e.message)}</p>`; }
  loadTimeline();
  loadKeywords();
  loadEntities();
}

async function loadKeywords() {
  try {
    const d = await fetchJSON("/api/keywords?limit=40");
    const items = d.items || [];
    el("overview-keywords").innerHTML = items.length
      ? items.map((k) => `<span class="kw ${k.count >= 50 ? "big" : ""}">${esc(k.word)} <span class="muted">${k.count}</span></span>`).join("")
      : `<span class="muted">no keywords</span>`;
  } catch (_) { el("overview-keywords").innerHTML = `<span class="muted">unavailable</span>`; }
}

async function loadEntities() {
  try {
    const d = await fetchJSON("/api/entities?limit=40");
    const items = d.items || [];
    el("overview-entities").innerHTML = items.length
      ? items.map((e) => `<span class="kw ${e.degree >= 10 ? "big" : ""}">${esc(e.name)} <span class="muted">${e.degree}</span></span>`).join("")
      : `<span class="muted">no entities</span>`;
  } catch (_) { el("overview-entities").innerHTML = `<span class="muted">unavailable</span>`; }
}

function renderOverviewCards(stats, health, tokens, live) {
  const cats = Object.keys(stats.by_category || {}).length;
  const projs = Object.keys(stats.by_project || {}).length;
  const pct = health.embedding_coverage ?? 0;
  const cards = [
    card("Total memories", stats.total_memories ?? 0, `${cats} categories · ${projs} projects`),
    card("Projects", projs, "detected"),
    card("Embedding coverage", pct.toFixed(0) + "%", `${health.memories?.with_embedding}/${health.memories?.total} with vector`),
    card("Sync dirty", health.memories?.sync_dirty ?? 0, `last sync: ${fmtDate(health.sync?.last_sync_at)}`),
  ];
  // Live sessions card — fills the grid row and links straight to the cockpit.
  if (live) {
    const n = live.count || 0;
    const active = (live.sessions || []).filter((s) => s.state === "active").length;
    cards.push(card("Live sessions", n, n ? `${active} active · ${n - active} idle` : "none open", n ? "accent" : ""));
  }
  // Token savings (v0.13 recall telemetry). Requires both injections and a
  // non-zero baseline (a project with no CLAUDE.md/AGENTS.md/skills has no
  // baseline to compare against), so a fresh install never shows a hollow 0%.
  if (tokens && tokens.injections > 0 && tokens.baseline_tokens > 0) {
    const saved = Math.max(0, (tokens.baseline_tokens || 0) - (tokens.injected_tokens || 0));
    cards.push(card(
      "Tokens saved (7d)",
      fmtCount(saved),
      `${(tokens.savings_pct || 0).toFixed(0)}% · ${fmtCount(tokens.injected_tokens)} injected vs ${fmtCount(tokens.baseline_tokens)} baseline`,
      "accent",
    ));
  }
  el("overview-cards").innerHTML = cards.join("");
}
// fmtCount renders large counts compactly (1.6k, 412k).
const fmtCount = (n) => {
  n = Number(n) || 0;
  if (n < 1000) return String(n);
  if (n < 1e6) return (n / 1e3).toFixed(n < 1e4 ? 1 : 0) + "k";
  return (n / 1e6).toFixed(1) + "M";
};
const card = (label, value, sub, kind = "") =>
  `<div class="card ${kind}"><div class="label">${esc(label)}</div><div class="value">${esc(value)}</div><div class="sub">${esc(sub)}</div></div>`;

// ---------------- cockpit ----------------
const AGO = (s) => {
  if (!s) return "—";
  const secs = Math.max(0, (Date.now() - new Date(s).getTime()) / 1000);
  if (secs < 90) return Math.round(secs) + "s";
  if (secs < 5400) return Math.round(secs / 60) + "m";
  return Math.round(secs / 3600) + "h";
};
let liveSessions = [];
async function loadCockpit() {
  loadCockpit.loaded = true;
  const host = el("cockpit-list");
  host.innerHTML = `<p class="muted">loading…</p>`;
  try {
    const d = await fetchJSON("/api/sessions/live");
    liveSessions = d.sessions || [];
    if (!liveSessions.length) {
      host.innerHTML = `<p class="empty">No live sessions. Open Claude Code, Cursor, Codex… and it shows up here.</p>`;
      return;
    }
    host.innerHTML = liveSessions.map(cockpitCard).join("");
  } catch (e) {
    host.innerHTML = `<p class="empty">error: ${esc(e.message)}</p>`;
  }
}
// Tool avatar: two initials + a stable-ish color per tool family.
const TOOL_AVATAR = {
  "claude-code": { i: "CC", c: "#d97757" }, codex: { i: "CX", c: "#10a37f" },
  cursor: { i: "CU", c: "#3aa0ff" }, gemini: { i: "GM", c: "#8b6ef2" },
  opencode: { i: "OC", c: "#39c5cf" }, windsurf: { i: "WS", c: "#3fb950" },
};
function cockpitCard(s) {
  const live = s.state === "active";
  const av = TOOL_AVATAR[s.tool] || { i: (s.tool || "S").slice(0, 2).toUpperCase(), c: "#6b7684" };
  const evs = (s.recent_events || []).slice(0, 3).map((e) =>
    `<div class="ev"><span class="mono">${esc(e.event_type)}${e.tool_name ? " · " + esc(e.tool_name) : ""}</span>${e.summary ? " — " + esc(preview(e.summary, 72)) : ""}<span class="tm">${AGO(e.created_at)}</span></div>`
  ).join("") || `<div class="ev">no events yet</div>`;
  const prov = s.provider ? `<span class="prov">${esc(s.provider)}${s.model ? " · " + esc(s.model) : ""}</span>` : "";
  const task = s.task_key
    ? `<span class="chip live">◪ ${esc(s.task_key)}</span><span class="spacer"></span><button class="btn" data-cockpit="unlink" data-id="${esc(s.id)}">Unlink</button>`
    : `<button class="btn" data-cockpit="link" data-id="${esc(s.id)}">+ Link task</button><button class="btn" data-cockpit="promote" data-id="${esc(s.id)}">New task</button>`;
  return `<div class="sc ${live ? "act" : ""}" data-id="${esc(s.id)}" data-cockpit-card>
    <div class="sc-h">
      <span class="avatar" style="background:${av.c}">${esc(av.i)}</span>
      <span class="tool">${esc(s.tool || "session")}</span>${prov}
      <span class="st"><span class="dot ${live ? "live pulse" : "idle"}"></span> ${live ? "active" : "idle"} · ${AGO(s.last_activity_at)}</span>
    </div>
    <div class="sc-b">
      <div class="proj">${esc(s.project_id ? projectLabel(s.project_id) : "—")}${s.intent ? `<span class="intent">${esc(s.intent)}</span>` : ""}</div>
      <div class="evs">${evs}</div>
    </div>
    <div class="sc-f">${task}</div>
  </div>`;
}
async function cockpitWrite(url, opts) {
  const r = await fetch(url, opts);
  if (!r.ok) {
    let msg = r.status + " " + r.statusText;
    try { const j = await r.json(); if (j.error) msg = j.error; } catch (_) {}
    throw new Error(msg);
  }
  return r.json();
}
async function unlinkSession(id) {
  try {
    await cockpitWrite("/api/sessions/" + encodeURIComponent(id) + "/link", { method: "DELETE" });
    showToast("session unlinked", "ok");
    loadCockpit();
  } catch (e) { showToast("unlink failed: " + e.message, "err"); }
}
document.addEventListener("click", (e) => {
  if (e.target && e.target.id === "cockpit-refresh") { loadCockpit(); return; }
  const btn = e.target && e.target.closest ? e.target.closest("[data-cockpit]") : null;
  if (!btn) return;
  const id = btn.dataset.id;
  if (btn.dataset.cockpit === "link" || btn.dataset.cockpit === "promote") openCockpitLink(id);
  else if (btn.dataset.cockpit === "unlink") unlinkSession(id);
});

// ---- cockpit link/create-task modal (replaces prompt/alert) ----
let cmSessionId = null;
async function openCockpitLink(sessionId) {
  cmSessionId = sessionId;
  const s = liveSessions.find((x) => x.id === sessionId);
  el("cm-session").innerHTML = s
    ? `<span class="chip">${esc(s.tool || "session")}</span> ${esc(s.project_id ? projectLabel(s.project_id) : "—")}${s.intent ? ` · ${esc(s.intent)}` : ""}`
    : "";
  el("cm-search").value = "";
  el("cm-newkey").value = "";
  el("cockpit-modal").classList.add("open");
  el("cm-list").innerHTML = `<div class="muted" style="padding:12px;text-align:center">loading tasks…</div>`;
  // The board's task list may be empty if Tasks wasn't opened yet — fetch it.
  try {
    const d = await fetchJSON("/api/tasks?limit=1000");
    tasks = d.items || [];
  } catch (_) { /* keep whatever we have */ }
  paintCmList("");
  setTimeout(() => el("cm-search").focus(), 30);
}
function closeCockpitModal() { el("cockpit-modal").classList.remove("open"); cmSessionId = null; }
function paintCmList(q) {
  q = (q || "").trim().toLowerCase();
  const open = (tasks || []).filter((t) => t.status !== "done" && t.status !== "cancelled");
  const match = (t) => !q || (t.task_key || "").toLowerCase().includes(q) ||
    (t.external_ref || "").toLowerCase().includes(q) ||
    (t.project_names || []).some((n) => (n || "").toLowerCase().includes(q));
  const rows = open.filter(match).slice(0, 30);
  el("cm-list").innerHTML = rows.length
    ? rows.map((t) => `<button class="cm-item" data-key="${esc(t.task_key)}">
        <span class="cm-key">${esc(t.task_key)}</span>
        <span class="cm-sub">${esc((t.project_names || []).map(shortName).join(", ") || t.external_ref || "")}</span>
        <span class="chip ${t.status === "active" ? "live" : ""}">${esc(t.status)}</span>
      </button>`).join("")
    : `<div class="muted" style="padding:12px;text-align:center">no matching tasks — create one below</div>`;
  el("cm-list").querySelectorAll(".cm-item").forEach((b) =>
    b.addEventListener("click", () => cmLink(b.dataset.key)));
}
async function cmLink(key) {
  try {
    await cockpitWrite("/api/sessions/" + encodeURIComponent(cmSessionId) + "/link",
      { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ task_key: key }) });
    showToast("linked to " + key, "ok");
    closeCockpitModal();
    loadCockpit();
  } catch (e) { showToast("link failed: " + e.message, "err"); }
}
async function cmCreate() {
  const key = el("cm-newkey").value.trim();
  try {
    const r = await cockpitWrite("/api/sessions/" + encodeURIComponent(cmSessionId) + "/promote-task",
      { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ key }) });
    showToast("created & linked " + (r.task_key || ""), "ok");
    closeCockpitModal();
    loadCockpit();
  } catch (e) { showToast("create failed: " + e.message, "err"); }
}
// wire the cockpit modal once
(function wireCockpitModal() {
  const search = el("cm-search");
  if (!search) return;
  search.addEventListener("input", () => paintCmList(search.value));
  el("cm-create").addEventListener("click", cmCreate);
  el("cm-newkey").addEventListener("keydown", (e) => { if (e.key === "Enter") cmCreate(); });
  el("cm-close").addEventListener("click", closeCockpitModal);
  el("cockpit-modal").addEventListener("click", (e) => { if (e.target.id === "cockpit-modal") closeCockpitModal(); });
})();

// ---------------- projects ----------------
async function loadProjects() {
  loadProjects.loaded = true;
  const tb = el("projects-tbody");
  tb.innerHTML = `<tr><td colspan="6" class="muted" style="padding:14px">loading…</td></tr>`;
  try {
    const [pd, live] = await Promise.all([
      fetchJSON("/api/projects"),
      fetchJSON("/api/sessions/live").catch(() => ({ sessions: [] })),
    ]);
    const items = pd.items || [];
    // count live sessions per project id
    const liveByProj = {};
    (live.sessions || []).forEach((s) => { if (s.project_id) liveByProj[s.project_id] = (liveByProj[s.project_id] || 0) + 1; });
    el("projects-sub").textContent =
      `${items.length} projects · ${items.filter((p) => p.remote_key).length} linked`;
    tb.innerHTML = items.length
      ? items.map((p) => {
        const link = p.remote_key
          ? `<span class="chip live">🔗 linked</span>`
          : `<span class="chip">⛓ local-only</span>`;
        const n = liveByProj[p.id] || 0;
        const liveCell = n ? `<span class="chip live">${n} open</span>` : `<span class="muted">—</span>`;
        return `<tr>
          <td><b>${esc(p.name || shortName(p.id))}</b></td>
          <td>${link}</td>
          <td class="mono">${p.memories ?? 0}</td>
          <td>${liveCell}</td>
          <td class="muted">${fmtDate(p.last_activity)}</td>
          <td class="mono muted" style="font-size:11px">${esc(p.path || "")}</td>
        </tr>`;
      }).join("")
      : `<tr><td colspan="6" class="empty">no projects yet</td></tr>`;
  } catch (e) {
    tb.innerHTML = `<tr><td colspan="6" class="empty">error: ${esc(e.message)}</td></tr>`;
  }
}
document.addEventListener("click", (e) => { if (e.target && e.target.id === "projects-refresh") loadProjects(); });

// ---------------- connections ----------------
async function loadConnections() {
  loadConnections.loaded = true;
  const host = el("connections-list");
  if (!host) return;
  host.innerHTML = `<p class="muted">loading…</p>`;
  try {
    const d = await fetchJSON("/api/connections");
    const hosts = d.hosts || [];
    const status = (h) => {
      if (h.registered) return { chip: "ok", label: "conectado" };
      if (h.installed) return { chip: "warn", label: "instalado — não registrado" };
      return { chip: "muted", label: "ausente" };
    };
    host.innerHTML = hosts.map((h) => {
      const s = status(h);
      const hint = h.installed && !h.registered
        ? `<code>anchored init --tool ${esc(h.name)}</code>`
        : "";
      return `<div class="conn-row">
        <span class="conn-name">${esc(h.name)}</span>
        <span class="chip ${s.chip}">${esc(s.label)}</span>
        <span class="conn-hint">${hint}</span>
      </div>`;
    }).join("") || `<p class="empty">no known hosts</p>`;
  } catch (e) {
    host.innerHTML = `<p class="empty">error: ${esc(e.message)}</p>`;
  }
}

function renderCategoryChart(byCat) {
  const entries = Object.entries(byCat).sort((a, b) => b[1] - a[1]);
  if (charts.cat) charts.cat.destroy();
  charts.cat = new Chart(el("chart-categories"), {
    type: "doughnut",
    data: {
      labels: entries.map((e) => e[0]),
      datasets: [{ data: entries.map((e) => e[1]), backgroundColor: PALETTE, borderColor: "#0d1117", borderWidth: 2 }],
    },
    options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { position: "right", labels: { color: "#8b949e" } } } },
  });
}

function renderProjectChart(byProj) {
  const entries = Object.entries(byProj).sort((a, b) => b[1] - a[1]).slice(0, 10);
  if (charts.proj) charts.proj.destroy();
  charts.proj = new Chart(el("chart-projects"), {
    type: "bar",
    data: {
      labels: entries.map((e) => projectLabel(e[0])),
      datasets: [{ data: entries.map((e) => e[1]), backgroundColor: "#58a6ff" }],
    },
    options: { indexAxis: "y", responsive: true, maintainAspectRatio: false, plugins: { legend: { display: false } }, scales: { x: { ticks: { color: "#8b949e" }, grid: { color: "#1c232c" } }, y: { ticks: { color: "#8b949e" }, grid: { color: "#1c232c" } } } },
  });
}
const shortName = (p) => (p ? p.split("/").filter(Boolean).pop() || p : "(global)");

// projectMap resolves the UUID project_id stored on memories to the readable
// {name, path, remote_key} row from the projects table. Populated at boot from
// /api/projects and reused across every view so the user sees names, not IDs.
const projectMap = new Map();
const projectLabel = (id) => {
  if (!id) return "(global)";
  const p = projectMap.get(id);
  return p && p.name ? p.name : shortName(id);
};
const projectPath = (id) => (projectMap.get(id) || {}).path || "";

async function loadTimeline() {
  const bucket = el("timeline-bucket").value;
  const stacked = el("timeline-stacked").checked;
  try {
    const data = await fetchJSON("/api/timeline?bucket=" + bucket + (stacked ? "&by_category=1" : ""));
    if (charts.time) charts.time.destroy();
    const xScale = { ticks: { color: "#8b949e", maxTicksLimit: 12 }, grid: { color: "#1c232c" } };
    const yScale = { beginAtZero: true, ticks: { color: "#8b949e" }, grid: { color: "#1c232c" } };
    if (stacked) {
      xScale.stacked = true; yScale.stacked = true;
      const periods = [...new Set(data.points.map((p) => p.period))].sort();
      const cats = [...new Set(data.points.map((p) => p.category))];
      const lookup = {}; data.points.forEach((p) => { lookup[p.period + "|" + p.category] = p.count; });
      const datasets = cats.map((c, i) => ({
        label: c, backgroundColor: PALETTE[i % PALETTE.length],
        data: periods.map((per) => lookup[per + "|" + c] || 0),
      }));
      charts.time = new Chart(el("chart-timeline"), {
        type: "bar", data: { labels: periods, datasets },
        options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { position: "right", labels: { color: "#8b949e" } } }, scales: { x: xScale, y: yScale } },
      });
    } else {
      charts.time = new Chart(el("chart-timeline"), {
        type: "line",
        data: { labels: data.points.map((p) => p.period), datasets: [{ data: data.points.map((p) => p.count), borderColor: "#3fb950", backgroundColor: "rgba(63,185,80,.15)", fill: true, tension: .3, pointRadius: 2 }] },
        options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { display: false } }, scales: { x: xScale, y: yScale } },
      });
    }
  } catch (e) { console.error(e); }
}
el("timeline-bucket").addEventListener("change", loadTimeline);
el("timeline-stacked").addEventListener("change", loadTimeline);

// ---------------- memories ----------------
const mem = { offset: 0, limit: 50, mode: "list", q: "", rows: [], order: "", dir: "desc" };

async function loadMemories() {
  // populate filter options once; project names come from projectMap (loaded at
  // boot from /api/projects) so the dropdowns show readable names.
  if (!loadMemories.populated) {
    loadMemories.populated = true;
    try {
      const stats = await fetchJSON("/api/stats");
      fillSelect(el("mem-category"), Object.keys(stats.by_category || {}));
      // value = project id (sent to the API as ?project=), label = readable name
      fillProjectOptions(el("mem-project"), Object.keys(stats.by_project || {}));
    } catch (_) {}
    fillProjectOptions(el("kg-project"), [...projectMap.keys()]);
  }
  if (!loadMemories.loaded) { loadMemories.loaded = true; await runMemQuery(); }
}
const fillSelect = (sel, vals) => {
  vals.forEach((v) => { const o = document.createElement("option"); o.value = v; o.textContent = v; sel.appendChild(o); });
};
const fillProjectOptions = (sel, ids) => {
  ids.forEach((id) => {
    const o = document.createElement("option");
    o.value = id;
    o.textContent = projectLabel(id);
    sel.appendChild(o);
  });
};

async function runMemQuery() {
  const q = el("mem-search").value.trim();
  mem.q = q;
  mem.mode = q ? "search" : "list";
  mem.offset = 0;
  await fetchMemPage();
}

async function fetchMemPage() {
  const params = new URLSearchParams({ limit: mem.limit, offset: mem.offset });
  let url;
  if (mem.mode === "search") {
    params.set("q", mem.q);
    const cat = el("mem-category").value; if (cat) params.set("category", cat);
    url = "/api/search?" + params;
  } else {
    const cat = el("mem-category").value; if (cat) params.set("category", cat);
    const proj = el("mem-project").value; if (proj) params.set("project", proj);
    if (el("mem-since").value) params.set("since", el("mem-since").value);
    if (el("mem-until").value) params.set("until", el("mem-until").value);
    if (mem.order) { params.set("order", mem.order); params.set("dir", mem.dir); }
    url = "/api/memories?" + params;
  }
  try {
    const data = await fetchJSON(url);
    mem.rows = data.items || [];
    renderMemTable();
  } catch (e) {
    el("mem-tbody").innerHTML = `<tr><td colspan="5" class="empty">error: ${esc(e.message)}</td></tr>`;
    el("mem-count").textContent = "";
  }
}

function renderMemTable() {
  const tb = el("mem-tbody");
  if (!mem.rows.length) {
    tb.innerHTML = `<tr><td colspan="5" class="empty">no memories</td></tr>`;
    el("mem-count").textContent = "";
    return;
  }
  tb.innerHTML = mem.rows.map((m) =>
    `<tr class="memrow" data-id="${esc(m.id)}">
       <td><span class="cat">${esc(m.category)}</span></td>
       <td class="content"><div class="preview">${esc(preview(m.content))}</div></td>
       <td class="muted" title="${esc(projectPath(m.project_id))}">${esc(projectLabel(m.project_id))}</td>
       <td class="muted">${esc(m.source || "—")}</td>
       <td class="muted">${esc(fmtDate(m.created_at))}</td>
     </tr>`).join("");
  tb.querySelectorAll("tr.memrow").forEach((tr) => tr.addEventListener("click", () => openMemory(tr.dataset.id)));
  const isSearch = mem.mode === "search";
  // Search returns a hybrid top-N (the ranker has a floor) and isn't paginated;
  // only the list/browse view paginates with offset+limit.
  if (isSearch) {
    el("mem-count").textContent = `${mem.rows.length} resultados`;
    el("mem-prev").disabled = true;
    el("mem-next").disabled = true;
  } else {
    const from = mem.offset + 1, to = mem.offset + mem.rows.length;
    el("mem-count").textContent = `${from}–${to}`;
    el("mem-prev").disabled = mem.offset === 0;
    el("mem-next").disabled = mem.rows.length < mem.limit;
  }
}

el("mem-go").addEventListener("click", runMemQuery);
el("mem-search").addEventListener("keydown", (e) => { if (e.key === "Enter") runMemQuery(); });
el("mem-clear").addEventListener("click", () => { el("mem-search").value = ""; el("mem-category").value = ""; el("mem-project").value = ""; runMemQuery(); });
el("mem-prev").addEventListener("click", () => { mem.offset = Math.max(0, mem.offset - mem.limit); fetchMemPage(); });
el("mem-next").addEventListener("click", () => { mem.offset += mem.limit; fetchMemPage(); });
// sortable headers (list mode only — search ranks by relevance)
document.querySelectorAll("#tab-memories th.sortable").forEach((th) => {
  th.addEventListener("click", () => {
    const col = th.dataset.order;
    if (mem.order === col) { mem.dir = mem.dir === "asc" ? "desc" : "asc"; } else { mem.order = col; mem.dir = "desc"; }
    if (mem.mode === "list") fetchMemPage();
  });
});

async function openMemory(id) {
  try {
    const m = await withLoading(fetchJSON("/api/memories/" + encodeURIComponent(id)));
    el("m-cat").innerHTML = `<span class="cat">${esc(m.category)}</span>`;
    el("m-meta").innerHTML = [
      ["id", m.id], ["projeto", m.project_id || "(global)"], ["origem", m.source || "—"],
      ["source id", m.source_id || "—"], ["autor", m.author || "—"],
      ["criado", fmtDate(m.created_at)], ["atualizado", fmtDate(m.updated_at)],
      ["último acesso", fmtDate(m.last_accessed)], ["acessos", m.access_count ?? 0],
      ["hash", m.content_hash ? m.content_hash.slice(0, 12) : "—"],
    ].map(([k, v]) => `<span>${esc(k)}</span><span>${esc(v)}</span>`).join("");
    // keywords as badges
    const kws = m.keywords || [];
    const kwHtml = kws.length
      ? `<div class="kw-row">${kws.map((k) => `<span class="cat">${esc(k)}</span>`).join("")}</div>`
      : "";
    // metadata pretty-printed (only if non-empty)
    const hasMeta = m.metadata != null && !(typeof m.metadata === "object" && !Array.isArray(m.metadata) && Object.keys(m.metadata).length === 0);
    const metaHtml = hasMeta
      ? `<div style="margin-top:10px"><div class="label">metadata</div><pre class="meta">${esc(JSON.stringify(m.metadata, null, 2))}</pre></div>`
      : "";
    currentMem = m;
    currentExtras = kwHtml + metaHtml;
    renderModalBody();
    el("modal").classList.add("open");
    el("m-delete").onclick = () => deleteMemory(id);
  } catch (e) { showToast("error: " + e.message, "err"); }
}

// renderModalBody draws the memory content either as rendered Markdown (when
// the toggle is on and marked+DOMPurify are loaded) or as escaped plain text.
// Re-runs on every toggle change so the user can compare without re-opening.
let currentMem = null;
let currentExtras = "";
function renderModalBody() {
  const c = (currentMem && currentMem.content) || "";
  const useMd = el("m-md").checked && window.marked && window.DOMPurify;
  const body = useMd ? window.DOMPurify.sanitize(window.marked.parse(c)) : esc(c);
  el("m-body").innerHTML = body + currentExtras;
  el("m-body").classList.toggle("markdown", useMd);
}
el("m-md").addEventListener("change", renderModalBody);

async function deleteMemory(id) {
  if (!confirm("Delete this memory? (soft-delete — hidden from search, kept in the DB)")) return;
  if (!confirm("Confirma definitivamente?")) return;
  try {
    const r = await fetch("/api/memories/" + encodeURIComponent(id), { method: "DELETE" });
    if (r.status !== 204) throw new Error(await r.text());
    el("modal").classList.remove("open");
    showToast("memory deleted (restore under System → Trash)", "ok");
    await fetchMemPage();
  } catch (e) { showToast("delete failed: " + e.message, "err"); }
}
el("m-close").addEventListener("click", () => el("modal").classList.remove("open"));
el("modal").addEventListener("click", (e) => { if (e.target.id === "modal") el("modal").classList.remove("open"); });

// ---------------- export ----------------
document.querySelectorAll("button[data-export]").forEach((b) =>
  b.addEventListener("click", () => exportResults(b.dataset.export)));

function exportResults(fmt) {
  const rows = mem.rows;
  if (!rows.length) { showToast("nothing to export — search/list first", "err"); return; }
  let content, mime, ext;
  if (fmt === "json") {
    content = JSON.stringify(rows, null, 2); mime = "application/json"; ext = "json";
  } else if (fmt === "csv") {
    const cols = ["id", "category", "content", "source", "project_id", "created_at"];
    const c = (s) => `"${String(s ?? "").replace(/"/g, '""')}"`;
    content = [cols.join(","), ...rows.map((r) => cols.map((k) => c(r[k])).join(","))].join("\n");
    mime = "text/csv"; ext = "csv";
  } else { // markdown
    content = rows.map((r) =>
      `## ${r.category} — ${fmtDate(r.created_at)}\n\n${r.content}\n\n` +
      `\`id: ${r.id}\` · projeto: ${projectLabel(r.project_id)} · origem: ${r.source || "—"}\n`
    ).join("\n---\n\n");
    mime = "text/markdown"; ext = "md";
  }
  download(content, `anchored-${mem.mode}.${ext}`, mime);
  showToast(`exported ${rows.length} memories (${ext})`, "ok");
}
function download(text, name, mime) {
  const blob = new Blob([text], { type: mime });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = name;
  a.click();
  URL.revokeObjectURL(a.href);
}

// ---------------- trash (soft-delete) + restore ----------------
async function loadTrash() {
  try {
    const d = await fetchJSON("/api/deleted?limit=100");
    const items = d.items || [];
    el("sys-trash").innerHTML = items.length
      ? items.map((m) => `<tr>
          <td><span class="cat">${esc(m.category)}</span></td>
          <td class="content"><div class="preview">${esc(preview(m.content))}</div></td>
          <td class="muted">${esc(projectLabel(m.project_id))}</td>
          <td class="muted">${esc(fmtDate(m.deleted_at))}</td>
          <td><button class="btn restore" data-restore="${esc(m.id)}">restaurar</button></td>
        </tr>`).join("")
      : `<tr><td colspan="5" class="empty">no deleted memories</td></tr>`;
    el("sys-trash").querySelectorAll("button[data-restore]").forEach((b) =>
      b.addEventListener("click", () => restoreMemory(b.dataset.restore)));
  } catch (_) { el("sys-trash").innerHTML = `<tr><td colspan="5" class="empty">unavailable</td></tr>`; }
}
async function restoreMemory(id) {
  if (!confirm("Restaurar esta memory? (remove o soft-delete e ela volta às buscas)")) return;
  try {
    const r = await fetch("/api/memories/" + encodeURIComponent(id) + "/restore", { method: "POST" });
    if (r.status !== 204) throw new Error(await r.text());
    showToast("memory restored", "ok");
    loadTrash();
  } catch (e) { showToast("restore failed: " + e.message, "err"); }
}

// ---------------- tasks (kanban) ----------------
const TASK_STATUSES = ["backlog", "active", "paused", "done", "cancelled"];
let tasks = [];
let tmMode = "create";   // "create" | "edit"
let tmKey = null;

async function loadTasks() {
  if (!loadTasks.wired) {
    loadTasks.wired = true;
    // populate the project picker in the task modal from the shared projectMap
    fillProjectOptions(el("tm-project"), [...projectMap.keys()]);
    el("task-new").addEventListener("click", () => openTaskModal(null));
    el("task-refresh").addEventListener("click", () => renderTasksBoard(true));
    el("task-find").addEventListener("input", () => paintBoard());
    el("task-show-closed").addEventListener("change", () => paintBoard());
    el("tm-close").addEventListener("click", closeTaskModal);
    el("task-modal").addEventListener("click", (e) => { if (e.target.id === "task-modal") closeTaskModal(); });
    el("tm-save").addEventListener("click", saveTask);
    el("tm-status-row").querySelectorAll("button[data-set]").forEach((b) =>
      b.addEventListener("click", () => changeTaskStatus(tmKey, b.dataset.set)));
    // drop zones — HTML5 native DnD, one listener set per column
    document.querySelectorAll("#kanban .kcards").forEach((zone) => {
      zone.addEventListener("dragover", (e) => { e.preventDefault(); zone.classList.add("drop-hover"); });
      zone.addEventListener("dragleave", () => zone.classList.remove("drop-hover"));
      zone.addEventListener("drop", (e) => {
        e.preventDefault();
        zone.classList.remove("drop-hover");
        const key = e.dataTransfer.getData("text/task-key");
        const status = zone.dataset.drop;
        if (key && status) changeTaskStatus(key, status);
      });
    });
  }
  await renderTasksBoard();
}

async function renderTasksBoard(loud) {
  try {
    const d = await withLoading(fetchJSON("/api/tasks?limit=1000"));
    tasks = d.items || [];
    paintBoard();
    if (loud) showToast(`${tasks.length} tasks`, "ok");
  } catch (e) {
    showToast("failed to load tasks: " + e.message, "err");
  }
}

// paintBoard (re)renders every column from the in-memory `tasks`, applying the
// current text filter. Kept separate from the fetch so filtering is instant.
const KANBAN_COL_CAP = 12; // cap cards per column; the rest collapse behind "+N more"
function paintBoard() {
  const q = el("task-find").value.trim().toLowerCase();
  const showClosed = el("task-show-closed") && el("task-show-closed").checked;
  const match = (t) => !q ||
    (t.task_key || "").toLowerCase().includes(q) ||
    (t.external_ref || "").toLowerCase().includes(q) ||
    (t.project_names || []).some((n) => n.toLowerCase().includes(q));
  const shown = tasks.filter(match);
  TASK_STATUSES.forEach((st) => {
    const col = document.querySelector(`#kanban .kcol[data-status="${st}"]`);
    const zone = document.querySelector(`#kanban .kcards[data-drop="${st}"]`);
    const rows = shown.filter((t) => t.status === st);
    document.querySelector(`.kcount[data-count="${st}"]`).textContent = rows.length;
    // Done/Cancelled are "closable": when empty (and not explicitly shown) they
    // collapse to a slim rail so the board stays focused on live work.
    if (col.classList.contains("closable")) {
      col.classList.toggle("collapsed", rows.length === 0 && !showClosed);
    }
    const capped = rows.slice(0, KANBAN_COL_CAP);
    const more = rows.length - capped.length;
    zone.innerHTML = rows.length
      ? capped.map(taskCardHTML).join("") + (more > 0 ? `<div class="kmore">+${more} more</div>` : "")
      : `<div class="muted" style="padding:10px;font-size:12px;text-align:center">—</div>`;
  });
  el("task-stats").textContent = `${shown.length}/${tasks.length} tasks`;
  // wire card interactions
  document.querySelectorAll("#kanban .tcard").forEach((c) => {
    c.addEventListener("click", () => openTaskModal(tasks.find((t) => t.task_key === c.dataset.key)));
    c.addEventListener("contextmenu", (e) => { e.preventDefault(); openTaskContextMenu(e, c.dataset.key); });
    c.addEventListener("dragstart", (e) => {
      e.dataTransfer.setData("text/task-key", c.dataset.key);
      e.dataTransfer.effectAllowed = "move";
      c.classList.add("dragging");
    });
    c.addEventListener("dragend", () => c.classList.remove("dragging"));
  });
}

// ---- right-click context menu for task cards ----
function closeCtxMenu() { const m = el("ctxmenu"); if (m) { m.hidden = true; m.innerHTML = ""; } }
document.addEventListener("click", closeCtxMenu);
document.addEventListener("scroll", closeCtxMenu, true);
function openTaskContextMenu(e, key) {
  const m = el("ctxmenu");
  const items = [
    { label: "Open details", act: () => openTaskModal(tasks.find((t) => t.task_key === key)) },
    { sep: true },
    { label: "→ Backlog", act: () => changeTaskStatus(key, "backlog") },
    { label: "→ Active", act: () => changeTaskStatus(key, "active") },
    { label: "→ Paused", act: () => changeTaskStatus(key, "paused") },
    { label: "→ Done", act: () => changeTaskStatus(key, "done") },
    { sep: true },
    { label: "Cancel task", danger: true, act: () => changeTaskStatus(key, "cancelled") },
  ];
  m.innerHTML = items.map((it, i) => it.sep
    ? `<div class="sep"></div>`
    : `<button data-i="${i}" class="${it.danger ? "danger" : ""}">${esc(it.label)}</button>`).join("");
  m.querySelectorAll("button[data-i]").forEach((b) => {
    b.addEventListener("click", (ev) => { ev.stopPropagation(); closeCtxMenu(); items[+b.dataset.i].act(); });
  });
  m.hidden = false;
  const vw = window.innerWidth, vh = window.innerHeight;
  m.style.left = Math.min(e.clientX, vw - 200) + "px";
  m.style.top = Math.min(e.clientY, vh - m.offsetHeight - 10) + "px";
}

function taskCardHTML(t) {
  const projs = (t.project_names || []).slice(0, 3)
    .map((n) => `<span class="tchip proj">${esc(shortName(n))}</span>`).join("");
  const extra = (t.project_names || []).length > 3 ? `<span class="tchip">+${t.project_names.length - 3}</span>` : "";
  const note = (t.journal || [])[0];
  const liveDot = t.live_session_id ? `<span class="tlive" title="a live session is working on this">● live</span>` : "";
  return `<div class="tcard" draggable="true" data-key="${esc(t.task_key)}" data-status="${esc(t.status)}">
    <div class="tkey">${esc(t.task_key)} ${liveDot}</div>
    ${t.external_ref ? `<div class="tref">${esc(t.external_ref)}</div>` : ""}
    <div class="tmeta">
      ${projs}${extra}
      ${t.journal_count ? `<span class="tchip">📓 ${t.journal_count}</span>` : ""}
      ${t.session_count ? `<span class="tchip">⎇ ${t.session_count}</span>` : ""}
    </div>
    ${note ? `<div class="tnote">${esc(preview(note, 90))}</div>` : ""}
  </div>`;
}

const STATUS_TINT = { backlog: "var(--purple)", active: "var(--accent)", paused: "var(--warn)", done: "var(--accent2)", cancelled: "var(--danger)" };
function openTaskModal(task) {
  tmMode = task ? "edit" : "create";
  tmKey = task ? task.task_key : null;
  el("tm-title").textContent = task ? task.task_key : "New task";
  el("tm-key").value = task ? task.task_key : "";
  el("tm-key").readOnly = !!task;
  el("tm-ref").value = task ? (task.external_ref || "") : "";
  el("tm-project").value = "";
  el("tm-note").value = "";
  // status side panel + journal only in edit mode
  el("tm-side").hidden = !task;
  el("tm-journal-wrap").hidden = !task;
  const chip = el("tm-statuschip");
  if (task) {
    chip.hidden = false;
    chip.textContent = task.status;
    chip.style.color = STATUS_TINT[task.status] || "var(--text-dim)";
    chip.style.background = "color-mix(in srgb, " + (STATUS_TINT[task.status] || "var(--text-dim)") + " 16%, transparent)";
    el("tm-status-row").querySelectorAll("button[data-set]").forEach((b) =>
      b.classList.toggle("primary", b.dataset.set === task.status));
    const j = task.journal || [];
    el("tm-journal").innerHTML = j.length
      ? j.map((n) => `<div class="jentry">${esc(n)}</div>`).join("")
      : `<div class="muted" style="font-size:12px">no journal notes yet</div>`;
    el("tm-meta").innerHTML = `created ${fmtDate(task.created_at)}<br>updated ${fmtDate(task.updated_at)}`;
  } else {
    chip.hidden = true;
    el("tm-meta").textContent = "";
  }
  el("task-modal").classList.add("open");
  if (!task) setTimeout(() => el("tm-key").focus(), 30);
}
function closeTaskModal() { el("task-modal").classList.remove("open"); }

async function saveTask() {
  const key = el("tm-key").value.trim();
  const ref = el("tm-ref").value.trim();
  const note = el("tm-note").value.trim();
  const proj = el("tm-project").value;
  if (!key) { showToast("enter a task key", "err"); return; }
  try {
    if (tmMode === "create") {
      const body = { task_key: key, external_ref: ref };
      if (note) body.journal_note = note;
      if (proj) body.project_ids = [proj];
      const r = await fetch("/api/tasks", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
      if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.status);
      showToast("task created", "ok");
    } else {
      const body = {};
      if (ref !== "") body.external_ref = ref;
      if (note) body.journal_note = note;
      if (proj) body.project_id = proj;
      await patchTask(tmKey, body);
      showToast("task updated", "ok");
    }
    closeTaskModal();
    await renderTasksBoard();
  } catch (e) { showToast("save failed: " + e.message, "err"); }
}

async function changeTaskStatus(key, status) {
  if (!key) return;
  try {
    await patchTask(key, { status });
    // reflect immediately without a full refetch flicker, then reconcile
    const t = tasks.find((x) => x.task_key === key);
    if (t) t.status = status;
    paintBoard();
    if (el("task-modal").classList.contains("open") && tmKey === key) {
      const fresh = await fetchJSON("/api/tasks/" + encodeURIComponent(key));
      const i = tasks.findIndex((x) => x.task_key === key);
      if (i >= 0) tasks[i] = fresh;
      openTaskModal(fresh);
    }
    showToast(`→ ${status}`, "ok");
  } catch (e) { showToast("falha ao mover: " + e.message, "err"); renderTasksBoard(); }
}

async function patchTask(key, body) {
  const r = await fetch("/api/tasks/" + encodeURIComponent(key), {
    method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
  });
  if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.status);
  return r.json();
}

// ---------------- knowledge graph ----------------
let kgNetwork = null;
let kgTriples = [];
async function loadKG() {
  if (!loadKG.loaded) { loadKG.loaded = true; await renderKG(); }
}
async function renderKG() {
  const proj = el("kg-project").value;
  const limit = parseInt(el("kg-limit").value, 10) || 300;
  const url = "/api/kg?limit=" + limit + (proj ? "&project=" + encodeURIComponent(proj) : "");
  try {
    const data = await fetchJSON(url);
    const triples = data.triples || [];
    kgTriples = triples;
    el("kg-stats").textContent = `${triples.length} relações`;
    if (!triples.length) { if (kgNetwork) kgNetwork.destroy(); el("kg-network").innerHTML = `<div class="empty">sem relações no knowledge graph</div>`; return; }

    const degrees = data.degrees || {};
    const nodeSet = new Map();
    triples.forEach((t) => {
      [t.subject, t.object].forEach((n) => { if (!nodeSet.has(n)) nodeSet.set(n, degrees[n] || 1); });
    });
    const nodes = [...nodeSet.entries()].map(([id, deg], i) => ({
      id, label: id.length > 24 ? id.slice(0, 22) + "…" : id,
      value: deg, color: { background: PALETTE[i % PALETTE.length], border: "#0d1117" },
      font: { color: "#e6edf3", size: 13 },
    }));
    const edges = triples.map((t, i) => ({
      from: t.subject, to: t.object, label: t.predicate,
      arrows: "to", font: { color: "#8b949e", size: 10, strokeWidth: 0, background: "#161b22" },
      color: { color: "#2a313a" }, id: i,
    }));
    if (kgNetwork) kgNetwork.destroy();
    const vis = window.vis || (window.vis = {});
    kgNetwork = new vis.Network(el("kg-network"),
      { nodes: new vis.DataSet(nodes), edges: new vis.DataSet(edges) },
      { physics: { stabilization: { iterations: 200 } }, nodes: { shape: "dot", scaling: { min: 8, max: 40 } }, interaction: { hover: true, tooltipDelay: 120 } });
    // click a node → report its relations (vis already highlights it + its edges)
    kgNetwork.on("selectNode", (params) => {
      const id = params.nodes[0];
      const rels = triples.filter((t) => t.subject === id || t.object === id);
      el("kg-stats").innerHTML = `<strong>${esc(id)}</strong> — ${rels.length} relação(ões) <span class="muted">(clique no vazio p/ limpar)</span>`;
    });
    kgNetwork.on("deselectNode", () => { el("kg-stats").textContent = `${triples.length} relações`; });
  } catch (e) { el("kg-network").innerHTML = `<div class="empty">error: ${esc(e.message)}</div>`; }
}
el("kg-go").addEventListener("click", renderKG);
el("kg-find-go").addEventListener("click", () => {
  const q = el("kg-find").value.trim().toLowerCase();
  if (!q || !kgNetwork) { showToast("carregue o grafo primeiro", "err"); return; }
  const nodes = kgNetwork.body.data.nodes.get();
  const match = nodes.find((n) => (n.label || n.id).toLowerCase().includes(q));
  if (match) { kgNetwork.selectNodes([match.id]); kgNetwork.focus(match.id, { scale: 1.3, animation: true }); showToast("nó: " + match.id, "ok"); }
  else showToast("nó não encontrado", "err");
});
el("kg-export").addEventListener("click", () => {
  if (!kgTriples.length) { showToast("carregue o grafo primeiro", "err"); return; }
  download(JSON.stringify(kgTriples, null, 2), "anchored-kg.json", "application/json");
  showToast(`exportado ${kgTriples.length} relações`, "ok");
});

// ---------------- system ----------------
async function loadSystem() {
  if (!loadSystem.loaded) { loadSystem.loaded = true; await renderSystem(); loadTrash(); }
}
el("trash-refresh").addEventListener("click", loadTrash);

// ---------------- dream (consolidação) ----------------
async function loadDream() {
  if (!loadDream.loaded) { loadDream.loaded = true; await renderDream(); }
}
async function renderDream() {
  try {
    const d = await withLoading(fetchJSON("/api/dream?limit=100"));
    const last = d.last_run || {};
    const st = d.by_status || {};
    el("dream-cards").innerHTML = [
      card("Runs de consolidação", d.total_runs ?? 0, "execuções do dream", "good"),
      card("Ações propostas", d.total_actions ?? 0, `${st.applied || 0} aplicadas · ${st.proposed || 0} pendentes`),
      card("Última análise", last.memories_analyzed ?? 0, `status: ${last.status || "—"}`),
      card("Última run", fmtDate(last.started_at), `término: ${fmtDate(last.finished_at)}`),
    ].join("");
    el("dream-bytype").innerHTML = Object.keys(d.by_type || {}).length
      ? Object.entries(d.by_type).sort((a, b) => b[1] - a[1]).map(([t, c]) => `<span class="kw">${esc(t)} <span class="muted">${c}</span></span>`).join("")
      : `<span class="muted">sem ações</span>`;
    const actions = d.recent || [];
    el("dream-actions").innerHTML = actions.length
      ? actions.map((a) => `<tr>
          <td><span class="cat">${esc(a.action_type)}</span></td>
          <td class="muted">${esc(a.status)}</td>
          <td class="muted">${((a.confidence || 0) * 100).toFixed(0)}%</td>
          <td class="content"><div class="preview">${esc(preview(a.reason, 200))}</div></td>
          <td class="muted">${esc(fmtDate(a.proposed_at))}</td>
        </tr>`).join("")
      : `<tr><td colspan="5" class="empty">no actions</td></tr>`;
  } catch (e) { el("dream-cards").innerHTML = `<p class="empty">error: ${esc(e.message)}</p>`; }
}

// ---------------- artifacts + chunks ----------------
async function loadArtifacts() {
  if (!loadArtifacts.loaded) { loadArtifacts.loaded = true; await renderArtifacts(); }
}
async function renderArtifacts() {
  try {
    const type = el("art-type").value;
    const d = await withLoading(fetchJSON("/api/artifacts?limit=100" + (type ? "&type=" + encodeURIComponent(type) : "")));
    const byType = d.by_type || {};
    if (!el("art-type").dataset.filled) {
      el("art-type").dataset.filled = "1";
      Object.keys(byType).sort().forEach((t) => {
        const o = document.createElement("option"); o.value = t; o.textContent = `${t} (${byType[t].count})`; el("art-type").appendChild(o);
      });
    }
    const total = Object.values(byType).reduce((s, a) => s + a.count, 0);
    const bytes = Object.values(byType).reduce((s, a) => s + a.bytes, 0);
    el("art-cards").innerHTML = [
      card("Artifacts", total, fmtBytes(bytes), "good"),
      ...Object.entries(byType).sort((a, b) => b[1].count - a[1].count).slice(0, 4).map(([t, a]) => card(t, a.count, fmtBytes(a.bytes))),
    ].join("");
    const items = d.recent || [];
    el("art-tbody").innerHTML = items.length
      ? items.map((x) => `<tr>
          <td><span class="cat">${esc(x.type)}</span></td>
          <td>${esc(preview(x.source_label, 60) || "—")}</td>
          <td class="muted">${esc(x.source_tool || "—")}</td>
          <td class="muted">${fmtBytes(x.bytes)}</td>
          <td class="muted">${esc(fmtDate(x.created_at))}</td>
          <td class="muted">${esc(fmtDate(x.expires_at))}</td>
        </tr>`).join("")
      : `<tr><td colspan="6" class="empty">no artifact</td></tr>`;
  } catch (e) { el("art-cards").innerHTML = `<p class="empty">error: ${esc(e.message)}</p>`; }
  try {
    const c = await fetchJSON("/api/chunks");
    const t = Object.entries(c.by_type || {}).map(([k, v]) => `${esc(k)}:${v}`).join(" · ") || "—";
    const s = Object.entries(c.by_source || {}).map(([k, v]) => `${esc(k)}:${v}`).join(" · ") || "—";
    el("chunks-stats").innerHTML = `<strong>${c.total}</strong> chunks · por tipo: ${t}<br>por source: ${s}`;
  } catch (_) {}
}
el("art-go").addEventListener("click", renderArtifacts);

// ---------------- activity (events + imports) ----------------
async function loadActivity() {
  if (!loadActivity.loaded) { loadActivity.loaded = true; await renderActivity(); }
}
async function renderActivity() {
  try {
    const [d, imp] = await Promise.all([
      withLoading(fetchJSON("/api/events?limit=80")),
      fetchJSON("/api/imports"),
    ]);
    el("act-cards").innerHTML = [
      card("Session events", d.total ?? 0, "tool calls / errors / etc", "good"),
      card("Top tool", (d.top_tools || [])[0]?.tool || "—", `${(d.top_tools || [])[0]?.count || 0} eventos`),
    ].join("");
    el("act-toptools").innerHTML = (d.top_tools || []).length
      ? d.top_tools.map((t) => `<span class="kw ${t.count >= 100 ? "big" : ""}">${esc(t.tool)} <span class="muted">${t.count}</span></span>`).join("")
      : `<span class="muted">sem tools</span>`;
    const evs = d.recent || [];
    el("act-events").innerHTML = evs.length
      ? evs.map((e) => `<tr>
          <td><span class="cat">${esc(e.event_type)}</span></td>
          <td class="muted">${esc(e.tool_name || "—")}</td>
          <td class="content"><div class="preview">${esc(preview(e.summary, 160))}</div></td>
          <td class="muted">${esc(fmtDate(e.created_at))}</td>
        </tr>`).join("")
      : `<tr><td colspan="4" class="empty">sem eventos</td></tr>`;
    const imps = imp.items || [];
    el("act-imports").innerHTML = imps.length
      ? imps.map((i) => `<tr>
          <td><span class="cat">${esc(i.source)}</span></td>
          <td class="muted">${esc(preview(i.path, 50))}</td>
          <td class="muted">${esc(i.status)}</td>
          <td>${i.memories}</td>
          <td>${i.entities}</td>
          <td class="muted">${esc(fmtDate(i.started_at))}</td>
          <td class="muted">${esc(fmtDate(i.finished_at))}</td>
        </tr>`).join("")
      : `<tr><td colspan="7" class="empty">no import</td></tr>`;
  } catch (e) { el("act-cards").innerHTML = `<p class="empty">error: ${esc(e.message)}</p>`; }
}
async function renderSystem() {
  try {
    const [health, projects, sessions] = await Promise.all([
      fetchJSON("/api/health"), fetchJSON("/api/projects"), fetchJSON("/api/sessions"),
    ]);
    const pct = health.embedding_coverage ?? 0;
    const covKind = pct >= 80 ? "good" : pct >= 50 ? "" : pct >= 20 ? "warn" : "bad";
    const dirty = health.memories?.sync_dirty ?? 0;
    const dirtyKind = dirty === 0 ? "good" : dirty > 500 ? "warn" : "";
    el("system-health").innerHTML = [
      card("Total memorys", health.memories?.total ?? 0, "ativas (não deletadas)", "good"),
      card("Com embedding", health.memories?.with_embedding ?? 0, `${pct.toFixed(0)}% do total`, covKind),
      card("DB (alocado)", fmtBytes(health.db_bytes), "páginas × page_size"),
      card("Sync dirty", dirty, `projetos sync: ${health.sync?.projects ?? 0}`, dirtyKind),
      card("Sessões", sessions.total ?? 0, `${sessions.active ?? 0} ativas`),
      card("Último sync", fmtDate(health.sync?.last_sync_at), `watermark: ${health.sync?.last_watermark || "—"}`),
    ].join("");
    el("sys-projects").innerHTML = (projects.items || []).map((p) =>
      `<tr><td>${esc(p.name)}</td><td class="muted">${esc(p.path)}</td><td class="muted">${esc(p.remote_key || "—")}</td><td>${p.memories}</td><td class="muted">${esc(fmtDate(p.last_activity))}</td></tr>`).join("")
      || `<tr><td colspan="5" class="empty">no projeto</td></tr>`;
    el("sys-sessions").innerHTML = (sessions.recent || []).map((s) =>
      `<tr><td>${esc(preview(s.title, 50) || "(sem título)")}</td><td class="muted">${esc(s.directory || "—")}</td><td class="muted">${esc(s.source)}</td><td>${s.message_count}</td><td class="muted">${esc(fmtDate(s.last_activity_at))}</td></tr>`).join("")
      || `<tr><td colspan="5" class="empty">no sessions</td></tr>`;
  } catch (e) { el("system-health").innerHTML = `<p class="empty">error: ${esc(e.message)}</p>`; }
}

// boot — populate the project id→name map first so every view shows readable
// project names instead of raw UUIDs, then render the default tab.
(async () => {
  try {
    const p = await fetchJSON("/api/projects");
    (p.items || []).forEach((x) => projectMap.set(x.id, x));
  } catch (_) { /* map stays empty; views fall back to shortName(id) */ }
  loadOverview();
})();
