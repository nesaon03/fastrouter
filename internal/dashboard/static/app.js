// FastRouter dashboard: a small Preact + htm SPA (no build step).
// Structure and look follow 9Router's dashboard (MIT, decolua/9router).
import { html, render, useState, useEffect, useCallback } from "./vendor/preact-htm.js";

/* ------------------------------------------------------------------ api */
let csrf = "";
let onUnauthorized = () => {};
async function api(method, path, body) {
  const opt = { method, credentials: "same-origin", headers: {} };
  if (method !== "GET") opt.headers["X-CSRF-Token"] = csrf;
  if (body !== undefined) { opt.headers["Content-Type"] = "application/json"; opt.body = JSON.stringify(body); }
  const res = await fetch(path, opt);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (e) { data = { error: text.trim().slice(0, 300) }; }
  if (res.status === 401 && !path.startsWith("/api/auth/")) onUnauthorized();
  if (!res.ok) throw new Error((data && (data.error || data.message)) || res.statusText);
  return data;
}

/* ------------------------------------------------------------- toasts */
let pushToast = () => {};
const toast = (msg, type = "success") => pushToast({ msg, type, id: Math.random() });
function Toasts() {
  const [list, setList] = useState([]);
  pushToast = (t) => { setList((l) => [...l, t]); setTimeout(() => setList((l) => l.filter((x) => x.id !== t.id)), 3200); };
  return html`<div class="toasts">${list.map((t) => html`
    <div class="toast ${t.type}" key=${t.id}><span class="ms">${t.type === "error" ? "error" : "check_circle"}</span><span>${t.msg}</span></div>`)}</div>`;
}
async function run(fn, ok) {
  try { const r = await fn(); if (ok) toast(ok); return r; } catch (e) { toast(e.message, "error"); throw e; }
}

/* ------------------------------------------------------- provider meta */
// Display names, colours and logos from 9Router's provider registry.
const PROVIDERS = {
  deepseek: { name: "DeepSeek", color: "#4D6BFE", key: "https://platform.deepseek.com/api_keys" },
  openrouter: { name: "OpenRouter", color: "#F97316", key: "https://openrouter.ai/keys" },
  "opencode-go": { name: "OpenCode Go", color: "#E87040" },
  "xiaomi-tokenplan": { name: "Xiaomi MiMo (Token Plan)", color: "#FF6700" },
  openai: { name: "OpenAI", color: "#10A37F", key: "https://platform.openai.com/api-keys" },
  gemini: { name: "Gemini", color: "#4285F4", key: "https://aistudio.google.com/apikey" },
  nvidia: { name: "NVIDIA NIM", color: "#76B900", key: "https://build.nvidia.com" },
  xai: { name: "xAI (Grok)", color: "#1DA1F2", key: "https://console.x.ai" },
  anthropic: { name: "Anthropic", color: "#D97757", key: "https://console.anthropic.com/settings/keys" },
  codex: { name: "OpenAI Codex", color: "#3B82F6" },
  glm: { name: "Zai GLM Coding", color: "#2563EB" },
  antigravity: { name: "Antigravity", color: "#F59E0B" },
  cursor: { name: "Cursor IDE", color: "#00D4AA" },
  opencode: { name: "OpenCode Free", color: "#E87040" },
};
function provMeta(p) {
  const m = PROVIDERS[p.id];
  if (m) return { ...m, logo: `logos/${p.id}.png` };
  if (p.kind === "anthropic-compatible") return { name: p.name, color: "#D97757", logo: "logos/anthropic-m.png" };
  return { name: p.name, color: "#6366F1", logo: p.apiType === "responses" ? "logos/oai-r.png" : "logos/oai-cc.png" };
}
function Logo({ p, lg }) {
  const m = provMeta(p);
  const [bad, setBad] = useState(false);
  return html`<div class="logo ${lg ? "lg" : ""}" style=${`background:${m.color}18`}>
    ${bad ? html`<div class="logo-fallback" style=${`background:${m.color}`}>${m.name.slice(0, 2).toUpperCase()}</div>`
          : html`<img src=${m.logo} alt="" onError=${() => setBad(true)} />`}</div>`;
}

/* -------------------------------------------------------- primitives */
const Icon = ({ n, fill, cls = "", style }) => html`<span class="ms ${fill ? "fill" : ""} ${cls}" style=${style} aria-hidden="true">${n}</span>`;
const Toggle = ({ on, onChange, sm, disabled, title }) =>
  html`<button type="button" class="toggle ${on ? "on" : ""} ${sm ? "sm" : ""}" role="switch" aria-checked=${on} title=${title}
    disabled=${disabled} onClick=${(e) => { e.preventDefault(); e.stopPropagation(); onChange(!on); }}></button>`;
const Badge = ({ kind = "", dot, icon, children }) =>
  html`<span class="badge ${kind}">${dot && html`<span class="dot"></span>`}${icon && html`<${Icon} n=${icon} />`}${children}</span>`;
function Seg({ value, options, onChange, disabled }) {
  return html`<div class="seg">${options.map(([v, l]) => html`
    <button type="button" class=${value === v ? "on" : ""} disabled=${disabled} onClick=${() => onChange(v)}>${l}</button>`)}</div>`;
}
function Modal({ title, icon, onClose, children, foot, wide }) {
  useEffect(() => {
    const k = (e) => e.key === "Escape" && onClose();
    addEventListener("keydown", k); return () => removeEventListener("keydown", k);
  }, []);
  return html`<div class="overlay" onMouseDown=${(e) => e.target === e.currentTarget && onClose()}>
    <div class="modal ${wide ? "wide" : ""}" role="dialog" aria-modal="true">
      <div class="modal-head"><h3>${icon && html`<${Icon} n=${icon} style="color:var(--primary)" />`}${title}</h3>
        <button class="icon-btn" onClick=${onClose} aria-label="Close"><${Icon} n="close" /></button></div>
      <div class="modal-body">${children}</div>
      ${foot && html`<div class="modal-foot">${foot}</div>`}
    </div></div>`;
}
function Confirm({ title, text, danger = true, label = "Delete", onYes, onClose }) {
  const [busy, setBusy] = useState(false);
  return html`<${Modal} title=${title} icon="warning" onClose=${onClose}
    foot=${html`<button class="btn ghost" onClick=${onClose}>Cancel</button>
      <button class="btn ${danger ? "danger" : "primary"}" disabled=${busy} onClick=${async () => { setBusy(true); try { await onYes(); onClose(); } catch (e) { setBusy(false); } }}>${label}</button>`}>
    <p class="muted">${text}</p></${Modal}>`;
}
function Field({ label, hint, children }) {
  return html`<div class="field"><label>${label}</label>${children}${hint && html`<div class="hint">${hint}</div>`}</div>`;
}
function SecretInput({ value, onInput, placeholder, autofocus }) {
  const [show, setShow] = useState(false);
  return html`<div class="input-wrap"><input class="input mono" type=${show ? "text" : "password"} value=${value} placeholder=${placeholder}
      autocomplete="off" spellcheck="false" autofocus=${autofocus} onInput=${(e) => onInput(e.target.value)} />
    <button type="button" class="icon-btn" onClick=${() => setShow(!show)} aria-label="Show"><${Icon} n=${show ? "visibility_off" : "visibility"} /></button></div>`;
}
function useCopy() {
  const [copied, setCopied] = useState("");
  const copy = (text, id = "x") => {
    const done = () => { setCopied(id); setTimeout(() => setCopied(""), 1500); };
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, done);
    else { const t = document.createElement("textarea"); t.value = text; document.body.appendChild(t); t.select(); try { document.execCommand("copy"); } catch (e) {} t.remove(); done(); }
  };
  return [copied, copy];
}
function CopyBtn({ text, id, copied, copy, title = "Copy" }) {
  return html`<button class="icon-btn" title=${title} onClick=${() => copy(text, id)}><${Icon} n=${copied === id ? "check" : "content_copy"} /></button>`;
}
const fmt = (n) => (n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(n || 0));
const money = (n) => "$" + (n || 0).toFixed(n >= 10 ? 2 : 4);
function ago(ts) {
  if (!ts) return "never";
  const s = (Date.now() - new Date(ts).getTime()) / 1000;
  if (s < 60) return "just now"; if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago"; return Math.floor(s / 86400) + "d ago";
}

/* -------------------------------------------------------- reorderable */
// Drag-and-drop list (HTML5 DnD) with arrow buttons as a keyboard fallback.
function useDrag(items, onMove) {
  const [from, setFrom] = useState(-1);
  const [over, setOver] = useState(-1);
  const props = (i) => ({
    draggable: true,
    onDragStart: (e) => { setFrom(i); e.dataTransfer.effectAllowed = "move"; try { e.dataTransfer.setData("text/plain", String(i)); } catch (x) {} },
    onDragOver: (e) => { e.preventDefault(); if (over !== i) setOver(i); },
    onDragLeave: () => over === i && setOver(-1),
    onDrop: (e) => { e.preventDefault(); if (from >= 0 && from !== i) onMove(from, i); setFrom(-1); setOver(-1); },
    onDragEnd: () => { setFrom(-1); setOver(-1); },
  });
  const cls = (i) => (i === from ? "dragging" : i === over && from !== -1 ? "drop" : "");
  return [props, cls];
}
const move = (arr, a, b) => { const c = arr.slice(); const [x] = c.splice(a, 1); c.splice(b, 0, x); return c; };

function ModelListEditor({ models, setModels, known }) {
  const [add, setAdd] = useState("");
  const [props, cls] = useDrag(models, (a, b) => setModels(move(models, a, b)));
  const push = () => { const v = add.trim(); if (v && !models.includes(v)) setModels([...models, v]); setAdd(""); };
  const small = "width:24px;height:24px";
  return html`<div class="col" style="gap:10px">
    <div class="model-list">
      ${models.length === 0 && html`<div class="section small muted" style="text-align:center">No models yet. Add one below.</div>`}
      ${models.map((m, i) => html`<div class="model-item ${cls(i)}" key=${m} ...${props(i)}>
        <span class="handle" title="Drag to reorder"><${Icon} n="drag_indicator" /></span>
        <span class="idx">${i + 1}</span><code title=${m}>${m}</code>
        <button type="button" class="icon-btn" style=${small} disabled=${i === 0} onClick=${() => setModels(move(models, i, i - 1))} aria-label="Move up"><${Icon} n="arrow_upward" style="font-size:15px" /></button>
        <button type="button" class="icon-btn" style=${small} disabled=${i === models.length - 1} onClick=${() => setModels(move(models, i, i + 1))} aria-label="Move down"><${Icon} n="arrow_downward" style="font-size:15px" /></button>
        <button type="button" class="icon-btn danger" style=${small} onClick=${() => setModels(models.filter((x) => x !== m))} aria-label="Remove"><${Icon} n="close" style="font-size:15px" /></button>
      </div>`)}
    </div>
    <div class="row">
      <input class="input mono grow" list="known-models" placeholder="prefix/model or combo name, e.g. ds/deepseek-v4-flash" value=${add}
        onInput=${(e) => setAdd(e.target.value)} onKeyDown=${(e) => { if (e.key === "Enter") { e.preventDefault(); push(); } }} />
      <button type="button" class="btn secondary" onClick=${push}><${Icon} n="add" />Add</button>
    </div>
    <datalist id="known-models">${(known || []).map((m) => html`<option value=${m} />`)}</datalist>
  </div>`;
}

/* --------------------------------------------------------------- shell */
const NAV = [
  { id: "endpoint", label: "Dashboard", icon: "api", title: "Endpoint & Overview", sub: "Your local endpoint, connected providers and today's traffic" },
  { id: "providers", label: "Providers", icon: "dns", title: "Providers", sub: "Accounts behind each model prefix" },
  { id: "combos", label: "Combos & Vision", icon: "layers", title: "Combos & Vision Adapter", sub: "Named model chains with fallback or round-robin" },
  { id: "usage", label: "Usage", icon: "bar_chart", title: "Usage", sub: "Requests, tokens and estimated cost" },
  { id: "quota", label: "Quota Tracker", icon: "data_usage", title: "Quota Tracker", sub: "Account health, cooldowns and failure rates" },
  { id: "keys", label: "API Keys", icon: "vpn_key", title: "API Keys", sub: "Local keys your tools use to call FastRouter" },
  { id: "cli", label: "CLI Tools", icon: "terminal", title: "CLI Tools", sub: "Copy-paste setup for coding agents and SDKs" },
];
const SYSTEM = [{ id: "settings", label: "Settings", icon: "settings", title: "Settings", sub: "Token savers, thinking, vision fallback and routing" }];

function useHash() {
  const [h, setH] = useState(location.hash.slice(2) || "endpoint");
  useEffect(() => { const f = () => setH(location.hash.slice(2) || "endpoint"); addEventListener("hashchange", f); return () => removeEventListener("hashchange", f); }, []);
  return h;
}
function useTheme() {
  const [dark, setDark] = useState(document.documentElement.classList.contains("dark"));
  const toggle = () => { const d = !dark; setDark(d); document.documentElement.classList.toggle("dark", d); try { localStorage.setItem("fr-theme", d ? "dark" : "light"); } catch (e) {} };
  return [dark, toggle];
}

function Sidebar({ page, data }) {
  const usable = data ? data.providers.reduce((n, p) => n + p.usable, 0) : 0;
  const link = (it) => html`<a href=${"#/" + it.id} class=${page === it.id ? "active" : ""}><${Icon} n=${it.icon} /><span>${it.label}</span></a>`;
  return html`<aside class="sidebar">
    <div class="lights"><i style="background:#FF5F56"></i><i style="background:#FFBD2E"></i><i style="background:#27C93F"></i></div>
    <a class="brand" href="#/endpoint"><div class="brand-tile"><${Icon} n="bolt" fill /></div>
      <div><h1>FastRouter</h1><small>v${data ? data.version : ""}</small></div></a>
    <nav class="nav">${NAV.map(link)}<div class="nav-label">System</div>${SYSTEM.map(link)}</nav>
    <div class="side-foot"><div class="health"><span class="pulse"></span>Router online · ${usable} usable accounts</div></div>
  </aside>`;
}

function Header({ meta, dark, toggleTheme, onLogout, passwordSet }) {
  return html`<header class="header">
    <div class="grow"><h1><${Icon} n=${meta.icon} />${meta.title}</h1><p>${meta.sub}</p></div>
    <div class="header-actions">
      <span class="pill"><${Icon} n="person" />Admin${passwordSet ? "" : " · local"}</span>
      <button class="icon-btn" title=${dark ? "Light mode" : "Dark mode"} onClick=${toggleTheme}><${Icon} n=${dark ? "light_mode" : "dark_mode"} /></button>
      ${passwordSet && html`<button class="icon-btn" title="Log out" onClick=${onLogout}><${Icon} n="logout" /></button>`}
    </div></header>`;
}

/* --------------------------------------------------------------- login */
function Login({ onDone, error }) {
  const [pw, setPw] = useState("");
  const [err, setErr] = useState(error || "");
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault(); setBusy(true); setErr("");
    try { const r = await api("POST", "/api/auth/login", { password: pw }); csrf = r.csrf; onDone(); }
    catch (x) { setErr(x.message); setBusy(false); }
  };
  return html`<div class="auth"><div class="grid-bg"></div>
    <div class="card auth-card" style="padding:36px">
      <div class="brand-tile"><${Icon} n="bolt" fill /></div>
      <h1>Welcome to FastRouter</h1>
      <p class="muted" style="margin-top:6px">Sign in with your dashboard password (the same one as your 9Router dashboard).</p>
      <form onSubmit=${submit}>
        <${Field} label="Password"><${SecretInput} value=${pw} onInput=${setPw} placeholder="Dashboard password" autofocus /></${Field}>
        ${err && html`<div class="notice err"><${Icon} n="error" />${err}</div>`}
        <button class="btn primary" style="height:42px" disabled=${busy || !pw}><${Icon} n="login" />${busy ? "Signing in…" : "Sign in"}</button>
      </form>
    </div></div>`;
}

/* ------------------------------------------------------------ endpoint */
function EndpointPage({ data, go }) {
  const [copied, copy] = useCopy();
  const [usage, setUsage] = useState(null);
  useEffect(() => { api("GET", "/api/usage?window=24h").then(setUsage).catch(() => {}); }, []);
  const origin = location.origin;
  const rows = [["OpenAI", origin + "/v1"], ["Anthropic", origin], ["Gemini", origin + "/v1beta"]];
  const connected = data.providers.filter((p) => p.accounts.length > 0);
  const accounts = connected.reduce((n, p) => n + p.accounts.length, 0);
  const usable = data.providers.reduce((n, p) => n + p.usable, 0);
  const t = ((usage && usage.models) || []).reduce((a, r) => ({ req: a.req + r.requests, tok: a.tok + r.input_tokens + r.output_tokens, cost: a.cost + r.cost_usd }), { req: 0, tok: 0, cost: 0 });
  const keys = data.keys.filter((k) => k.active).length;
  const stats = [["dns", "Providers", connected.length, `${data.providers.length} available`],
    ["group", "Accounts", accounts, `${usable} usable right now`],
    ["layers", "Combos", data.combos.length, `${data.models.length} routable models`],
    ["speed", "Requests · 24h", fmt(t.req), t.req ? `${fmt(t.tok)} tokens · ${money(t.cost)}` : "No traffic yet"]];
  return html`
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="api" />API Endpoint</h2><p>Point any OpenAI, Anthropic or Gemini client here. Formats are translated for you.</p></div>
        <${Badge} kind=${keys ? "success" : "warning"} dot>${keys ? keys + " active API key" + (keys > 1 ? "s" : "") : "No API key — local only"}</${Badge}></div>
      <div class="col" style="gap:8px">${rows.map(([l, u]) => html`<div class="ep-row">
        <span class="ep-tag">${l}</span><input class="input mono grow" readonly value=${u} />
        <${CopyBtn} text=${u} id=${l} copied=${copied} copy=${copy} /></div>`)}</div>
    </div>
    <div class="grid g4">${stats.map(([ic, l, v, s]) => html`<div class="card sm stat"><span class="label"><${Icon} n=${ic} />${l}</span><span class="value">${v}</span><span class="sub">${s}</span></div>`)}</div>
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="hub" />Routing</h2><p>Clients → FastRouter → your providers, with failover between accounts.</p></div>
        <button class="btn outline sm" onClick=${() => go("providers")}>Manage providers<${Icon} n="chevron_right" /></button></div>
      <div class="topo">
        <div class="topo-node"><div class="topo-icon"><${Icon} n="terminal" /></div>Claude Code, Codex,<br/>Cursor, SDKs</div>
        <div class="topo-line"></div>
        <div class="topo-node"><div class="topo-icon hub"><${Icon} n="bolt" fill /></div><strong style="color:var(--text)">FastRouter</strong>${origin.replace(/^https?:\/\//, "")}</div>
        <div class="topo-line"></div>
        <div class="topo-node"><div class="topo-logos">${connected.slice(0, 14).map((p) => html`<span title=${provMeta(p).name}><${Logo} p=${p} /></span>`)}</div>${connected.length} connected</div>
      </div>
    </div>
    <div class="grid g2">
      <div class="card"><div class="card-title"><h2><${Icon} n="trending_up" />Top models · 24h</h2><a class="btn ghost sm" href="#/usage">Details</a></div>
        ${usage && usage.models && usage.models.length ? html`<table class="table"><tbody>${usage.models.slice(0, 6).map((r) => html`<tr><td class="mono small">${r.key}</td><td class="num">${fmt(r.requests)} req</td><td class="num muted">${money(r.cost_usd)}</td></tr>`)}</tbody></table>`
          : html`<div class="empty" style="padding:20px"><p class="muted small">No requests in the last 24 hours.</p></div>`}</div>
      <div class="card"><div class="card-title"><h2><${Icon} n="info" />Import notes</h2><span class="muted small">${data.warnings.length}</span></div>
        <div class="col" style="gap:8px;max-height:260px;overflow:auto">${data.warnings.length === 0 ? html`<p class="muted small">Nothing to review.</p>` :
          data.warnings.slice(0, 8).map((w) => html`<div class="row" style="align-items:flex-start;gap:8px"><${Badge} kind="warning">${w.Kind}</${Badge}><span class="small muted">${w.Message}</span></div>`)}</div></div>
    </div>`;
}

/* ----------------------------------------------------------- providers */
function statusOf(p) {
  if (p.kind === "plugin") return html`<${Badge} icon="extension">Needs plugin</${Badge}>`;
  if (p.accounts.length === 0) return html`<span class="muted">Not configured</span>`;
  const active = p.accounts.filter((a) => a.active);
  if (active.length === 0) return html`<${Badge} icon="pause_circle">Disabled</${Badge}>`;
  const cooling = active.filter((a) => a.cooling).length;
  const bad = active.filter((a) => a.expired || (a.status !== "ok" && a.status !== "needs-plugin")).length;
  return html`<${Badge} kind="success" dot>${p.usable} connected</${Badge}>
    ${cooling ? html`<${Badge} kind="warning" dot>${cooling} cooling</${Badge}>` : ""}
    ${bad ? html`<${Badge} kind="error" dot>${bad} error</${Badge}>` : ""}`;
}
const KIND_DOT = { apikey: "#f59e0b", login: "#3b82f6", plugin: "#9ca3af", "openai-compatible": "#f97316", "anthropic-compatible": "#f97316" };

function ProvidersPage({ data, reload, go, query }) {
  const [modal, setModal] = useState(/(^|&)add\b/.test(query) ? { t: "account" } : null);
  const toggleAll = (p, on) => run(async () => { for (const a of p.accounts) await api("PATCH", `/api/admin/accounts/${a.id}`, { active: on }); await reload(); }, on ? "Provider enabled" : "Provider disabled");
  const section = (title, icon, list, action) => html`<div>
    <div class="sec-head"><h2><${Icon} n=${icon} style="color:var(--primary)" />${title}<span class="count">${list.length}</span></h2>${action}</div>
    <div class="prov-grid">${list.map((p) => html`<div class="card xs hover" key=${p.id} onClick=${() => go("providers/" + encodeURIComponent(p.id))}
        style=${p.accounts.length && p.accounts.every((a) => !a.active) ? "opacity:.55" : ""}>
      <div class="prov-card"><div class="row grow" style="gap:12px"><${Logo} p=${p} />
        <div class="grow"><div class="prov-name truncate">${provMeta(p).name}</div>
          <div class="prov-meta"><span class="sdot" style=${`background:${KIND_DOT[p.kind]}`}></span><span class="muted mono">${p.prefix}/</span>${statusOf(p)}</div></div></div>
        ${p.accounts.length > 0 && p.kind !== "plugin" && html`<${Toggle} sm on=${p.accounts.some((a) => a.active)} title="Enable or disable every account" onChange=${(v) => toggleAll(p, v)} />`}
      </div></div>`)}</div></div>`;
  const builtin = data.providers.filter((p) => p.kind === "apikey");
  const logins = data.providers.filter((p) => p.kind === "login" || p.kind === "plugin");
  const nodes = data.providers.filter((p) => p.kind.endsWith("compatible"));
  return html`
    <div class="row between"><div class="legend">
      <span><i style="background:#f59e0b"></i>API key</span><span><i style="background:#3b82f6"></i>Official sign-in</span>
      <span><i style="background:#f97316"></i>Compatible endpoint</span><span><i style="background:#9ca3af"></i>Needs plugin</span></div>
      <div class="row" style="gap:8px"><button class="btn outline" onClick=${() => setModal({ t: "node" })}><${Icon} n="link" />Add endpoint</button>
        <button class="btn primary" onClick=${() => setModal({ t: "account" })}><${Icon} n="add" />Add account</button></div></div>
    ${section("API key providers", "vpn_key", builtin)}
    ${section("Sign-in providers", "verified", logins)}
    ${section("Custom endpoints", "link", nodes, html`<button class="btn ghost sm" onClick=${() => setModal({ t: "node" })}><${Icon} n="add" />New endpoint</button>`)}
    ${modal && modal.t === "account" && html`<${AccountForm} data=${data} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "node" && html`<${NodeForm} onClose=${() => setModal(null)} onSaved=${reload} />`}`;
}

function AccountForm({ data, provider, account, onClose, onSaved }) {
  const choices = data.providers.filter((p) => p.kind === "apikey" || p.kind.endsWith("compatible"));
  const [prov, setProv] = useState(account ? account.provider : provider ? provider.id : choices[0] && choices[0].id);
  const p = data.providers.find((x) => x.id === prov);
  const [name, setName] = useState(account ? account.name : "");
  const [key, setKey] = useState("");
  const [region, setRegion] = useState(account ? account.region || "" : "");
  const [active, setActive] = useState(account ? account.active : true);
  const [busy, setBusy] = useState(false);
  const isLogin = p && p.kind === "login";
  const save = async (e) => {
    if (e) e.preventDefault();
    setBusy(true);
    try {
      if (account) {
        const body = { name, active };
        if (key) body.apiKey = key;
        if (p && p.regions) body.region = region;
        await run(() => api("PATCH", `/api/admin/accounts/${account.id}`, body), "Account saved");
      } else {
        await run(() => api("POST", "/api/admin/accounts", { provider: prov, name, apiKey: key, active, region }), "Account added");
      }
      setKey(""); await onSaved(); onClose();
    } catch (x) { setBusy(false); }
  };
  const meta = p ? provMeta(p) : null;
  return html`<${Modal} title=${account ? "Edit account" : "Add account"} icon=${account ? "edit" : "add"} onClose=${onClose}
    foot=${html`<button class="btn ghost" onClick=${onClose}>Cancel</button>
      <button class="btn primary" disabled=${busy || !name || (!account && !key)} onClick=${save}><${Icon} n="save" />${account ? "Save" : "Add account"}</button>`}>
    <form onSubmit=${save} class="col" style="gap:16px">
      ${!account && !provider ? html`<${Field} label="Provider">
          <select class="input" value=${prov} onChange=${(e) => setProv(e.target.value)}>${choices.map((c) => html`<option value=${c.id}>${provMeta(c).name}  (${c.prefix}/)</option>`)}</select></${Field}>`
        : p && html`<div class="row section" style="padding:12px"><${Logo} p=${p} /><div><div class="strong">${meta.name}</div><div class="small muted mono">${p.prefix + "/<model>"}</div></div></div>`}
      <${Field} label="Account name" hint="Shown in the dashboard and usage reports."><input class="input" value=${name} placeholder="e.g. work, personal-2" maxlength="100" onInput=${(e) => setName(e.target.value)} /></${Field}>
      ${!isLogin && html`<${Field} label="API key" hint=${account ? `Stored encrypted. Current key ${account.keyHint || "not set"} — leave blank to keep it.` : "Stored encrypted (AES-GCM). It is never shown again, only a masked hint."}>
        <${SecretInput} value=${key} onInput=${setKey} placeholder=${account ? "Leave blank to keep the current key" : "sk-…"} /></${Field}>`}
      ${meta && meta.key && !account && html`<a class="small" style="color:var(--primary)" href=${meta.key} target="_blank" rel="noopener noreferrer">Get a ${meta.name} API key<${Icon} n="open_in_new" style="font-size:13px;vertical-align:-2px;margin-left:3px" /></a>`}
      ${p && p.regions && html`<${Field} label="Region"><select class="input" value=${region} onChange=${(e) => setRegion(e.target.value)}>
        <option value="">Default</option>${p.regions.map((r) => html`<option value=${r}>${r}</option>`)}</select></${Field}>`}
      <div class="row between section" style="padding:12px 14px"><div><div class="medium">Active</div><div class="small muted">Inactive accounts are kept but never used.</div></div><${Toggle} on=${active} onChange=${setActive} /></div>
    </form></${Modal}>`;
}

function NodeForm({ node, onClose, onSaved }) {
  const [f, setF] = useState(node ? { name: node.name, prefix: node.prefix, baseUrl: node.baseUrl, type: node.kind, apiType: node.apiType || "chat" }
    : { name: "", prefix: "", baseUrl: "", type: "openai-compatible", apiType: "chat" });
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setF({ ...f, [k]: e && e.target ? e.target.value : e });
  const save = async (e) => {
    if (e) e.preventDefault();
    setBusy(true);
    const body = { name: f.name, prefix: f.prefix, baseUrl: f.baseUrl };
    if (f.type === "openai-compatible") body.apiType = f.apiType;
    try {
      if (node) await run(() => api("PATCH", `/api/admin/nodes/${encodeURIComponent(node.id)}`, body), "Endpoint saved");
      else await run(() => api("POST", "/api/admin/nodes", { ...body, type: f.type }), "Endpoint added — now add an account to it");
      await onSaved(); onClose();
    } catch (x) { setBusy(false); }
  };
  return html`<${Modal} title=${node ? "Edit endpoint" : "Add compatible endpoint"} icon="link" onClose=${onClose}
    foot=${html`<button class="btn ghost" onClick=${onClose}>Cancel</button><button class="btn primary" disabled=${busy || !f.name || !f.prefix || !f.baseUrl} onClick=${save}><${Icon} n="save" />Save</button>`}>
    <form onSubmit=${save} class="col" style="gap:16px">
      ${!node && html`<${Field} label="Type"><${Seg} value=${f.type} onChange=${set("type")} options=${[["openai-compatible", "OpenAI compatible"], ["anthropic-compatible", "Anthropic compatible"]]} /></${Field}>`}
      <div class="grid g2"><${Field} label="Name"><input class="input" value=${f.name} placeholder="My gateway" onInput=${set("name")} /></${Field}>
        <${Field} label="Model prefix" hint=${html`Models are called as <code>${f.prefix || "prefix"}/model</code>`}><input class="input mono" value=${f.prefix} placeholder="gw" onInput=${set("prefix")} /></${Field}></div>
      <${Field} label="Base URL" hint=${f.type === "openai-compatible" ? "Up to /v1 — FastRouter adds /chat/completions or /responses." : "Up to /v1 — FastRouter adds /messages."}>
        <input class="input mono" value=${f.baseUrl} placeholder="https://api.example.com/v1" onInput=${set("baseUrl")} /></${Field}>
      ${f.type === "openai-compatible" && html`<${Field} label="API style"><${Seg} value=${f.apiType} onChange=${set("apiType")} options=${[["chat", "Chat Completions"], ["responses", "Responses"]]} /></${Field}>`}
    </form></${Modal}>`;
}

function ProviderDetail({ data, id, reload, go, query }) {
  const p = data.providers.find((x) => x.id === id);
  const [modal, setModal] = useState(/(^|&)add\b/.test(query) ? { t: "add" } : null);
  const [order, setOrder] = useState(null);
  const accts = order || (p ? p.accounts : []);
  const [props, cls] = useDrag(accts, async (a, b) => {
    const next = move(accts, a, b); setOrder(next);
    try { await run(() => api("POST", "/api/admin/accounts/reorder", { provider: p.id, ids: next.map((x) => x.id) }), "Priority updated"); await reload(); } catch (e) {}
    setOrder(null);
  });
  if (!p) return html`<div class="card empty"><p>Provider not found.</p><button class="btn outline" onClick=${() => go("providers")}>Back</button></div>`;
  const meta = provMeta(p);
  const patch = (a, body, ok) => run(async () => { await api("PATCH", `/api/admin/accounts/${a.id}`, body); await reload(); }, ok);
  const signIn = (conn) => setModal({ t: "signin", conn });
  const canAdd = p.kind === "apikey" || p.kind.endsWith("compatible");
  const others = p.aliases.filter((a) => a !== p.prefix);
  const dotColor = (a) => !a.active ? "var(--subtle)" : a.cooling ? "var(--warning)" : a.expired || (a.status !== "ok" && a.status !== "needs-plugin") ? "var(--danger)" : "var(--success)";
  const state = (a) => !a.active ? html`<${Badge}>Paused</${Badge}>` : a.cooling ? html`<${Badge} kind="warning" icon="hourglass_top">Cooling down</${Badge}>`
    : a.expired ? html`<${Badge} kind="error">Expired</${Badge}>` : a.status === "ok" || a.status === "needs-plugin" ? html`<${Badge} kind="success">Ready</${Badge}>` : html`<${Badge} kind="error">${a.status}</${Badge}>`;
  return html`
    <a class="back" href="#/providers"><${Icon} n="arrow_back" style="font-size:16px" />All providers</a>
    <div class="card"><div class="row between">
      <div class="row" style="gap:16px"><${Logo} p=${p} lg /><div><h2 style="font-size:20px;font-weight:600">${meta.name}</h2>
        <div class="row small muted" style="gap:8px;margin-top:4px;flex-wrap:wrap">Model prefix <span class="chip">${p.prefix}/</span>
          ${others.length > 0 && html`<span>also <span class="mono">${others.join(", ")}</span></span>`}
          ${p.baseUrl && html`<span class="mono truncate" style="max-width:360px">${p.baseUrl}</span>`}</div></div></div>
      <div class="row" style="gap:8px">
        ${p.kind.endsWith("compatible") && html`<button class="btn outline" onClick=${() => setModal({ t: "node" })}><${Icon} n="edit" />Edit endpoint</button>
          <button class="btn ghost" onClick=${() => setModal({ t: "delnode" })}><${Icon} n="delete" />Delete</button>`}
        ${p.kind === "login" && html`<button class="btn primary" onClick=${() => signIn("")}><${Icon} n="login" />Add ${meta.name} account</button>`}
        ${canAdd && html`<button class="btn primary" onClick=${() => setModal({ t: "add" })}><${Icon} n="add" />Add account</button>`}
      </div></div>
      ${p.kind === "plugin" && html`<div class="notice warn" style="margin-top:16px"><${Icon} n="extension" />This provider signs in through a workaround that FastRouter core does not ship. Accounts are kept from your import and skipped at runtime.</div>`}
      ${p.kind === "login" && html`<div class="notice info" style="margin-top:16px"><${Icon} n="info" />Official sign-in. ${p.id === "codex" ? "Tokens from FastRouter sign-ins refresh automatically; imported ones are left alone so your 9Router session stays signed in." : "Z.ai sign-in gives a coding-plan key; sign in again when it stops working."}</div>`}
    </div>
    <div class="card flush">
      <div class="row between" style="padding:16px 18px;border-bottom:1px solid var(--border-subtle)"><div><h2 style="font-size:16px;font-weight:600">Accounts</h2>
        <p class="small muted">Tried in priority order${accts.length > 1 ? " — drag to reorder" : ""}. ${p.usable} of ${p.accounts.length} usable now.</p></div></div>
      ${accts.length === 0 ? html`<div class="empty"><div class="bubble"><${Icon} n="group" /></div><p class="medium">No accounts yet</p>
          <p class="small muted" style="margin:4px 0 14px">Add an account to start routing <span class="mono">${p.prefix}/…</span> models.</p>
          ${canAdd && html`<button class="btn primary" onClick=${() => setModal({ t: "add" })}><${Icon} n="add" />Add account</button>`}</div>`
        : accts.map((a, i) => html`<div class="acct ${a.active ? "" : "off"} ${cls(i)}" key=${a.id} ...${accts.length > 1 ? props(i) : {}}>
          ${accts.length > 1 && html`<span class="handle" title="Drag to reorder"><${Icon} n="drag_indicator" /></span>`}
          <span class="prio" title="Priority">${i + 1}</span>
          <span class="sdot" style=${`background:${dotColor(a)}`}></span>
          <div class="grow"><div class="row" style="gap:8px"><span class="medium truncate">${a.name}</span>${state(a)}
              ${a.region && html`<${Badge} kind="info">${a.region}</${Badge}>`}</div>
            <div class="row small muted" style="gap:14px;margin-top:3px;flex-wrap:wrap">
              ${a.keyHint && html`<span class="row" style="gap:4px"><${Icon} n=${a.keyHint === "signed in" ? "verified" : "key"} style="font-size:14px" /><span class="mono">${a.keyHint}</span></span>`}
              ${a.expiresAt && html`<span>expires ${new Date(a.expiresAt).toLocaleDateString()}</span>`}
              <span>${fmt(a.uses)} requests</span>${a.failures > 0 && html`<span>${fmt(a.failures)} failed</span>`}<span>last used ${ago(a.lastUsedAt)}</span></div></div>
          ${p.kind === "login" && html`<button class="btn outline sm" onClick=${() => signIn(a.id)}><${Icon} n="refresh" />Sign in again</button>`}
          <${Toggle} sm on=${a.active} title=${a.active ? "Pause" : "Resume"} onChange=${(v) => patch(a, { active: v }, v ? "Account resumed" : "Account paused")} />
          <button class="icon-btn" title="Edit" onClick=${() => setModal({ t: "edit", a })}><${Icon} n="edit" /></button>
          <button class="icon-btn danger" title="Delete" onClick=${() => setModal({ t: "del", a })}><${Icon} n="delete" /></button>
        </div>`)}
    </div>
    ${modal && modal.t === "add" && html`<${AccountForm} data=${data} provider=${p} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "edit" && html`<${AccountForm} data=${data} account=${modal.a} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "node" && html`<${NodeForm} node=${p} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "signin" && html`<${SignIn} provider=${p} conn=${modal.conn} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "del" && html`<${Confirm} title="Delete account?" text=${`"${modal.a.name}" and its stored key will be removed. This can't be undone.`} onClose=${() => setModal(null)}
      onYes=${() => run(async () => { await api("DELETE", `/api/admin/accounts/${modal.a.id}`); await reload(); }, "Account deleted")} />`}
    ${modal && modal.t === "delnode" && html`<${Confirm} title="Delete endpoint?" text=${`"${p.name}" and its ${p.accounts.length} account(s) will be removed. Combos that use ${p.prefix}/ models will skip them.`} onClose=${() => setModal(null)}
      onYes=${() => run(async () => { await api("DELETE", `/api/admin/nodes/${encodeURIComponent(p.id)}`); await reload(); go("providers"); }, "Endpoint deleted")} />`}`;
}

function SignIn({ provider, conn, onClose, onSaved }) {
  const [flow, setFlow] = useState(null);
  const [err, setErr] = useState("");
  const [paste, setPaste] = useState("");
  useEffect(() => {
    let stop = false, t;
    api("POST", `/api/login/${provider.id}/start`, { connectionId: conn || "" }).then((f) => {
      setFlow(f);
      const poll = async () => {
        if (stop) return;
        try {
          const g = await api("GET", `/api/login/flows/${f.id}`); setFlow(g);
          if (g.status === "done") { toast("Signed in"); await onSaved(); return; }
          if (g.status === "error") return;
        } catch (e) { /* keep polling */ }
        t = setTimeout(poll, 2000);
      };
      t = setTimeout(poll, 2000);
    }).catch((e) => setErr(e.message));
    return () => { stop = true; clearTimeout(t); };
  }, []);
  const submitPaste = () => run(async () => { const g = await api("POST", `/api/login/flows/${flow.id}/callback`, { url: paste }); setFlow(g); if (g.status === "done") await onSaved(); });
  return html`<${Modal} title=${`Sign in to ${provMeta(provider).name}`} icon="login" onClose=${onClose} foot=${html`<button class="btn ghost" onClick=${onClose}>Close</button>`}>
    ${err && html`<div class="notice err"><${Icon} n="error" />${err}</div>`}
    ${!flow && !err && html`<p class="muted">Starting sign-in…</p>`}
    ${flow && flow.status === "done" && html`<div class="notice ok"><${Icon} n="check_circle" />Signed in. The account is ready to use.</div>`}
    ${flow && flow.status === "error" && html`<div class="notice err"><${Icon} n="error" />${flow.message}</div>`}
    ${flow && flow.status === "pending" && html`<ol class="small" style="margin:0;padding-left:18px;line-height:1.8">
        <li>Open the official sign-in page and approve access.</li>
        <li>${flow.kind === "browser" ? "If the browser can't reach this machine, paste the final localhost URL below." : "Come back here; this window updates by itself."}</li></ol>
      <a class="btn primary" href=${flow.url} target="_blank" rel="noopener noreferrer"><${Icon} n="open_in_new" />Open sign-in page</a>
      ${flow.kind === "browser" && html`<${Field} label="Paste the redirect URL (optional)"><div class="row"><input class="input mono grow" value=${paste} placeholder="http://localhost:1455/auth/callback?code=…" onInput=${(e) => setPaste(e.target.value)} />
        <button class="btn secondary" disabled=${!paste} onClick=${submitPaste}>Submit</button></div></${Field}>`}
      <p class="small muted row" style="gap:6px"><${Icon} n="sync" cls="spin" style="font-size:16px" />Waiting for sign-in…</p>`}
  </${Modal}>`;
}

/* -------------------------------------------------------------- combos */
function ModelChain({ models, empty }) {
  if (!models.length) return html`<span class="small muted">${empty}</span>`;
  return html`<div class="combo-models">${models.map((m, i) => html`${i > 0 && html`<span class="arrow-sep"><${Icon} n="chevron_right" style="font-size:16px" /></span>`}<span class="chip" title=${m}>${m}</span>`)}</div>`;
}
function ComboForm({ data, combo, onClose, onSaved }) {
  const [name, setName] = useState(combo ? combo.name : "");
  const [strategy, setStrategy] = useState(combo ? combo.strategy || "fallback" : "fallback");
  const [models, setModels] = useState(combo ? combo.models.slice() : []);
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    try {
      const body = { name, strategy, models };
      if (combo) await run(() => api("PATCH", `/api/admin/combos/${combo.id}`, body), "Combo saved");
      else await run(() => api("POST", "/api/admin/combos", body), "Combo created");
      await onSaved(); onClose();
    } catch (e) { setBusy(false); }
  };
  return html`<${Modal} wide title=${combo ? "Edit combo" : "New combo"} icon="layers" onClose=${onClose}
    foot=${html`<button class="btn ghost" onClick=${onClose}>Cancel</button><button class="btn primary" disabled=${busy || !name || !models.length} onClick=${save}><${Icon} n="save" />Save combo</button>`}>
    <div class="grid g2"><${Field} label="Combo name" hint="Clients call it as the model name."><input class="input mono" value=${name} placeholder="my-combo" onInput=${(e) => setName(e.target.value)} /></${Field}>
      <${Field} label="Strategy" hint=${strategy === "fallback" ? "Always start with model 1; move on when it fails." : "Rotate the starting model on every request."}>
        <${Seg} value=${strategy} onChange=${setStrategy} options=${[["fallback", "Fallback"], ["round-robin", "Round-robin"]]} /></${Field}></div>
    <${Field} label="Models" hint="Drag to set the order. Each entry is prefix/model or another combo's name."><${ModelListEditor} models=${models} setModels=${setModels} known=${data.models} /></${Field}>
  </${Modal}>`;
}

function CombosPage({ data, reload }) {
  const [modal, setModal] = useState(null);
  const [vm, setVm] = useState(null);
  const v = data.settings.vision;
  const saveVision = (next) => run(async () => { await api("PATCH", "/api/admin/settings", { vision: next }); await reload(); }, "Vision adapter saved");
  return html`
    <div class="row between"><p class="muted">A combo is a model name that walks through a list of models until one answers.</p>
      <button class="btn primary" onClick=${() => setModal({ t: "new" })}><${Icon} n="add" />New combo</button></div>
    ${data.combos.length === 0 ? html`<div class="card empty"><div class="bubble"><${Icon} n="layers" /></div><p class="medium">No combos yet</p>
        <p class="small muted" style="margin:4px 0 14px">Chain models across providers so one failing never stops your agent.</p>
        <button class="btn primary" onClick=${() => setModal({ t: "new" })}><${Icon} n="add" />Create combo</button></div>`
      : html`<div class="grid g2">${data.combos.map((c) => html`<div class="card" key=${c.id}>
          <div class="row between" style="margin-bottom:14px"><div class="row" style="gap:10px"><div class="saver-icon"><${Icon} n="layers" /></div>
            <div><div class="strong mono">${c.name}</div><div class="row" style="gap:6px;margin-top:3px">
              <${Badge} kind=${c.strategy === "round-robin" ? "info" : "primary"}>${c.strategy === "round-robin" ? "Round-robin" : "Fallback"}</${Badge}>
              ${c.kind && html`<${Badge}>${c.kind}</${Badge}>`}<span class="small muted">${c.models.length} model${c.models.length === 1 ? "" : "s"}</span></div></div></div>
            <div class="row" style="gap:2px"><button class="icon-btn" title="Edit" onClick=${() => setModal({ t: "edit", c })}><${Icon} n="edit" /></button>
              <button class="icon-btn danger" title="Delete" onClick=${() => setModal({ t: "del", c })}><${Icon} n="delete" /></button></div></div>
          <${ModelChain} models=${c.models} empty="Empty — add models to use it." />
        </div>`)}</div>`}
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="image" />Vision adapter</h2><p>When a request carries images and the chosen model can't read them, FastRouter tries these vision models first.</p></div>
        <${Toggle} on=${v.enabled} onChange=${(on) => saveVision({ ...v, enabled: on })} /></div>
      <div class="row between section" style="padding:12px 14px;margin-bottom:14px"><div><div class="medium">Rotation</div><div class="small muted">Fallback keeps the order; round-robin spreads image requests.</div></div>
        <${Seg} value=${v.roundRobin ? "rr" : "fb"} onChange=${(x) => saveVision({ ...v, roundRobin: x === "rr" })} options=${[["fb", "Fallback"], ["rr", "Round-robin"]]} /></div>
      ${vm ? html`<${ModelListEditor} models=${vm} setModels=${setVm} known=${data.models} />
          <div class="row" style="justify-content:flex-end;margin-top:12px;gap:8px"><button class="btn ghost" onClick=${() => setVm(null)}>Cancel</button>
            <button class="btn primary" onClick=${async () => { await saveVision({ ...v, models: vm }); setVm(null); }}><${Icon} n="save" />Save models</button></div>`
        : html`<div class="row between" style="align-items:flex-start"><div class="grow"><${ModelChain} models=${v.models} empty="No vision models set." /></div>
            <button class="btn outline sm" onClick=${() => setVm(v.models.slice())}><${Icon} n="edit" />Edit models</button></div>`}
    </div>
    ${modal && modal.t === "new" && html`<${ComboForm} data=${data} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "edit" && html`<${ComboForm} data=${data} combo=${modal.c} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "del" && html`<${Confirm} title="Delete combo?" text=${`Clients calling "${modal.c.name}" will get "model not found".`} onClose=${() => setModal(null)}
      onYes=${() => run(async () => { await api("DELETE", `/api/admin/combos/${modal.c.id}`); await reload(); }, "Combo deleted")} />`}`;
}

/* --------------------------------------------------------------- usage */
function UsageTable({ title, icon, list, label }) {
  const max = Math.max(1, ...list.map((r) => r.requests));
  return html`<div class="card flush"><div class="row between" style="padding:16px 18px"><h2 style="font-size:16px;font-weight:600" class="row"><${Icon} n=${icon} style="color:var(--primary)" />${title}</h2></div>
    ${list.length === 0 ? html`<p class="small muted" style="padding:0 18px 18px">No requests in this window.</p>` : html`<table class="table"><thead><tr><th>${label}</th><th>Requests</th><th class="num">Tokens in / out</th><th class="num">Cached</th><th class="num">Avg latency</th><th class="num">Cost</th></tr></thead>
      <tbody>${list.map((r) => html`<tr><td class="mono small">${r.name || r.key}${r.provider ? html`<span class="muted"> · ${r.provider}</span>` : ""}</td>
        <td><div class="row" style="gap:8px"><div class="bar grow"><i style=${`width:${(100 * r.requests) / max}%`}></i></div><span class="small" style="min-width:42px">${fmt(r.requests)}</span>${r.failures ? html`<${Badge} kind="error">${r.failures} failed</${Badge}>` : ""}</div></td>
        <td class="num small">${fmt(r.input_tokens)} / ${fmt(r.output_tokens)}</td><td class="num small muted">${fmt(r.cache_read_tokens)}</td><td class="num small">${r.avg_latency_ms} ms</td><td class="num small">${money(r.cost_usd)}</td></tr>`)}</tbody></table>`}</div>`;
}
function UsagePage() {
  const [win, setWin] = useState("24h");
  const [u, setU] = useState(null);
  useEffect(() => { setU(null); api("GET", "/api/usage?window=" + win).then(setU).catch((e) => setU({ error: e.message })); }, [win]);
  const rows = (u && u.models) || [];
  const t = rows.reduce((a, r) => ({ req: a.req + r.requests, fail: a.fail + r.failures, inp: a.inp + r.input_tokens, out: a.out + r.output_tokens, cache: a.cache + r.cache_read_tokens, cost: a.cost + r.cost_usd }), { req: 0, fail: 0, inp: 0, out: 0, cache: 0, cost: 0 });
  const stats = [["speed", "Requests", fmt(t.req), t.fail ? `${t.fail} failed` : "all succeeded"], ["arrow_downward", "Input tokens", fmt(t.inp), `${fmt(t.cache)} from cache`],
    ["arrow_upward", "Output tokens", fmt(t.out), "including reasoning"], ["payments", "Estimated cost", money(t.cost), "from 9Router's price table"]];
  return html`
    <div class="row between"><${Seg} value=${win} onChange=${setWin} options=${[["24h", "24 hours"], ["7d", "7 days"], ["30d", "30 days"], ["all", "All time"]]} /></div>
    ${u && u.error && html`<div class="notice err"><${Icon} n="error" />${u.error}</div>`}
    <div class="grid g4">${stats.map(([ic, l, v, s]) => html`<div class="card sm stat"><span class="label"><${Icon} n=${ic} />${l}</span><span class="value">${u ? v : "…"}</span><span class="sub">${s}</span></div>`)}</div>
    ${u ? html`<${UsageTable} title="By model" icon="memory" list=${rows} label="Model" /><${UsageTable} title="By account" icon="group" list=${u.accounts || []} label="Account" />
      <${UsageTable} title="By API key" icon="vpn_key" list=${u.keys || []} label="Key" />` : html`<div class="skeleton"></div>`}`;
}

/* --------------------------------------------------------------- quota */
function QuotaPage({ data }) {
  const accts = data.providers.flatMap((p) => p.accounts.map((a) => ({ ...a, p })));
  const active = accts.filter((a) => a.active).sort((a, b) => b.uses - a.uses);
  const maxUse = Math.max(1, ...active.map((a) => a.uses));
  const stats = [["group", "Active accounts", active.length], ["hourglass_top", "Cooling down", active.filter((a) => a.cooling).length], ["error", "Failures (all time)", fmt(accts.reduce((n, a) => n + a.failures, 0))]];
  return html`
    <div class="notice info"><${Icon} n="info" />Provider-side balances and plan limits aren't fetched yet. This page shows what FastRouter tracks itself: per-account use, failures and cooldowns.</div>
    <div class="grid g3">${stats.map(([ic, l, v]) => html`<div class="card sm stat"><span class="label"><${Icon} n=${ic} />${l}</span><span class="value">${v}</span></div>`)}</div>
    <div class="card flush"><div style="padding:16px 18px"><h2 style="font-size:16px;font-weight:600">Accounts by use</h2></div>
      <table class="table"><thead><tr><th>Account</th><th>State</th><th>Requests</th><th class="num">Failure rate</th><th class="num">Last used</th></tr></thead><tbody>
      ${active.map((a) => html`<tr><td><div class="row" style="gap:10px"><${Logo} p=${a.p} /><div><div class="medium">${a.name}</div><div class="small muted">${provMeta(a.p).name}</div></div></div></td>
        <td>${a.p.kind === "plugin" ? html`<${Badge} icon="extension">Needs plugin</${Badge}>` : a.cooling ? html`<${Badge} kind="warning" icon="hourglass_top">Cooling</${Badge}>` : a.expired ? html`<${Badge} kind="error">Expired</${Badge}>` : html`<${Badge} kind="success" dot>Ready</${Badge}>`}</td>
        <td><div class="row" style="gap:8px"><div class="bar grow"><i style=${`width:${(100 * a.uses) / maxUse}%`}></i></div><span class="small">${fmt(a.uses)}</span></div></td>
        <td class="num small">${a.uses ? ((100 * a.failures) / a.uses).toFixed(1) + "%" : "—"}</td><td class="num small muted">${ago(a.lastUsedAt)}</td></tr>`)}</tbody></table></div>`;
}

/* ---------------------------------------------------------------- keys */
function newKey() {
  const b = new Uint8Array(24); crypto.getRandomValues(b);
  return "sk-fr-" + Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}
function KeysPage({ data, reload }) {
  const [modal, setModal] = useState(null);
  const patch = (k, body, ok) => run(async () => { await api("PATCH", `/api/admin/keys/${k.id}`, body); await reload(); }, ok);
  return html`
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="vpn_key" />API Keys</h2><p>${data.keys.length ? "Requests must carry one of these keys." : "No keys: only clients on this machine can call FastRouter."}</p></div>
        <button class="btn primary" onClick=${() => setModal({ t: "new" })}><${Icon} n="add" />Create key</button></div>
      ${data.keys.length === 0 ? html`<div class="empty"><div class="bubble"><${Icon} n="vpn_key" /></div><p class="medium">No API keys yet</p><p class="small muted" style="margin-top:4px">Create one before exposing FastRouter beyond this machine.</p></div>`
        : data.keys.map((k) => html`<div class="acct ${k.active ? "" : "off"}" style="padding:14px 0" key=${k.id}>
          <div class="saver-icon"><${Icon} n="key" /></div>
          <div class="grow"><div class="row" style="gap:8px"><span class="medium">${k.name}</span>${!k.active && html`<${Badge}>Paused</${Badge}>`}
              ${k.restricted ? html`<${Badge} kind="warning" icon="lock">${k.allow.length} model${k.allow.length === 1 ? "" : "s"}</${Badge}>` : html`<${Badge} kind="success">All models</${Badge}>`}</div>
            <div class="small muted mono" style="margin-top:3px">${k.hint}</div>
            ${k.restricted && k.allow.length > 0 && html`<div class="combo-models" style="margin-top:8px">${k.allow.map((m) => html`<span class="chip">${m}</span>`)}</div>`}</div>
          <${Toggle} sm on=${k.active} title=${k.active ? "Pause key" : "Resume key"} onChange=${(v) => patch(k, { active: v }, v ? "Key resumed" : "Key paused")} />
          <button class="icon-btn" title="Edit access" onClick=${() => setModal({ t: "edit", k })}><${Icon} n="tune" /></button>
          <button class="icon-btn danger" title="Delete" onClick=${() => setModal({ t: "del", k })}><${Icon} n="delete" /></button></div>`)}
    </div>
    <div class="notice info"><${Icon} n="shield" />Keys are stored encrypted and never shown again after creation, only the last four characters. Lost a key? Create a new one and delete the old.</div>
    ${modal && (modal.t === "new" || modal.t === "edit") && html`<${KeyForm} data=${data} k=${modal.k} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "del" && html`<${Confirm} title="Delete key?" text=${`Tools using "${modal.k.name}" will get 401 errors.`} onClose=${() => setModal(null)}
      onYes=${() => run(async () => { await api("DELETE", `/api/admin/keys/${modal.k.id}`); await reload(); }, "Key deleted")} />`}`;
}
function KeyForm({ data, k, onClose, onSaved }) {
  const [name, setName] = useState(k ? k.name : "");
  const [restricted, setRestricted] = useState(k ? k.restricted : false);
  const [allow, setAllow] = useState(k ? k.allow.slice() : []);
  const [created, setCreated] = useState("");
  const [copied, copy] = useCopy();
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    try {
      if (k) { await run(() => api("PATCH", `/api/admin/keys/${k.id}`, { name, restricted, allow }), "Key saved"); await onSaved(); onClose(); return; }
      const key = newKey(); // generated here; the server stores it encrypted and never returns it
      await run(() => api("POST", "/api/admin/keys", { name, key, restricted, allow }), "Key created");
      setCreated(key); await onSaved(); setBusy(false);
    } catch (e) { setBusy(false); }
  };
  if (created) return html`<${Modal} title="Key created" icon="check_circle" onClose=${onClose} foot=${html`<button class="btn primary" onClick=${onClose}>Done</button>`}>
    <div class="notice warn"><${Icon} n="warning" />Copy it now. FastRouter stores it encrypted and won't show it again.</div>
    <div class="code">${created}<button class="icon-btn" onClick=${() => copy(created, "k")}><${Icon} n=${copied ? "check" : "content_copy"} /></button></div></${Modal}>`;
  return html`<${Modal} wide title=${k ? "Edit key" : "Create API key"} icon="vpn_key" onClose=${onClose}
    foot=${html`<button class="btn ghost" onClick=${onClose}>Cancel</button><button class="btn primary" disabled=${busy || !name} onClick=${save}><${Icon} n=${k ? "save" : "add"} />${k ? "Save" : "Create key"}</button>`}>
    <${Field} label="Name"><input class="input" value=${name} placeholder="e.g. laptop, claude-code" onInput=${(e) => setName(e.target.value)} /></${Field}>
    <div class="row between section" style="padding:12px 14px"><div><div class="medium">Restrict models</div><div class="small muted">Only allow the models and combos listed below. Globs like <code>ds/*</code> work.</div></div><${Toggle} on=${restricted} onChange=${setRestricted} /></div>
    ${restricted && html`<${ModelListEditor} models=${allow} setModels=${setAllow} known=${data.models} />`}
  </${Modal}>`;
}

/* ----------------------------------------------------------- cli tools */
// Snippets never contain a real key: paste your own in place of the placeholder.
const KEY = "YOUR_FASTROUTER_KEY";
const TOOLS = [
  { id: "claude", name: "Claude Code", logo: "claude", note: "Anthropic format. Add to ~/.claude/settings.json or export in your shell.",
    files: (o, m) => [["~/.claude/settings.json", `{\n  "env": {\n    "ANTHROPIC_BASE_URL": "${o}",\n    "ANTHROPIC_AUTH_TOKEN": "${KEY}",\n    "ANTHROPIC_MODEL": "${m}",\n    "ANTHROPIC_SMALL_FAST_MODEL": "${m}"\n  }\n}`],
      ["Shell", `export ANTHROPIC_BASE_URL="${o}"\nexport ANTHROPIC_AUTH_TOKEN="${KEY}"\nclaude --model ${m}`]] },
  { id: "codex", name: "OpenAI Codex CLI", logo: "codex", note: "Responses API. Add a provider to ~/.codex/config.toml.",
    files: (o, m) => [["~/.codex/config.toml", `model = "${m}"\nmodel_provider = "fastrouter"\n\n[model_providers.fastrouter]\nname = "FastRouter"\nbase_url = "${o}/v1"\nenv_key = "FASTROUTER_API_KEY"\nwire_api = "responses"`],
      ["Shell", `export FASTROUTER_API_KEY="${KEY}"\ncodex`]] },
  { id: "opencode", name: "OpenCode", logo: "opencode", note: "OpenAI-compatible provider in opencode.json.",
    files: (o, m) => [["~/.config/opencode/opencode.json", `{\n  "$schema": "https://opencode.ai/config.json",\n  "provider": {\n    "fastrouter": {\n      "npm": "@ai-sdk/openai-compatible",\n      "name": "FastRouter",\n      "options": { "baseURL": "${o}/v1", "apiKey": "${KEY}" },\n      "models": { "${m}": { "name": "${m}" } }\n    }\n  },\n  "model": "fastrouter/${m}"\n}`]] },
  { id: "cline", name: "Cline", logo: "cline", note: "Settings → API Provider → OpenAI Compatible.", form: true },
  { id: "roo", name: "Roo Code", logo: "roo", note: "Settings → Providers → OpenAI Compatible.", form: true },
  { id: "kilocode", name: "Kilo Code", logo: "kilocode", note: "Settings → API Provider → OpenAI Compatible.", form: true },
  { id: "continue", name: "Continue", logo: "continue", note: "Add a model to ~/.continue/config.yaml.",
    files: (o, m) => [["~/.continue/config.yaml", `models:\n  - name: FastRouter ${m}\n    provider: openai\n    model: ${m}\n    apiBase: ${o}/v1\n    apiKey: ${KEY}\n    roles: [chat, edit, apply]`]] },
  { id: "droid", name: "Factory Droid", logo: "droid", note: "Custom model in ~/.factory/config.json.",
    files: (o, m) => [["~/.factory/config.json", `{\n  "custom_models": [\n    {\n      "model_display_name": "FastRouter ${m}",\n      "model": "${m}",\n      "base_url": "${o}/v1",\n      "api_key": "${KEY}",\n      "provider": "generic-chat-completion-api"\n    }\n  ]\n}`]] },
  { id: "cursor", name: "Cursor", logo: "cursor", note: "Settings → Models → override the OpenAI base URL. Cursor needs a public HTTPS address (e.g. a tunnel).", form: true },
  { id: "curl", name: "cURL", icon: "terminal", note: "Quick test from any shell.",
    files: (o, m) => [["Shell", `curl ${o}/v1/chat/completions \\\n  -H "Authorization: Bearer ${KEY}" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${m}","messages":[{"role":"user","content":"Hello"}]}'`]] },
  { id: "python", name: "OpenAI SDK", logo: "openai", note: "Python; the Node SDK takes the same baseURL and apiKey.",
    files: (o, m) => [["main.py", `from openai import OpenAI\n\nclient = OpenAI(base_url="${o}/v1", api_key="${KEY}")\nr = client.chat.completions.create(\n    model="${m}",\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(r.choices[0].message.content)`]] },
  { id: "anthropic-sdk", name: "Anthropic SDK", logo: "anthropic", note: "Python; works with any Anthropic-format client.",
    files: (o, m) => [["main.py", `import anthropic\n\nclient = anthropic.Anthropic(base_url="${o}", api_key="${KEY}")\nr = client.messages.create(\n    model="${m}",\n    max_tokens=1024,\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(r.content[0].text)`]] },
];
function CliPage({ data }) {
  const names = [...data.combos.map((c) => c.name), ...data.models.filter((m) => !data.combos.some((c) => c.name === m))];
  const [tool, setTool] = useState("claude");
  const [model, setModel] = useState(names[0] || "ds/deepseek-v4.1-flash");
  const [copied, copy] = useCopy();
  const t = TOOLS.find((x) => x.id === tool);
  const o = location.origin;
  const files = t.form ? [["Fields", `Base URL:  ${o}/v1\nAPI key:   ${KEY}\nModel ID:  ${model}`]] : t.files(o, model);
  return html`
    <div class="tool-grid">${TOOLS.map((x) => html`<button class="card xs hover tool ${x.id === tool ? "on" : ""}" style="text-align:left" onClick=${() => setTool(x.id)}>
      ${x.logo ? html`<div class="logo"><img src=${"logos/" + x.logo + ".png"} alt="" /></div>` : html`<div class="saver-icon"><${Icon} n=${x.icon} /></div>`}
      <div class="grow"><div class="medium">${x.name}</div><div class="small muted">${x.form ? "Settings form" : "Config file"}</div></div>
      ${x.id === tool && html`<${Icon} n="check_circle" fill style="color:var(--primary)" />`}</button>`)}</div>
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="terminal" />${t.name}</h2><p>${t.note}</p></div>
        <div class="field" style="min-width:280px"><label>Model</label><select class="input mono" value=${model} onChange=${(e) => setModel(e.target.value)}>
          ${names.map((n) => html`<option value=${n}>${n}</option>`)}</select></div></div>
      <div class="col" style="gap:14px">${files.map(([f, body], i) => html`<div><div class="small muted mono" style="margin-bottom:6px">${f}</div>
        <pre class="code">${body}<${CopyBtn} text=${body} id=${"f" + i} copied=${copied} copy=${copy} /></pre></div>`)}</div>
      <div class="notice info" style="margin-top:16px"><${Icon} n="key" />Replace <code>${KEY}</code> with a key from <a href="#/keys">API Keys</a>${data.keys.length ? "" : " (or leave any value: with no keys, local clients are allowed)"}.</div>
    </div>`;
}

/* ------------------------------------------------------------ settings */
const THINK = [["auto", "Auto"], ["off", "Off"], ["low", "Low"], ["medium", "Medium"], ["high", "High"], ["max", "Max"]];
function SettingsPage({ data, reload, relogin }) {
  const s = data.settings;
  const locked = (k) => s.lockedByConfig.includes(k);
  const save = (body, ok = "Settings saved") => run(async () => { await api("PATCH", "/api/admin/settings", body); await reload(); }, ok);
  const [hrUrl, setHrUrl] = useState(s.headroomUrl);
  const [sticky, setSticky] = useState(String(s.stickyRoundRobinLimit));
  const Lock = ({ k }) => locked(k) && html`<span class="lock" title="Set in the config file"><${Icon} n="lock" />config file</span>`;
  const routed = data.providers.filter((p) => p.accounts.length > 0 && p.kind !== "plugin");
  const saver = (icon, title, desc, enabledKey, levelKey, lockKey, extra) => html`<div class="set-row" style="align-items:flex-start">
    <div class="saver-head" style="align-items:flex-start"><div class="saver-icon"><${Icon} n=${icon} /></div>
      <div><div class="t">${title}<${Lock} k=${lockKey} /></div><div class="d">${desc}</div>${extra}</div></div>
    <div class="row" style="gap:12px">${levelKey && s[enabledKey] && html`<${Seg} value=${s[levelKey]} disabled=${locked(lockKey)} onChange=${(v) => save({ [levelKey]: v })} options=${[["lite", "Lite"], ["full", "Full"], ["ultra", "Ultra"]]} />`}
      <${Toggle} on=${s[enabledKey]} disabled=${locked(lockKey)} onChange=${(v) => save({ [enabledKey]: v }, v ? title + " on" : title + " off")} /></div></div>`;
  return html`
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="compress" />Token savers</h2><p>Shrink what goes upstream. Applied to every request; changes take effect immediately.</p></div></div>
      ${saver("terminal", "RTK", "Compresses long tool output (test logs, git diffs, builds) before it reaches the model.", "rtkEnabled", null, "rtkEnabled")}
      ${saver("record_voice_over", "Caveman", "Asks the model to answer tersely. Fewer output tokens, same substance.", "cavemanEnabled", "cavemanLevel", "caveman")}
      ${saver("content_cut", "Ponytail", "Trims the system prompt and old turns of long conversations.", "ponytailEnabled", "ponytailLevel", "ponytail")}
      ${saver("cloud_sync", "Headroom", "Sends context through a Headroom compression server first. Requests fall back to direct if it's down.", "headroomEnabled", null, "headroomEnabled",
        s.headroomEnabled && html`<div class="row" style="gap:8px;margin-top:10px;flex-wrap:wrap"><input class="input mono" style="width:280px" value=${hrUrl} disabled=${locked("headroomUrl")} onInput=${(e) => setHrUrl(e.target.value)} />
          <button class="btn outline sm" disabled=${hrUrl === s.headroomUrl || locked("headroomUrl")} onClick=${() => save({ headroomUrl: hrUrl })}>Save URL</button>
          <label class="row small" style="gap:8px"><${Toggle} sm on=${s.headroomCodeAware} disabled=${locked("headroomCodeAware")} onChange=${(v) => save({ headroomCodeAware: v })} />Code-aware</label></div>`)}
    </div>
    <div class="grid g2" style="align-items:start">
      <div class="card">
        <div class="card-title"><div><h2><${Icon} n="psychology" />Thinking</h2><p>Reasoning effort per provider. Auto passes through whatever the client asked for.</p></div></div>
        ${routed.length === 0 ? html`<p class="small muted">Add an account first.</p>` : routed.map((p) => html`<div class="set-row">
          <div class="row" style="gap:10px"><${Logo} p=${p} /><div class="t">${provMeta(p).name}</div></div>
          <select class="input" style="width:130px" value=${s.providerThinking[p.id] || "auto"} onChange=${(e) => save({ providerThinking: { [p.id]: e.target.value } }, "Thinking updated")}>
            ${THINK.map(([v, l]) => html`<option value=${v}>${l}</option>`)}</select></div>`)}
      </div>
      <div class="card">
        <div class="card-title"><div><h2><${Icon} n="alt_route" />Routing strategy</h2><p>How each provider picks between its accounts.</p></div></div>
        ${routed.map((p) => { const st = s.providerStrategies[p.id] || {}; const v = st.fallbackStrategy || "fallback"; return html`<div class="set-row">
          <div class="row" style="gap:10px"><${Logo} p=${p} /><div><div class="t">${provMeta(p).name}</div><div class="d">${p.accounts.length} account${p.accounts.length === 1 ? "" : "s"}</div></div></div>
          ${p.accounts.length < 2 ? html`<span class="small muted">One account — nothing to rotate</span>` : html`<${Seg} value=${v} onChange=${(x) => save({ providerStrategies: { [p.id]: { fallbackStrategy: x, stickyRoundRobinLimit: st.stickyRoundRobinLimit || 0 } } }, "Strategy updated")}
            options=${[["fallback", "Fill first"], ["round-robin", "Round-robin"]]} />`}</div>`; })}
        <div class="set-row"><div><div class="t">Sticky round-robin</div><div class="d">Requests in a row that stay on one account before rotating.</div></div>
          <div class="row" style="gap:8px"><input class="input" type="number" min="1" max="1000" style="width:80px" value=${sticky} onInput=${(e) => setSticky(e.target.value)} />
            <button class="btn outline sm" disabled=${sticky === String(s.stickyRoundRobinLimit)} onClick=${() => save({ stickyRoundRobinLimit: parseInt(sticky, 10) || 1 })}>Save</button></div></div>
      </div>
    </div>
    <div class="card">
      <div class="set-row"><div class="saver-head"><div class="saver-icon"><${Icon} n="image" /></div><div><div class="t">Vision fallback</div>
        <div class="d">Route image requests to a vision model when the chosen one is text-only. ${s.vision.models.length} model${s.vision.models.length === 1 ? "" : "s"} configured — <a href="#/combos">edit list</a>.</div></div></div>
        <${Toggle} on=${s.vision.enabled} onChange=${(v) => save({ vision: { ...s.vision, enabled: v } }, v ? "Vision fallback on" : "Vision fallback off")} /></div>
    </div>
    <${PasswordCard} passwordSet=${data.passwordSet} relogin=${relogin} />`;
}
function PasswordCard({ passwordSet, relogin }) {
  const [cur, setCur] = useState(""), [pw, setPw] = useState(""), [pw2, setPw2] = useState("");
  const [busy, setBusy] = useState(false);
  const ok = pw.length >= 8 && pw === pw2 && (!passwordSet || cur);
  const submit = async () => {
    setBusy(true);
    try { await run(() => api("POST", "/api/auth/password", { current: cur, new: pw }), "Password saved — sign in again"); relogin(); }
    catch (e) { setBusy(false); }
  };
  return html`<div class="card">
    <div class="card-title"><div><h2><${Icon} n="lock" />Dashboard password</h2><p>${passwordSet ? "Changing it signs out every session." : "No password yet: the dashboard only opens from this machine. Set one before exposing it."}</p></div>
      <${Badge} kind=${passwordSet ? "success" : "warning"} dot>${passwordSet ? "Protected" : "Local only"}</${Badge}></div>
    <div class="grid g3">
      ${passwordSet && html`<${Field} label="Current password"><${SecretInput} value=${cur} onInput=${setCur} /></${Field}>`}
      <${Field} label="New password" hint="At least 8 characters."><${SecretInput} value=${pw} onInput=${setPw} /></${Field}>
      <${Field} label="Confirm" hint=${pw2 && pw !== pw2 ? "Doesn't match." : ""}><${SecretInput} value=${pw2} onInput=${setPw2} /></${Field}>
    </div>
    <div class="row" style="justify-content:flex-end;margin-top:14px"><button class="btn primary" disabled=${!ok || busy} onClick=${submit}><${Icon} n="save" />${passwordSet ? "Change password" : "Set password"}</button></div>
  </div>`;
}

/* ----------------------------------------------------------------- app */
const PAGES = { endpoint: EndpointPage, providers: ProvidersPage, combos: CombosPage, usage: UsagePage, quota: QuotaPage, keys: KeysPage, cli: CliPage, settings: SettingsPage };
function App() {
  const hash = useHash();
  const [dark, toggleTheme] = useTheme();
  const [auth, setAuth] = useState(null); // null = checking
  const [data, setData] = useState(null);
  const [err, setErr] = useState("");
  const reload = useCallback(async () => { try { setData(await api("GET", "/api/admin/state")); setErr(""); } catch (e) { setErr(e.message); } }, []);
  const check = useCallback(async () => {
    try {
      const s = await api("GET", "/api/auth/session");
      if (s.authenticated) { csrf = s.csrf; setAuth({ ok: true }); await reload(); }
      else { setData(null); setAuth({ ok: false, error: s.error || "" }); }
    } catch (e) { setAuth({ ok: false, error: e.message }); }
  }, []);
  useEffect(() => { onUnauthorized = check; check(); }, []);
  const logout = () => run(async () => { await api("POST", "/api/auth/logout", {}); csrf = ""; setData(null); setAuth({ ok: false }); });
  const go = (p) => { location.hash = "#/" + p; };

  if (!auth) return html`<div class="auth"><div class="grid-bg"></div></div>`;
  if (!auth.ok) return html`<${Login} error=${auth.error} onDone=${check} /><${Toasts} />`;
  const [path, query = ""] = hash.split("?");
  const [page, sub] = path.split("/");
  const meta = [...NAV, ...SYSTEM].find((x) => x.id === page) || NAV[0];
  const props = { data, reload, go, query, relogin: check };
  let body;
  if (!data) body = err ? html`<div class="notice err"><${Icon} n="error" />${err}</div>` : html`<div class="skeleton"></div><div class="skeleton"></div>`;
  else if (page === "providers" && sub) body = html`<${ProviderDetail} ...${props} id=${decodeURIComponent(sub)} />`;
  else { const Page = PAGES[meta.id]; body = html`<${Page} ...${props} />`; }
  return html`<div class="shell">
    <${Sidebar} page=${meta.id} data=${data} />
    <main class="main">
      <${Header} meta=${meta} dark=${dark} toggleTheme=${toggleTheme} onLogout=${logout} passwordSet=${data && data.passwordSet} />
      <div class="content"><div class="content-inner" key=${hash.split("?")[0]}>${body}</div></div>
    </main>
    <${Toasts} /></div>`;
}

render(html`<${App} />`, document.getElementById("app"));
