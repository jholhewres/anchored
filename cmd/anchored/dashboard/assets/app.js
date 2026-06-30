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
  return d.toLocaleString("pt-BR", { dateStyle: "short", timeStyle: "short" });
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
const tabs = document.querySelectorAll("nav.tabs button");
tabs.forEach((b) => b.addEventListener("click", () => {
  tabs.forEach((x) => x.classList.toggle("active", x === b));
  const name = b.dataset.tab;
  document.querySelectorAll("section.tab").forEach((s) => {
    s.classList.toggle("active", s.id === "tab-" + name);
  });
  const loaders = { overview: loadOverview, memories: loadMemories, kg: loadKG, system: loadSystem, dream: loadDream, artifacts: loadArtifacts, activity: loadActivity };
  if (loaders[name] && !loaders[name].loaded) loaders[name]();
}));

// ---------------- overview ----------------
const charts = {};
const PALETTE = ["#58a6ff", "#3fb950", "#d29922", "#f85149", "#bc8cff", "#39c5cf", "#ff7b72", "#7ee787", "#ffa657", "#a371f7"];

async function loadOverview() {
  loadOverview.loaded = true;
  try {
    const [stats, health] = await Promise.all([fetchJSON("/api/stats"), fetchJSON("/api/health")]);
    el("db-meta").textContent =
      `${stats.total_memories} memórias · ${fmtBytes(health.db_bytes)}`;
    renderOverviewCards(stats, health);
    renderCategoryChart(stats.by_category || {});
    renderProjectChart(stats.by_project || {});
    el("db-meta").textContent =
      `${stats.total_memories} memórias · ${fmtBytes(health.db_bytes)} · ${health.embedding_coverage?.toFixed(0)}% embed`;
  } catch (e) { el("overview-cards").innerHTML = `<p class="empty">erro: ${esc(e.message)}</p>`; }
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
      : `<span class="muted">sem keywords</span>`;
  } catch (_) { el("overview-keywords").innerHTML = `<span class="muted">indisponível</span>`; }
}

async function loadEntities() {
  try {
    const d = await fetchJSON("/api/entities?limit=40");
    const items = d.items || [];
    el("overview-entities").innerHTML = items.length
      ? items.map((e) => `<span class="kw ${e.degree >= 10 ? "big" : ""}">${esc(e.name)} <span class="muted">${e.degree}</span></span>`).join("")
      : `<span class="muted">sem entidades</span>`;
  } catch (_) { el("overview-entities").innerHTML = `<span class="muted">indisponível</span>`; }
}

function renderOverviewCards(stats, health) {
  const cats = Object.keys(stats.by_category || {}).length;
  const projs = Object.keys(stats.by_project || {}).length;
  const pct = health.embedding_coverage ?? 0;
  el("overview-cards").innerHTML = [
    card("Total de memórias", stats.total_memories ?? 0, `${cats} categorias · ${projs} projetos`),
    card("Projetos", projs, "detectados"),
    card("Cobertura embedding", pct.toFixed(0) + "%", `${health.memories?.with_embedding}/${health.memories?.total} com vetor`),
    card("Sync dirty", health.memories?.sync_dirty ?? 0, `último sync: ${fmtDate(health.sync?.last_sync_at)}`),
  ].join("");
}
const card = (label, value, sub, kind = "") =>
  `<div class="card ${kind}"><div class="label">${esc(label)}</div><div class="value">${esc(value)}</div><div class="sub">${esc(sub)}</div></div>`;

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
    el("mem-tbody").innerHTML = `<tr><td colspan="5" class="empty">erro: ${esc(e.message)}</td></tr>`;
    el("mem-count").textContent = "";
  }
}

function renderMemTable() {
  const tb = el("mem-tbody");
  if (!mem.rows.length) {
    tb.innerHTML = `<tr><td colspan="5" class="empty">nenhuma memória</td></tr>`;
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
  } catch (e) { showToast("erro: " + e.message, "err"); }
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
  if (!confirm("Deletar esta memória? (soft-delete — some das buscas, mas permanece no banco)")) return;
  if (!confirm("Confirma definitivamente?")) return;
  try {
    const r = await fetch("/api/memories/" + encodeURIComponent(id), { method: "DELETE" });
    if (r.status !== 204) throw new Error(await r.text());
    el("modal").classList.remove("open");
    showToast("memória deletada (restaurável na aba Sistema → Lixeira)", "ok");
    await fetchMemPage();
  } catch (e) { showToast("falha ao deletar: " + e.message, "err"); }
}
el("m-close").addEventListener("click", () => el("modal").classList.remove("open"));
el("modal").addEventListener("click", (e) => { if (e.target.id === "modal") el("modal").classList.remove("open"); });

// ---------------- export ----------------
document.querySelectorAll("button[data-export]").forEach((b) =>
  b.addEventListener("click", () => exportResults(b.dataset.export)));

function exportResults(fmt) {
  const rows = mem.rows;
  if (!rows.length) { showToast("nada para exportar — busque/liste primeiro", "err"); return; }
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
  showToast(`exportado ${rows.length} memórias (${ext})`, "ok");
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
      : `<tr><td colspan="5" class="empty">nenhuma memória deletada</td></tr>`;
    el("sys-trash").querySelectorAll("button[data-restore]").forEach((b) =>
      b.addEventListener("click", () => restoreMemory(b.dataset.restore)));
  } catch (_) { el("sys-trash").innerHTML = `<tr><td colspan="5" class="empty">indisponível</td></tr>`; }
}
async function restoreMemory(id) {
  if (!confirm("Restaurar esta memória? (remove o soft-delete e ela volta às buscas)")) return;
  try {
    const r = await fetch("/api/memories/" + encodeURIComponent(id) + "/restore", { method: "POST" });
    if (r.status !== 204) throw new Error(await r.text());
    showToast("memória restaurada", "ok");
    loadTrash();
  } catch (e) { showToast("falha ao restaurar: " + e.message, "err"); }
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
  } catch (e) { el("kg-network").innerHTML = `<div class="empty">erro: ${esc(e.message)}</div>`; }
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
      : `<tr><td colspan="5" class="empty">nenhuma ação</td></tr>`;
  } catch (e) { el("dream-cards").innerHTML = `<p class="empty">erro: ${esc(e.message)}</p>`; }
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
      : `<tr><td colspan="6" class="empty">nenhum artifact</td></tr>`;
  } catch (e) { el("art-cards").innerHTML = `<p class="empty">erro: ${esc(e.message)}</p>`; }
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
      card("Eventos de sessão", d.total ?? 0, "tool calls / erros / etc", "good"),
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
      : `<tr><td colspan="7" class="empty">nenhum import</td></tr>`;
  } catch (e) { el("act-cards").innerHTML = `<p class="empty">erro: ${esc(e.message)}</p>`; }
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
      card("Total memórias", health.memories?.total ?? 0, "ativas (não deletadas)", "good"),
      card("Com embedding", health.memories?.with_embedding ?? 0, `${pct.toFixed(0)}% do total`, covKind),
      card("DB (alocado)", fmtBytes(health.db_bytes), "páginas × page_size"),
      card("Sync dirty", dirty, `projetos sync: ${health.sync?.projects ?? 0}`, dirtyKind),
      card("Sessões", sessions.total ?? 0, `${sessions.active ?? 0} ativas`),
      card("Último sync", fmtDate(health.sync?.last_sync_at), `watermark: ${health.sync?.last_watermark || "—"}`),
    ].join("");
    el("sys-projects").innerHTML = (projects.items || []).map((p) =>
      `<tr><td>${esc(p.name)}</td><td class="muted">${esc(p.path)}</td><td class="muted">${esc(p.remote_key || "—")}</td><td>${p.memories}</td><td class="muted">${esc(fmtDate(p.last_activity))}</td></tr>`).join("")
      || `<tr><td colspan="5" class="empty">nenhum projeto</td></tr>`;
    el("sys-sessions").innerHTML = (sessions.recent || []).map((s) =>
      `<tr><td>${esc(preview(s.title, 50) || "(sem título)")}</td><td class="muted">${esc(s.directory || "—")}</td><td class="muted">${esc(s.source)}</td><td>${s.message_count}</td><td class="muted">${esc(fmtDate(s.last_activity_at))}</td></tr>`).join("")
      || `<tr><td colspan="5" class="empty">nenhuma sessão</td></tr>`;
  } catch (e) { el("system-health").innerHTML = `<p class="empty">erro: ${esc(e.message)}</p>`; }
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
