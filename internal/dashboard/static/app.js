// FastRouter dashboard: a small Preact + htm SPA (no build step).
// Structure and look follow 9Router's dashboard (MIT, decolua/9router).
import { html, render, useState, useEffect, useCallback, useRef } from "./vendor/preact-htm.js";

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
  if (p.kind && p.kind.endsWith("compatible")) {
    const anth = p.kind === "anthropic-compatible";
    return { name: p.name, color: anth ? "#D97757" : "#6366F1", logo: anth ? "logos/anthropic-m.png" : p.apiType === "responses" ? "logos/oai-r.png" : "logos/oai-cc.png" };
  }
  const m = PROVIDERS[p.id] || {};
  return { name: p.name || m.name || p.id, color: p.color || m.color || "#6366F1", key: p.keyUrl || m.key, logo: `logos/${p.id}.png` };
}
function Logo({ p, lg, xs }) {
  const m = provMeta(p);
  const [bad, setBad] = useState(false);
  return html`<div class="logo ${lg ? "lg" : ""} ${xs ? "xs" : ""}" style=${`background:${m.color}18`}>
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

/* -------------------------------------------------------- model picker */
// Searchable picker grouped by provider, after 9Router's ModelSelectModal.
const splitModel = (id) => { const i = id.indexOf("/"); return i < 0 ? ["", id] : [id.slice(0, i), id.slice(i + 1)]; };
const findProvider = (data, pre) => data.providers.find((p) => p.prefix === pre || p.id === pre || (p.aliases || []).includes(pre));
function modelInfo(data, id) {
  const combo = data.combos.find((c) => c.name === id);
  if (combo) return { combo, label: id };
  const [pre, m] = splitModel(id);
  const p = pre ? findProvider(data, pre) : null;
  if (!p) return { label: id };
  const g = (data.modelGroups || []).find((x) => x.provider === p.id);
  const mm = g && g.models.find((x) => x.id === p.prefix + "/" + m);
  return { p, label: m, name: mm && mm.name !== m ? mm.name : "" };
}
function ModelTag({ data, id }) {
  const i = modelInfo(data, id);
  return html`<span class="mtag" title=${i.name ? `${id} — ${i.name}` : id}>
    ${i.p ? html`<${Logo} p=${i.p} xs />` : html`<span class="mtag-ic"><${Icon} n=${i.combo ? "layers" : id.includes("*") ? "filter_alt" : "help"} /></span>`}
    ${i.p ? html`<span class="mtag-prov">${provMeta(i.p).name}</span>` : i.combo ? html`<span class="mtag-prov">Combo</span>` : ""}
    <span class="mono truncate">${i.label}</span></span>`;
}
// Space-separated terms must all match; "-term" excludes; "/re/" is a regex.
function matcher(q) {
  q = q.trim();
  if (!q) return () => true;
  if (q.length > 2 && q.startsWith("/") && q.endsWith("/")) {
    try { const re = new RegExp(q.slice(1, -1), "i"); return (s) => re.test(s); } catch (e) { return () => false; }
  }
  const terms = q.toLowerCase().split(/\s+/);
  return (s) => { s = s.toLowerCase(); return terms.every((t) => (t[0] === "-" ? t.length === 1 || !s.includes(t.slice(1)) : s.includes(t))); };
}
function ModelPicker({ data, selected = [], multi, onPick, onClose, title, combos = true, exclude }) {
  const [q, setQ] = useState("");
  const [custom, setCustom] = useState("");
  const [shut, setShut] = useState({});
  const ok = matcher(q);
  const sel = new Set(selected);
  const cs = combos ? data.combos.filter((c) => c.name !== exclude && ok(`combo ${c.name} ${c.models.join(" ")}`)) : [];
  const groups = (data.modelGroups || []).map((g) => {
    const p = data.providers.find((x) => x.id === g.provider) || { id: g.provider, name: g.name };
    return { g, p, models: g.models.filter((m) => ok(`${g.name} ${g.prefix} ${m.id} ${m.name}`)) };
  }).filter((x) => x.models.length);
  const total = cs.length + groups.reduce((n, x) => n + x.models.length, 0);
  const chip = (id, label, tag) => html`<button type="button" class="pick-chip ${sel.has(id) ? "on" : ""}" title=${id} onClick=${() => onPick(id)}>
    ${sel.has(id) && html`<${Icon} n="check" />`}<span class="truncate">${label}</span>${tag && html`<span class="tag">${tag}</span>`}</button>`;
  const addCustom = () => { const v = custom.trim(); if (v) { onPick(v); setCustom(""); } };
  return html`<${Modal} wide title=${title || (multi ? "Add models" : "Select model")} icon="neurology" onClose=${onClose}
    foot=${html`<span class="small muted grow">${multi ? `${selected.length} selected · click to add or remove` : "Click a model to choose it"}</span>
      <button class="btn primary" onClick=${onClose}>${multi ? "Done" : "Cancel"}</button>`}>
    <div class="picker">
      <div class="picker-search"><${Icon} n="search" /><input class="input" autofocus placeholder="Search models, providers or prefixes… (-exclude, /regex/)" value=${q} onInput=${(e) => setQ(e.target.value)} />
        <span class="small muted">${total} match${total === 1 ? "" : "es"}</span></div>
      <div class="picker-list">
        ${cs.length > 0 && html`<div class="pick-group"><div class="pick-head"><span class="mtag-ic"><${Icon} n="layers" /></span><span class="strong">Combos</span><span class="count">${cs.length}</span></div>
          <div class="pick-chips">${cs.map((c) => chip(c.name, c.name, c.strategy && c.strategy !== "fallback" ? STRATEGY[c.strategy].short : ""))}</div></div>`}
        ${groups.map(({ g, p, models }) => html`<div class="pick-group" key=${g.provider}>
          <button type="button" class="pick-head" onClick=${() => setShut({ ...shut, [g.provider]: !shut[g.provider] })}>
            <${Logo} p=${p} xs /><span class="strong">${g.name}</span><span class="mono small muted">${g.prefix}/</span>
            ${g.plugin && html`<${Badge} icon="extension">needs plugin</${Badge}>`}<span class="count">${models.length}</span>
            <span class="grow"></span><${Icon} n=${shut[g.provider] && !q ? "expand_more" : "expand_less"} /></button>
          ${!(shut[g.provider] && !q) && html`<div class="pick-chips">${models.map((m) => chip(m.id, m.name || splitModel(m.id)[1], m.custom ? "custom" : ""))}</div>`}</div>`)}
        ${total === 0 && html`<div class="empty" style="padding:28px"><div class="bubble"><${Icon} n="search_off" /></div><p class="small muted">No model matches “${q}”. Type its id below.</p></div>`}
      </div>
      <div class="row" style="gap:8px"><input class="input mono grow" placeholder="Not listed? Type prefix/model, e.g. ds/deepseek-chat" value=${custom}
          onInput=${(e) => setCustom(e.target.value)} onKeyDown=${(e) => { if (e.key === "Enter") { e.preventDefault(); addCustom(); } }} />
        <button type="button" class="btn secondary" disabled=${!custom.trim()} onClick=${addCustom}><${Icon} n="add" />Use</button></div>
    </div></${Modal}>`;
}
// One model chosen through the picker (router / judge model).
function ModelSelect({ data, value, onChange, placeholder, exclude }) {
  const [open, setOpen] = useState(false);
  return html`<div class="row" style="gap:8px">
    <button type="button" class="input model-select" onClick=${() => setOpen(true)}>${value ? html`<${ModelTag} data=${data} id=${value} />` : html`<span class="muted">${placeholder}</span>`}
      <span class="grow"></span><${Icon} n="unfold_more" /></button>
    ${value && html`<button type="button" class="icon-btn" title="Clear" onClick=${() => onChange("")}><${Icon} n="close" /></button>`}
    ${open && html`<${ModelPicker} data=${data} selected=${value ? [value] : []} exclude=${exclude} onPick=${(id) => { onChange(id); setOpen(false); }} onClose=${() => setOpen(false)} />`}</div>`;
}

function ModelListEditor({ models, setModels, data, exclude, globs }) {
  const [add, setAdd] = useState("");
  const [pick, setPick] = useState(false);
  const [props, cls] = useDrag(models, (a, b) => setModels(move(models, a, b)));
  const push = () => { const v = add.trim(); if (v && !models.includes(v)) setModels([...models, v]); setAdd(""); };
  const toggle = (id) => setModels(models.includes(id) ? models.filter((x) => x !== id) : [...models, id]);
  const small = "width:24px;height:24px";
  return html`<div class="col" style="gap:10px">
    <div class="model-list">
      ${models.length === 0 && html`<div class="section small muted" style="text-align:center">No models yet. Pick some below.</div>`}
      ${models.map((m, i) => html`<div class="model-item ${cls(i)}" key=${m} ...${props(i)}>
        <span class="handle" title="Drag to reorder"><${Icon} n="drag_indicator" /></span>
        <span class="idx">${i + 1}</span><${ModelTag} data=${data} id=${m} />
        <button type="button" class="icon-btn" style=${small} disabled=${i === 0} onClick=${() => setModels(move(models, i, i - 1))} aria-label="Move up"><${Icon} n="arrow_upward" style="font-size:15px" /></button>
        <button type="button" class="icon-btn" style=${small} disabled=${i === models.length - 1} onClick=${() => setModels(move(models, i, i + 1))} aria-label="Move down"><${Icon} n="arrow_downward" style="font-size:15px" /></button>
        <button type="button" class="icon-btn danger" style=${small} onClick=${() => setModels(models.filter((x) => x !== m))} aria-label="Remove"><${Icon} n="close" style="font-size:15px" /></button>
      </div>`)}
    </div>
    <div class="row">
      <button type="button" class="btn primary" onClick=${() => setPick(true)}><${Icon} n="add_circle" />Browse models</button>
      <input class="input mono grow" placeholder=${globs ? "or type a model, combo or glob like ds/*" : "or type prefix/model"} value=${add}
        onInput=${(e) => setAdd(e.target.value)} onKeyDown=${(e) => { if (e.key === "Enter") { e.preventDefault(); push(); } }} />
      <button type="button" class="btn secondary" disabled=${!add.trim()} onClick=${push}><${Icon} n="add" />Add</button>
    </div>
    ${pick && html`<${ModelPicker} multi data=${data} selected=${models} exclude=${exclude} onPick=${toggle} onClose=${() => setPick(false)} />`}
  </div>`;
}

/* --------------------------------------------------------------- shell */
const NAV = [
  { id: "endpoint", label: "Dashboard", icon: "api", title: "Endpoint & Overview", sub: "Your local endpoint, connected providers and today's traffic" },
  { id: "providers", label: "Providers", icon: "dns", title: "Providers", sub: "Accounts behind each model prefix" },
  { id: "combos", label: "Combos & Vision", icon: "layers", title: "Combos & Vision Adapter", sub: "Named model chains: fallback, round-robin, smart router or fusion" },
  { id: "usage", label: "Usage", icon: "bar_chart", title: "Usage", sub: "Requests, tokens and estimated cost" },
  { id: "quota", label: "Quota Tracker", icon: "data_usage", title: "Quota Tracker", sub: "Account health, cooldowns and failure rates" },
  { id: "keys", label: "API Keys", icon: "vpn_key", title: "API Keys", sub: "Local keys your tools use to call FastRouter" },
  { id: "token-saver", label: "Token Saver", icon: "savings", title: "Token Saver", sub: "Shrink what goes upstream: RTK, Caveman, Ponytail and Headroom" },
  { id: "cli", label: "CLI Tools", icon: "code_blocks", title: "CLI Tools", sub: "Copy-paste setup for coding agents and SDKs" },
];
const SYSTEM = [
  { id: "console-log", label: "Console Log", icon: "terminal", title: "Console Log", sub: "Live server log, redacted, newest at the bottom" },
  { id: "settings", label: "Settings", icon: "settings", title: "Settings", sub: "Thinking, routing strategy, vision fallback and password" },
];

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
        ${usage && usage.models && usage.models.length ? html`<table class="table"><tbody>${usage.models.slice(0, 6).map((r) => { const [pid, m] = splitModel(r.key); const p = data.providers.find((x) => x.id === pid);
          return html`<tr><td class="small">${p ? html`<div class="row" style="gap:8px"><${Logo} p=${p} xs /><span class="muted">${provMeta(p).name} /</span><span class="mono">${m}</span></div>` : html`<span class="mono">${r.key}</span>`}</td><td class="num">${fmt(r.requests)} req</td><td class="num muted">${money(r.cost_usd)}</td></tr>`; })}</tbody></table>`
          : html`<div class="empty" style="padding:20px"><p class="muted small">No requests in the last 24 hours.</p></div>`}</div>
      <div class="card"><div class="card-title"><h2><${Icon} n="info" />Import notes</h2><span class="muted small">${data.warnings.length}</span></div>
        <div class="col" style="gap:8px;max-height:260px;overflow:auto">${data.warnings.length === 0 ? html`<p class="muted small">Nothing to review.</p>` :
          data.warnings.slice(0, 8).map((w) => html`<div class="row" style="align-items:flex-start;gap:8px"><${Badge} kind="warning">${w.Kind}</${Badge}><span class="small muted">${w.Message}</span></div>`)}</div></div>
    </div>`;
}

/* ----------------------------------------------------------- providers */
function statusOf(p) {
  if (p.kind === "plugin") return html`<${Badge} icon="extension">Needs plugin</${Badge}>`;
  if (p.accounts.length === 0) return html`<span class="muted small">Not configured</span>`;
  const active = p.accounts.filter((a) => a.active);
  if (active.length === 0) return html`<${Badge} icon="pause_circle">Disabled</${Badge}>`;
  const cooling = active.filter((a) => a.cooling).length;
  const bad = active.filter((a) => a.expired || (a.status !== "ok" && a.status !== "needs-plugin")).length;
  return html`<${Badge} kind="success" dot>${p.usable} connected</${Badge}>
    ${cooling ? html`<${Badge} kind="warning" dot>${cooling} cooling</${Badge}>` : ""}
    ${bad ? html`<${Badge} kind="error" dot>${bad} error</${Badge}>` : ""}`;
}
const KIND_DOT = { apikey: "#f59e0b", login: "#3b82f6", plugin: "#9ca3af", "openai-compatible": "#f97316", "anthropic-compatible": "#f97316" };

const OTHER_KIND = { oauth: "Sign-in (OAuth)", webCookie: "Web session", free: "Free tier", freeTier: "Free tier" };
function ProvidersPage({ data, reload, go, query }) {
  const [modal, setModal] = useState(/(^|&)add\b/.test(query) ? { t: "account" } : null);
  const [q, setQ] = useState("");
  const [show, setShow] = useState("all");
  const ok = matcher(q);
  const toggleAll = (p, on) => run(async () => { for (const a of p.accounts) await api("PATCH", `/api/admin/accounts/${a.id}`, { active: on }); await reload(); }, on ? "Provider enabled" : "Provider disabled");
  const visible = (p) => ok(`${provMeta(p).name} ${p.id} ${p.prefix} ${(p.aliases || []).join(" ")}`) &&
    (show === "all" || (show === "configured") === (p.accounts.length > 0));
  const byName = (a, b) => (b.accounts.length > 0) - (a.accounts.length > 0) || provMeta(a).name.localeCompare(provMeta(b).name);
  const card = (p) => html`<div class="card xs hover ${p.accounts.length ? "" : "unset"}" key=${p.id} onClick=${() => go("providers/" + encodeURIComponent(p.id))}
      style=${p.accounts.length && p.accounts.every((a) => !a.active) ? "opacity:.55" : ""}>
    <div class="prov-card"><div class="row grow" style="gap:12px;min-width:0"><${Logo} p=${p} />
      <div class="grow" style="min-width:0"><div class="prov-name truncate">${provMeta(p).name}</div>
        <div class="prov-meta"><span class="sdot" style=${`background:${KIND_DOT[p.kind]}`}></span><span class="muted mono">${p.prefix}/</span>${statusOf(p)}</div></div></div>
      ${p.accounts.length > 0 && p.kind !== "plugin" ? html`<${Toggle} sm on=${p.accounts.some((a) => a.active)} title="Enable or disable every account" onChange=${(v) => toggleAll(p, v)} />`
        : p.kind === "apikey" && html`<span class="add-hint"><${Icon} n="add" /></span>`}
    </div></div>`;
  const section = (title, icon, all, action, note) => {
    const list = all.filter(visible).sort(byName);
    const conf = all.filter((p) => p.accounts.length > 0).length;
    if (!list.length && (q || show !== "all")) return "";
    return html`<div>
      <div class="sec-head"><h2><${Icon} n=${icon} style="color:var(--primary)" />${title}<span class="count">${conf} / ${all.length}</span></h2>${action}</div>
      ${note && html`<p class="small muted" style="margin:-4px 0 12px">${note}</p>`}
      <div class="prov-grid">${list.map(card)}</div></div>`;
  };
  const builtin = data.providers.filter((p) => p.kind === "apikey");
  const logins = data.providers.filter((p) => p.kind === "login" || p.kind === "plugin");
  const nodes = data.providers.filter((p) => p.kind.endsWith("compatible"));
  const others = show === "configured" ? [] : (data.others || []).filter((o) => ok(`${o.name} ${o.id}`));
  const configured = data.providers.filter((p) => p.accounts.length > 0).length;
  return html`
    <div class="prov-bar">
      <div class="picker-search grow" style="max-width:420px"><${Icon} n="search" /><input class="input" placeholder="Search ${data.providers.length + (data.others || []).length} providers…" value=${q} onInput=${(e) => setQ(e.target.value)} /></div>
      <${Seg} value=${show} onChange=${setShow} options=${[["all", "All"], ["configured", `Configured · ${configured}`], ["available", "Not configured"]]} />
      <span class="grow"></span>
      <button class="btn outline" onClick=${() => setModal({ t: "node" })}><${Icon} n="link" />Add endpoint</button>
      <button class="btn primary" onClick=${() => setModal({ t: "account" })}><${Icon} n="add" />Add account</button></div>
    <div class="legend">
      <span><i style="background:#f59e0b"></i>API key</span><span><i style="background:#3b82f6"></i>Official sign-in</span>
      <span><i style="background:#f97316"></i>Compatible endpoint</span><span><i style="background:#9ca3af"></i>Needs plugin</span></div>
    ${section("API key providers", "vpn_key", builtin, null, "Paste a key to route prefix/model calls through any of these. Click one to add an account.")}
    ${section("Sign-in providers", "verified", logins)}
    ${section("Custom endpoints", "link", nodes, html`<button class="btn ghost sm" onClick=${() => setModal({ t: "node" })}><${Icon} n="add" />New endpoint</button>`)}
    ${others.length > 0 && html`<div>
      <div class="sec-head"><h2><${Icon} n="extension" style="color:var(--text-muted)" />More 9Router providers<span class="count">${others.length}</span></h2></div>
      <p class="small muted" style="margin:-4px 0 12px">These sign in with browser sessions, OAuth apps or CLI tokens that FastRouter core does not ship. Keep using them through 9Router, or add their OpenAI-compatible URL as a custom endpoint.</p>
      <div class="prov-grid">${others.map((o) => html`<div class="card xs other" key=${o.id} title="Not supported by FastRouter yet">
        <div class="prov-card"><div class="row grow" style="gap:12px;min-width:0"><${Logo} p=${o} />
          <div class="grow" style="min-width:0"><div class="prov-name truncate">${o.name}</div>
            <div class="prov-meta"><span class="sdot" style="background:#9ca3af"></span><span class="muted small">${OTHER_KIND[o.category] || o.category}</span></div></div></div>
          <${Badge}>Not supported</${Badge}></div></div>`)}</div></div>`}
    ${modal && modal.t === "account" && html`<${AccountForm} data=${data} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "node" && html`<${NodeForm} onClose=${() => setModal(null)} onSaved=${reload} />`}`;
}

function AccountForm({ data, provider, account, onClose, onSaved }) {
  const choices = data.providers.filter((p) => p.kind === "apikey" || p.kind.endsWith("compatible")).sort((a, b) => provMeta(a).name.localeCompare(provMeta(b).name));
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
function ModelChain({ data, models, empty, sep = "chevron_right" }) {
  if (!models.length) return html`<span class="small muted">${empty}</span>`;
  return html`<div class="combo-models">${models.map((m, i) => html`${i > 0 && html`<span class="arrow-sep"><${Icon} n=${sep} style="font-size:16px" /></span>`}<${ModelTag} data=${data} id=${m} />`)}</div>`;
}
// The four combo strategies 9Router offers (combos/page.js STRATEGY_OPTIONS).
const STRATEGY = {
  fallback: { label: "Fallback", short: "Fallback", icon: "low_priority", badge: "primary", desc: "Always start with model 1; move to the next when it fails." },
  "round-robin": { label: "Round-robin", short: "Round-robin", icon: "autorenew", badge: "info", desc: "Rotate the starting model on every request to spread load." },
  smart: { label: "Smart Router", short: "Smart", icon: "auto_awesome", badge: "warning", desc: "System One style: a router model reads the request and picks the best model; sticky per conversation." },
  fusion: { label: "Fusion", short: "Fusion", icon: "merge", badge: "success", desc: "Ask every model in parallel (the panel), then a judge model writes one answer from their replies." },
};
const POLICY = [["smart", "Smart"], ["strict", "Strict"], ["per-turn", "Per-turn"]];
const POLICY_HINT = { smart: "Keep the conversation's model; switch only when the router is confident.", strict: "Pick once per conversation and never switch.", "per-turn": "Ask the router on every turn and follow its pick." };
function ComboForm({ data, combo, onClose, onSaved }) {
  const [name, setName] = useState(combo ? combo.name : "");
  const [strategy, setStrategy] = useState(combo ? combo.strategy || "fallback" : "fallback");
  const [models, setModels] = useState(combo ? combo.models.slice() : []);
  const [policy, setPolicy] = useState((combo && combo.smartPolicy) || "smart");
  const [threshold, setThreshold] = useState(String((combo && combo.confidenceThreshold) || 0.8));
  const [routerModel, setRouterModel] = useState((combo && combo.routerModel) || "");
  const [judgeModel, setJudgeModel] = useState((combo && combo.judgeModel) || "");
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    try {
      const body = { name, strategy, models };
      if (strategy === "smart") Object.assign(body, { smartPolicy: policy, confidenceThreshold: Math.min(1, Math.max(0.1, parseFloat(threshold) || 0.8)), routerModel });
      if (strategy === "fusion") body.judgeModel = judgeModel;
      if (combo) await run(() => api("PATCH", `/api/admin/combos/${combo.id}`, body), "Combo saved");
      else await run(() => api("POST", "/api/admin/combos", body), "Combo created");
      await onSaved(); onClose();
    } catch (e) { setBusy(false); }
  };
  const self = combo ? combo.name : name;
  return html`<${Modal} wide title=${combo ? "Edit combo" : "New combo"} icon="layers" onClose=${onClose}
    foot=${html`<button class="btn ghost" onClick=${onClose}>Cancel</button><button class="btn primary" disabled=${busy || !name || !models.length || (strategy === "fusion" && models.length < 2)} onClick=${save}><${Icon} n="save" />Save combo</button>`}>
    <${Field} label="Combo name" hint="Clients call it as the model name."><input class="input mono" value=${name} placeholder="my-combo" onInput=${(e) => setName(e.target.value)} /></${Field}>
    <${Field} label="Strategy"><div class="strat-grid">${Object.entries(STRATEGY).map(([k, v]) => html`
      <button type="button" class="strat ${strategy === k ? "on" : ""}" onClick=${() => setStrategy(k)}>
        <span class="strat-ic"><${Icon} n=${v.icon} /></span><span class="col" style="gap:2px;align-items:flex-start"><span class="strong">${v.label}</span><span class="small muted">${v.desc}</span></span>
      </button>`)}</div></${Field}>
    ${strategy === "smart" && html`<div class="section col" style="gap:14px;padding:14px">
      <div class="grid g2"><${Field} label="Policy" hint=${POLICY_HINT[policy]}><${Seg} value=${policy} onChange=${setPolicy} options=${POLICY} /></${Field}>
        <${Field} label="Confidence to switch" hint="0.1–1. Used by the Smart policy."><input class="input" type="number" min="0.1" max="1" step="0.05" value=${threshold} disabled=${policy !== "smart"} onInput=${(e) => setThreshold(e.target.value)} /></${Field}></div>
      <${Field} label="Router model" hint="A fast, cheap model that answers with JSON. Fails open to the listed order after 8s."><${ModelSelect} data=${data} value=${routerModel} onChange=${setRouterModel} exclude=${self} placeholder="Default: model 1 of this combo" /></${Field}>
    </div>`}
    ${strategy === "fusion" && html`<div class="section col" style="gap:14px;padding:14px">
      <${Field} label="Judge model" hint="Reads the panel's answers and writes the final reply, streamed to the client. Quorum of 2, 8s grace for stragglers, 90s cap."><${ModelSelect} data=${data} value=${judgeModel} onChange=${setJudgeModel} exclude=${self} placeholder="Default: model 1 of the panel" /></${Field}>
    </div>`}
    <${Field} label=${strategy === "fusion" ? "Panel models" : strategy === "smart" ? "Candidate models" : "Models"} hint=${strategy === "fusion" ? "At least two. All are asked in parallel; tools are stripped for the panel." : strategy === "smart" ? "The router picks one of these; the rest stay as fallbacks in this order." : "Drag to set the order. Each entry is prefix/model or another combo's name."}>
      <${ModelListEditor} models=${models} setModels=${setModels} data=${data} exclude=${self} /></${Field}>
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
              <${Badge} kind=${(STRATEGY[c.strategy] || STRATEGY.fallback).badge} icon=${(STRATEGY[c.strategy] || STRATEGY.fallback).icon}>${(STRATEGY[c.strategy] || STRATEGY.fallback).label}</${Badge}>
              ${c.strategy === "smart" && html`<span class="small muted">${c.smartPolicy || "smart"} policy</span>`}
              ${c.kind && html`<${Badge}>${c.kind}</${Badge}>`}<span class="small muted">${c.models.length} model${c.models.length === 1 ? "" : "s"}</span></div></div></div>
            <div class="row" style="gap:2px"><button class="icon-btn" title="Edit" onClick=${() => setModal({ t: "edit", c })}><${Icon} n="edit" /></button>
              <button class="icon-btn danger" title="Delete" onClick=${() => setModal({ t: "del", c })}><${Icon} n="delete" /></button></div></div>
          <${ModelChain} data=${data} models=${c.models} sep=${c.strategy === "fusion" ? "add" : "chevron_right"} empty="Empty — add models to use it." />
          ${c.strategy === "fusion" && html`<div class="small muted" style="margin-top:10px"><${Icon} n="gavel" style="font-size:14px;vertical-align:-2px" /> Judge: ${c.judgeModel ? html`<${ModelTag} data=${data} id=${c.judgeModel} />` : "model 1"}</div>`}
          ${c.strategy === "smart" && html`<div class="small muted" style="margin-top:10px"><${Icon} n="alt_route" style="font-size:14px;vertical-align:-2px" /> Router: ${c.routerModel ? html`<${ModelTag} data=${data} id=${c.routerModel} />` : "model 1"}</div>`}
        </div>`)}</div>`}
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="image" />Vision adapter</h2><p>When a request carries images and the chosen model can't read them, FastRouter tries these vision models first.</p></div>
        <${Toggle} on=${v.enabled} onChange=${(on) => saveVision({ ...v, enabled: on })} /></div>
      <div class="row between section" style="padding:12px 14px;margin-bottom:14px"><div><div class="medium">Rotation</div><div class="small muted">Fallback keeps the order; round-robin spreads image requests.</div></div>
        <${Seg} value=${v.roundRobin ? "rr" : "fb"} onChange=${(x) => saveVision({ ...v, roundRobin: x === "rr" })} options=${[["fb", "Fallback"], ["rr", "Round-robin"]]} /></div>
      ${vm ? html`<${ModelListEditor} models=${vm} setModels=${setVm} data=${data} />
          <div class="row" style="justify-content:flex-end;margin-top:12px;gap:8px"><button class="btn ghost" onClick=${() => setVm(null)}>Cancel</button>
            <button class="btn primary" onClick=${async () => { await saveVision({ ...v, models: vm }); setVm(null); }}><${Icon} n="save" />Save models</button></div>`
        : html`<div class="row between" style="align-items:flex-start"><div class="grow"><${ModelChain} data=${data} models=${v.models} empty="No vision models set." /></div>
            <button class="btn outline sm" onClick=${() => setVm(v.models.slice())}><${Icon} n="edit" />Edit models</button></div>`}
    </div>
    ${modal && modal.t === "new" && html`<${ComboForm} data=${data} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "edit" && html`<${ComboForm} data=${data} combo=${modal.c} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "del" && html`<${Confirm} title="Delete combo?" text=${`Clients calling "${modal.c.name}" will get "model not found".`} onClose=${() => setModal(null)}
      onYes=${() => run(async () => { await api("DELETE", `/api/admin/combos/${modal.c.id}`); await reload(); }, "Combo deleted")} />`}`;
}

/* --------------------------------------------------------------- usage */
function UsageTable({ data, title, icon, list, label }) {
  const max = Math.max(1, ...list.map((r) => r.requests));
  return html`<div class="card flush"><div class="row between" style="padding:16px 18px"><h2 style="font-size:16px;font-weight:600" class="row"><${Icon} n=${icon} style="color:var(--primary)" />${title}</h2></div>
    ${list.length === 0 ? html`<p class="small muted" style="padding:0 18px 18px">No requests in this window.</p>` : html`<table class="table"><thead><tr><th>${label}</th><th>Requests</th><th class="num">Tokens in / out</th><th class="num">Cached</th><th class="num">Avg latency</th><th class="num">Cost</th></tr></thead>
      <tbody>${list.map((r) => { const [pid, model] = label === "Model" ? splitModel(r.key) : [r.provider || r.key, ""];
        const p = data && data.providers.find((x) => x.id === (label === "Provider" ? r.key : pid));
        const name = label === "Provider" || label === "Model" ? "" : r.name || r.key;
        return html`<tr><td class="small">${p ? html`<div class="row" style="gap:8px;min-width:0"><${Logo} p=${p} xs />${name && html`<span class="mono">${name}</span><span class="muted">·</span>`}<span class=${name ? "muted" : ""}>${provMeta(p).name}</span>${model && html`<span class="muted">/</span><span class="mono truncate">${model}</span>`}</div>`
          : html`<span class="mono">${r.name || r.key}</span>`}</td>
        <td><div class="row" style="gap:8px"><div class="bar grow"><i style=${`width:${(100 * r.requests) / max}%`}></i></div><span class="small" style="min-width:42px">${fmt(r.requests)}</span>${r.failures ? html`<${Badge} kind="error">${r.failures} failed</${Badge}>` : ""}</div></td>
        <td class="num small">${fmt(r.input_tokens)} / ${fmt(r.output_tokens)}</td><td class="num small muted">${fmt(r.cache_read_tokens)}</td><td class="num small">${r.avg_latency_ms} ms</td><td class="num small">${money(r.cost_usd)}</td></tr>`; })}</tbody></table>`}</div>`;
}
// Usage page, after 9Router's usage/page.js: Overview / Details tabs, period
// pills, five stat cards, provider topology, recent requests and a chart.
const PERIODS = [["today", "Today"], ["24h", "24h"], ["7d", "7D"], ["30d", "30D"], ["60d", "60D"], ["all", "All"]];
function UsagePage({ data }) {
  const [tab, setTab] = useState("overview");
  const [period, setPeriod] = useState("7d");
  const [u, setU] = useState(null);
  useEffect(() => {
    let stop = false, t;
    const load = () => api("GET", "/api/usage?period=" + period).then((x) => { if (!stop) setU(x); }, (e) => !stop && setU({ error: e.message }))
      .finally(() => { if (!stop) t = setTimeout(load, 5000); });
    setU(null); load();
    return () => { stop = true; clearTimeout(t); };
  }, [period]);
  return html`
    <div class="row between" style="flex-wrap:wrap;gap:12px">
      <div class="tabs">${[["overview", "Overview", "dashboard"], ["details", "Details", "receipt_long"]].map(([k, l, ic]) => html`
        <button class=${tab === k ? "on" : ""} onClick=${() => setTab(k)}><${Icon} n=${ic} />${l}</button>`)}</div>
      <div class="pills">${PERIODS.map(([k, l]) => html`<button class=${period === k ? "on" : ""} onClick=${() => setPeriod(k)}>${l}</button>`)}</div>
    </div>
    ${u && u.error && html`<div class="notice err"><${Icon} n="error" />${u.error}</div>`}
    ${tab === "overview" ? html`<${UsageOverview} data=${data} u=${u} onDetails=${() => setTab("details")} />` : html`<${UsageDetails} data=${data} period=${period} />`}`;
}
function UsageCards({ u }) {
  const rows = (u && u.providers) || [];
  const t = rows.reduce((a, r) => ({ req: a.req + r.requests, fail: a.fail + r.failures, inp: a.inp + r.input_tokens, out: a.out + r.output_tokens, cache: a.cache + r.cache_read_tokens, cost: a.cost + r.cost_usd }), { req: 0, fail: 0, inp: 0, out: 0, cache: 0, cost: 0 });
  const cards = [["speed", "Total requests", fmt(t.req), t.fail ? `${fmt(t.fail)} failed` : "all succeeded", ""],
    ["input", "Total input tokens", fmt(t.inp), "prompt side", "orange"],
    ["cached", "Cached tokens", fmt(t.cache), t.inp ? `${Math.round((100 * t.cache) / t.inp)}% of input` : "prompt cache hits", "blue"],
    ["output", "Output tokens", fmt(t.out), "including reasoning", "green"],
    ["payments", "Est. cost", money(t.cost), "Estimated, not actual billing", "yellow"]];
  return html`<div class="grid g5">${cards.map(([ic, l, v, sub, c]) => html`<div class="card sm stat ${c}">
    <span class="label"><${Icon} n=${ic} />${l}</span><span class="value">${u ? v : "…"}</span><span class="sub">${sub}</span></div>`)}</div>`;
}
function UsageOverview({ data, u, onDetails }) {
  const [chart, setChart] = useState("tokens");
  return html`
    <${UsageCards} u=${u} />
    <div class="usage-row">
      <div class="card flush grow" style="min-width:0"><div class="row between" style="padding:14px 18px 0">
        <h2 class="row h-sm"><${Icon} n="hub" style="color:var(--primary)" />Provider topology</h2>
        <span class="small muted">${u && Object.keys(u.active || {}).length ? html`<span class="live-dot"></span>live` : "last request highlighted"}</span></div>
        <${Topology} data=${data} u=${u} /></div>
      <div class="card flush recent"><div class="row between" style="padding:14px 16px 10px;border-bottom:1px solid var(--border-subtle)">
        <h2 class="row h-sm"><${Icon} n="history" style="color:var(--primary)" />Recent requests</h2><button class="btn ghost sm" onClick=${onDetails}>View all<${Icon} n="chevron_right" /></button></div>
        <div class="recent-list">${!u ? html`<div class="skeleton" style="margin:12px"></div>` : (u.recent || []).length === 0 ? html`<p class="small muted" style="padding:16px">No requests yet.</p>`
          : u.recent.map((r, i) => { const p = data.providers.find((x) => x.id === r.provider); return html`<div class="recent-item" key=${i}>
            ${p ? html`<${Logo} p=${p} xs />` : html`<span class="mtag-ic"><${Icon} n="help" /></span>`}
            <div class="grow" style="min-width:0"><div class="row" style="gap:6px"><span class="mono small truncate strong">${r.model}</span>${r.status >= 400 || r.status === 0 ? html`<span class="sdot" style="background:var(--danger)" title=${"HTTP " + r.status}></span>` : ""}</div>
              <div class="tiny muted row" style="gap:8px"><span class="tok-in">${fmt(r.input_tokens)}↑</span><span class="tok-out">${fmt(r.output_tokens)}↓</span><span>${p ? provMeta(p).name : r.provider}</span></div></div>
            <span class="tiny muted nowrap">${ago(r.at)}</span></div>`; })}</div></div>
    </div>
    <div class="card"><div class="row between" style="margin-bottom:10px">
      <h2 class="row h-sm"><${Icon} n="show_chart" style="color:var(--primary)" />${u && (u.period === "today" || u.period === "24h") ? "By hour" : "By day"}</h2>
      <div class="pills sm">${[["tokens", "Tokens"], ["requests", "Requests"], ["cost", "Cost"]].map(([k, l]) => html`<button class=${chart === k ? "on" : ""} onClick=${() => setChart(k)}>${l}</button>`)}</div></div>
      ${u ? html`<${AreaChart} points=${u.series || []} metric=${chart} />` : html`<div class="skeleton"></div>`}</div>
    ${u ? html`<${UsageTable} data=${data} title="By provider" icon="dns" list=${u.providers || []} label="Provider" />
      <${UsageTable} data=${data} title="By model" icon="memory" list=${u.models || []} label="Model" />
      <${UsageTable} data=${data} title="By account" icon="group" list=${u.accounts || []} label="Account" />
      <${UsageTable} title="By API key" icon="vpn_key" list=${u.keys || []} label="Key" />` : ""}`;
}

// SVG topology: FastRouter in the middle, providers on an ellipse ring.
// The edge of the last request is orange (red if it failed); live ones animate.
function Topology({ data, u }) {
  const W = 900, H = 500, cx = W / 2, cy = H / 2;
  const [view, setView] = useState({ k: 1, x: 0, y: 0 });
  const [drag, setDrag] = useState(null);
  const used = new Set(((u && u.providers) || []).map((r) => r.key));
  const nodes = data.providers.filter((p) => p.accounts.length > 0 || used.has(p.id));
  const stats = Object.fromEntries(((u && u.providers) || []).map((r) => [r.key, r]));
  const last = u && u.recent && u.recent[0];
  const active = (u && u.active) || {};
  // Crowded rings alternate between an outer and an inner ellipse.
  const n = Math.max(nodes.length, 1), two = n > 12;
  const pos = (i) => { const a = -Math.PI / 2 + (i * 2 * Math.PI) / n, f = two && i % 2 ? 0.66 : 1; return [cx + 370 * f * Math.cos(a), cy + 205 * f * Math.sin(a)]; };
  const zoom = (f) => setView((v) => ({ ...v, k: Math.min(3, Math.max(0.4, v.k * f)) }));
  const fit = () => setView({ k: 1, x: 0, y: 0 });
  const t = `translate(${cx + view.x} ${cy + view.y}) scale(${view.k}) translate(${-cx} ${-cy})`;
  const down = (e) => { setDrag({ sx: e.clientX, sy: e.clientY, x: view.x, y: view.y, w: e.currentTarget.getBoundingClientRect().width }); };
  const moveP = (e) => { if (!drag) return; const f = W / drag.w; setView((v) => ({ ...v, x: drag.x + (e.clientX - drag.sx) * f, y: drag.y + (e.clientY - drag.sy) * f })); };
  return html`<div class="topo-wrap">
    <svg viewBox="0 0 ${W} ${H}" class="topo-svg ${drag ? "grabbing" : ""}" onPointerDown=${down} onPointerMove=${moveP} onPointerUp=${() => setDrag(null)} onPointerLeave=${() => setDrag(null)}
      onWheel=${(e) => { e.preventDefault(); zoom(e.deltaY < 0 ? 1.1 : 0.9); }}>
      <defs><radialGradient id="hubg" cx="50%" cy="35%" r="70%"><stop offset="0" stop-color="#fb923c" /><stop offset="1" stop-color="#ea580c" /></radialGradient></defs>
      <g transform=${t}>
        <ellipse cx=${cx} cy=${cy} rx="370" ry="205" class="topo-ring" />${two && html`<ellipse cx=${cx} cy=${cy} rx=${370 * 0.66} ry=${205 * 0.66} class="topo-ring" />`}
        ${nodes.map((p, i) => {
          const [x, y] = pos(i);
          const isLast = last && last.provider === p.id, bad = isLast && (last.status >= 400 || last.status === 0), live = active[p.id] > 0;
          return html`<line key=${"e" + p.id} x1=${cx} y1=${cy} x2=${x} y2=${y} class="topo-edge ${live ? "live" : ""} ${isLast ? (bad ? "err" : "last") : ""} ${stats[p.id] ? "used" : ""}" />`;
        })}
        ${nodes.map((p, i) => {
          const [x, y] = pos(i);
          const m = provMeta(p), st = stats[p.id], isLast = last && last.provider === p.id;
          const label = m.name.length > 16 ? m.name.slice(0, 15) + "…" : m.name;
          return html`<g key=${"n" + p.id} class="topo-pnode ${isLast ? "last" : ""} ${st ? "" : "idle"}" transform="translate(${x} ${y})">
            <title>${m.name} · ${p.prefix}/${st ? ` · ${st.requests} requests · ${fmt(st.input_tokens + st.output_tokens)} tokens` : ""}</title>
            <rect x="-62" y="-20" width="124" height="40" rx="12" />
            <circle cx="-42" cy="0" r="13" fill=${m.color + "22"} /><image href=${m.logo} x="-53" y="-11" width="22" height="22" />
            <text x="-24" y="-2" class="nm">${label}</text><text x="-24" y="12" class="sub">${st ? `${fmt(st.requests)} req · ${fmt(st.input_tokens + st.output_tokens)} tok` : p.prefix + "/"}</text>
            ${active[p.id] > 0 && html`<circle cx="56" cy="-14" r="5" class="topo-live" />`}</g>`;
        })}
        <g transform="translate(${cx} ${cy})" class="topo-hub">
          <circle r="54" class="halo" /><circle r="44" fill="url(#hubg)" />
          <path d="M13 2L3 14h7l-1 8 10-12h-7z" transform="translate(-14 -30) scale(1.15)" fill="#fff" />
          <text y="16" text-anchor="middle" class="hub-t">FastRouter</text>
          <text y="74" text-anchor="middle" class="hub-s">${nodes.length} provider${nodes.length === 1 ? "" : "s"}</text></g>
      </g>
      ${nodes.length === 0 && html`<text x=${cx} y=${cy + 90} text-anchor="middle" class="hub-s">Add a provider account to see it here</text>`}
    </svg>
    <div class="zoom">
      <button class="icon-btn" title="Zoom in" onClick=${() => zoom(1.2)}><${Icon} n="zoom_in" /></button>
      <button class="icon-btn" title="Zoom out" onClick=${() => zoom(1 / 1.2)}><${Icon} n="zoom_out" /></button>
      <button class="icon-btn" title="Fit" onClick=${fit}><${Icon} n="fit_screen" /></button></div>
    ${last && html`<div class="topo-legend"><span><i class="l-last"></i>last request</span><span><i class="l-live"></i>in flight</span><span><i class="l-err"></i>failed</span></div>`}
  </div>`;
}

const METRIC = { tokens: { color: "#f97316", f: fmt, label: "tokens" }, requests: { color: "#3b82f6", f: fmt, label: "requests" }, cost: { color: "#eab308", f: money, label: "est. cost" } };
function AreaChart({ points, metric }) {
  const [hover, setHover] = useState(-1);
  const M = METRIC[metric], W = 900, H = 210, L = 52, R = 28, T = 12, B = 28;
  if (!points.length) return html`<p class="small muted">No data.</p>`;
  const vals = points.map((p) => p[metric] || 0);
  const max = Math.max(...vals) || 1;
  const step = (W - L - R) / Math.max(points.length - 1, 1);
  const X = (i) => L + i * step, Y = (v) => T + (H - T - B) * (1 - v / max);
  const line = vals.map((v, i) => `${i ? "L" : "M"}${X(i).toFixed(1)},${Y(v).toFixed(1)}`).join(" ");
  const area = `${line} L${X(vals.length - 1)},${H - B} L${L},${H - B} Z`;
  const every = Math.ceil(points.length / 10);
  const onMove = (e) => { const r = e.currentTarget.getBoundingClientRect(); const x = ((e.clientX - r.left) / r.width) * W; setHover(Math.max(0, Math.min(points.length - 1, Math.round((x - L) / step)))); };
  const id = "ag-" + metric;
  return html`<div class="chart-wrap"><svg viewBox="0 0 ${W} ${H}" class="chart" onMouseMove=${onMove} onMouseLeave=${() => setHover(-1)}>
    <defs><linearGradient id=${id} x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color=${M.color} stop-opacity=".35" /><stop offset="1" stop-color=${M.color} stop-opacity="0" /></linearGradient></defs>
    ${[0, 0.25, 0.5, 0.75, 1].map((f) => html`<line x1=${L} x2=${W - R} y1=${Y(max * f)} y2=${Y(max * f)} class="grid-l" /><text x=${L - 8} y=${Y(max * f) + 4} text-anchor="end" class="axis">${M.f(metric === "cost" ? max * f : Math.round(max * f))}</text>`)}
    ${points.map((p, i) => i % every === 0 && html`<text x=${X(i)} y=${H - 8} text-anchor="middle" class="axis">${p.label}</text>`)}
    <path d=${area} fill=${`url(#${id})`} /><path d=${line} fill="none" stroke=${M.color} stroke-width="2.2" stroke-linejoin="round" />
    ${hover >= 0 && html`<line x1=${X(hover)} x2=${X(hover)} y1=${T} y2=${H - B} class="hover-l" /><circle cx=${X(hover)} cy=${Y(vals[hover])} r="4.5" fill=${M.color} stroke="#fff" stroke-width="2" />`}
  </svg>${hover >= 0 && html`<div class="chart-tip" style=${`left:${(100 * X(hover)) / W}%`}><div class="strong">${points[hover].label}</div>
    <div>${fmt(points[hover].tokens)} tokens</div><div>${fmt(points[hover].requests)} requests</div><div>${money(points[hover].cost)}</div></div>`}</div>`;
}

function UsageDetails({ data, period }) {
  const [prov, setProv] = useState("");
  const [page, setPage] = useState(0);
  const [r, setR] = useState(null);
  const size = 50;
  useEffect(() => { setPage(0); }, [period, prov]);
  useEffect(() => {
    setR(null);
    api("GET", `/api/usage/requests?period=${period}&limit=${size}&offset=${page * size}${prov ? "&provider=" + encodeURIComponent(prov) : ""}`).then(setR, (e) => setR({ error: e.message }));
  }, [period, prov, page]);
  const provs = data.providers.filter((p) => p.accounts.length > 0);
  const total = (r && r.total) || 0;
  return html`<div class="card flush">
    <div class="row between" style="padding:14px 18px;gap:12px;flex-wrap:wrap"><h2 class="row h-sm"><${Icon} n="receipt_long" style="color:var(--primary)" />Requests<span class="count">${fmt(total)}</span></h2>
      <select class="input" style="width:240px" value=${prov} onChange=${(e) => setProv(e.target.value)}><option value="">All providers</option>
        ${provs.map((p) => html`<option value=${p.id}>${provMeta(p).name}</option>`)}</select></div>
    ${!r ? html`<div class="skeleton" style="margin:0 18px 18px"></div>` : r.error ? html`<div class="notice err" style="margin:0 18px 18px">${r.error}</div>` : r.requests.length === 0 ? html`<p class="small muted" style="padding:0 18px 18px">No requests in this period.</p>`
      : html`<table class="table"><thead><tr><th>Time</th><th>Provider</th><th>Model</th><th>Account</th><th>Status</th><th class="num">In ↑</th><th class="num">Out ↓</th><th class="num">Cached</th><th class="num">Latency</th><th class="num">Cost</th></tr></thead>
        <tbody>${r.requests.map((x, i) => { const p = data.providers.find((y) => y.id === x.provider); return html`<tr key=${i}>
          <td class="small nowrap" title=${x.at}>${new Date(x.at).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" })}</td>
          <td><div class="row" style="gap:8px">${p && html`<${Logo} p=${p} xs />`}<span class="small">${p ? provMeta(p).name : x.provider}</span></div></td>
          <td class="mono small">${x.model}</td><td class="small muted">${x.account}</td>
          <td>${x.status >= 200 && x.status < 300 ? html`<${Badge} kind="success">${x.status}</${Badge}>` : html`<${Badge} kind="error">${x.status || "net"}</${Badge}>`}</td>
          <td class="num small tok-in">${fmt(x.input_tokens)}</td><td class="num small tok-out">${fmt(x.output_tokens)}</td><td class="num small muted">${fmt(x.cache_read_tokens)}</td>
          <td class="num small">${x.latency_ms} ms</td><td class="num small">${money(x.cost_usd)}</td></tr>`; })}</tbody></table>`}
    ${total > size && html`<div class="row between" style="padding:12px 18px;border-top:1px solid var(--border-subtle)"><span class="small muted">${page * size + 1}–${Math.min(total, (page + 1) * size)} of ${fmt(total)}</span>
      <div class="row" style="gap:6px"><button class="btn outline sm" disabled=${page === 0} onClick=${() => setPage(page - 1)}><${Icon} n="chevron_left" />Prev</button>
        <button class="btn outline sm" disabled=${(page + 1) * size >= total} onClick=${() => setPage(page + 1)}>Next<${Icon} n="chevron_right" /></button></div></div>`}
  </div>`;
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
    ${restricted && html`<${ModelListEditor} models=${allow} setModels=${setAllow} data=${data} globs />`}
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
function TokenSaverPage({ data, reload }) {
  const s = data.settings;
  const locked = (k) => s.lockedByConfig.includes(k);
  const save = (body, ok = "Settings saved") => run(async () => { await api("PATCH", "/api/admin/settings", body); await reload(); }, ok);
  const [hrUrl, setHrUrl] = useState(s.headroomUrl);
  const Lock = ({ k }) => locked(k) && html`<span class="lock" title="Set in the config file"><${Icon} n="lock" />config file</span>`;
  const saver = (icon, title, desc, enabledKey, levelKey, lockKey, extra) => html`<div class="set-row" style="align-items:flex-start">
    <div class="saver-head" style="align-items:flex-start"><div class="saver-icon"><${Icon} n=${icon} /></div>
      <div><div class="t">${title}<${Lock} k=${lockKey} /></div><div class="d">${desc}</div>${extra}</div></div>
    <div class="row" style="gap:12px">${levelKey && s[enabledKey] && html`<${Seg} value=${s[levelKey]} disabled=${locked(lockKey)} onChange=${(v) => save({ [levelKey]: v })} options=${[["lite", "Lite"], ["full", "Full"], ["ultra", "Ultra"]]} />`}
      <${Toggle} on=${s[enabledKey]} disabled=${locked(lockKey)} onChange=${(v) => save({ [enabledKey]: v }, v ? title + " on" : title + " off")} /></div></div>`;
  const on = [s.rtkEnabled, s.cavemanEnabled, s.ponytailEnabled, s.headroomEnabled].filter(Boolean).length;
  return html`
    <div class="grid g3">
      <div class="card sm stat"><span class="label"><${Icon} n="savings" />Savers on</span><span class="value">${on} / 4</span><span class="sub">applied to every request</span></div>
      <div class="card sm stat"><span class="label"><${Icon} n="bolt" />Hot path</span><span class="value">in-process</span><span class="sub">RTK, Caveman and Ponytail run inside FastRouter</span></div>
      <div class="card sm stat"><span class="label"><${Icon} n="cloud_sync" />Headroom</span><span class="value">${s.headroomEnabled ? "on" : "off"}</span><span class="sub">${s.headroomEnabled ? "falls back to direct if down" : "optional compression server"}</span></div>
    </div>
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="compress" />Token savers</h2><p>Shrink what goes upstream. Applied to every request; changes take effect immediately.</p></div></div>
      ${saver("terminal", "RTK", "Compresses long tool output (test logs, git diffs, builds) before it reaches the model.", "rtkEnabled", null, "rtkEnabled")}
      ${saver("record_voice_over", "Caveman", "Asks the model to answer tersely. Fewer output tokens, same substance.", "cavemanEnabled", "cavemanLevel", "caveman")}
      ${saver("content_cut", "Ponytail", "Trims the system prompt and old turns of long conversations.", "ponytailEnabled", "ponytailLevel", "ponytail")}
      ${saver("cloud_sync", "Headroom", "Sends context through a Headroom compression server first. Requests fall back to direct if it's down.", "headroomEnabled", null, "headroomEnabled",
        s.headroomEnabled && html`<div class="row" style="gap:8px;margin-top:10px;flex-wrap:wrap"><input class="input mono" style="width:280px" value=${hrUrl} disabled=${locked("headroomUrl")} onInput=${(e) => setHrUrl(e.target.value)} />
          <button class="btn outline sm" disabled=${hrUrl === s.headroomUrl || locked("headroomUrl")} onClick=${() => save({ headroomUrl: hrUrl })}>Save URL</button>
          <label class="row small" style="gap:8px"><${Toggle} sm on=${s.headroomCodeAware} disabled=${locked("headroomCodeAware")} onChange=${(v) => save({ headroomCodeAware: v })} />Code-aware</label></div>`)}
    </div>`;
}

/* ---------------------------------------------------------- console log */
// Live log over SSE (/api/admin/logs/stream), after 9Router's ConsoleLogClient.
const LEVELS = [["all", "All"], ["info", "Info"], ["warn", "Warn"], ["error", "Error"], ["debug", "Combo"]];
function ConsoleLogPage() {
  const [lines, setLines] = useState([]);
  const [conn, setConn] = useState("connecting");
  const [level, setLevel] = useState("all");
  const [q, setQ] = useState("");
  const [follow, setFollow] = useState(true);
  const [paused, setPaused] = useState(false);
  const box = useRef(null);
  const pausedRef = useRef(false);
  pausedRef.current = paused;
  useEffect(() => {
    const es = new EventSource("/api/admin/logs/stream");
    es.addEventListener("init", (e) => { setLines(JSON.parse(e.data) || []); setConn("live"); });
    es.addEventListener("line", (e) => { if (pausedRef.current) return; const l = JSON.parse(e.data); setLines((xs) => (xs.length >= 1000 ? [...xs.slice(-999), l] : [...xs, l])); });
    es.addEventListener("clear", () => setLines([]));
    es.onerror = () => setConn("reconnecting");
    es.onopen = () => setConn("live");
    return () => es.close();
  }, []);
  const ok = matcher(q);
  const shown = lines.filter((l) => (level === "all" || l.level === level) && ok(l.text));
  useEffect(() => { if (follow && box.current) box.current.scrollTop = box.current.scrollHeight; }, [shown.length, follow]);
  const counts = lines.reduce((m, l) => ((m[l.level] = (m[l.level] || 0) + 1), m), {});
  const clear = () => run(() => api("DELETE", "/api/admin/logs"), "Log cleared");
  const time = (ts) => new Date(ts).toLocaleTimeString([], { hour12: false });
  return html`
    <div class="row between" style="flex-wrap:wrap;gap:10px">
      <div class="row" style="gap:10px;flex-wrap:wrap">
        <${Badge} kind=${conn === "live" ? "success" : "warning"} dot>${conn === "live" ? (paused ? "Paused" : "Live") : "Reconnecting…"}</${Badge}>
        <div class="pills">${LEVELS.map(([k, l]) => html`<button class=${level === k ? "on" : ""} onClick=${() => setLevel(k)}>${l}${k !== "all" && counts[k] ? html`<span class="pill-n lv-${k}">${counts[k]}</span>` : ""}</button>`)}</div>
        <div class="picker-search" style="width:260px"><${Icon} n="search" /><input class="input" placeholder="Filter lines…" value=${q} onInput=${(e) => setQ(e.target.value)} /></div>
      </div>
      <div class="row" style="gap:8px">
        <label class="row small" style="gap:8px"><${Toggle} sm on=${follow} onChange=${setFollow} />Auto-scroll</label>
        <button class="btn outline sm" onClick=${() => setPaused(!paused)}><${Icon} n=${paused ? "play_arrow" : "pause"} />${paused ? "Resume" : "Pause"}</button>
        <button class="btn outline sm danger-text" onClick=${clear}><${Icon} n="delete_sweep" />Clear</button></div>
    </div>
    <div class="console" ref=${box}>
      ${shown.length === 0 ? html`<div class="console-empty">${lines.length ? "No lines match the filter." : "Waiting for log lines… Requests, retries, combo picks and reloads show up here."}</div>`
        : shown.map((l) => html`<div class="cl lv-${l.level}" key=${l.id}><span class="cl-t">${time(l.at)}</span><span class="cl-l">${l.level === "debug" ? "combo" : l.level}</span><span class="cl-x">${l.text}</span></div>`)}
    </div>
    <p class="tiny muted">Keeps the last 1,000 lines in memory. API keys, tokens and email addresses are masked before a line is stored.</p>`;
}

const THINK = [["auto", "Auto"], ["off", "Off"], ["low", "Low"], ["medium", "Medium"], ["high", "High"], ["max", "Max"]];
function SettingsPage({ data, reload, relogin }) {
  const s = data.settings;
  const locked = (k) => s.lockedByConfig.includes(k);
  const save = (body, ok = "Settings saved") => run(async () => { await api("PATCH", "/api/admin/settings", body); await reload(); }, ok);
  const [sticky, setSticky] = useState(String(s.stickyRoundRobinLimit));
  const routed = data.providers.filter((p) => p.accounts.length > 0 && p.kind !== "plugin");
  return html`
    <a class="card xs hover row between" href="#/token-saver" style="text-decoration:none;color:inherit"><div class="saver-head"><div class="saver-icon"><${Icon} n="savings" /></div>
      <div><div class="t">Token savers moved</div><div class="d">RTK, Caveman, Ponytail and Headroom now have their own page.</div></div></div><${Icon} n="chevron_right" /></a>
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
const PAGES = { endpoint: EndpointPage, providers: ProvidersPage, combos: CombosPage, usage: UsagePage, quota: QuotaPage, keys: KeysPage, cli: CliPage, settings: SettingsPage, "token-saver": TokenSaverPage, "console-log": ConsoleLogPage };
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
