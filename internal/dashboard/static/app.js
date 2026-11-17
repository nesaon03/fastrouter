// FastRouter dashboard: a small Preact + htm SPA (no build step).
// Structure and look follow 9Router's dashboard (MIT, decolua/9router).
import { html, render, useState, useEffect, useCallback, useRef, useMemo } from "./vendor/preact-htm.js";

/* ------------------------------------------------------------------ api */
let csrf = "";
let onUnauthorized = () => {};
async function api(method, path, body, retried) {
  const opt = { method, credentials: "same-origin", headers: {} };
  if (method !== "GET") opt.headers["X-CSRF-Token"] = csrf;
  if (body !== undefined) { opt.headers["Content-Type"] = "application/json"; opt.body = JSON.stringify(body); }
  const res = await fetch(path, opt);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (e) { data = { error: text.trim().slice(0, 300) }; }
  // A tab opened before a re-login or restart holds an old CSRF token: refresh it once and retry.
  if (res.status === 403 && !retried && method !== "GET" && data && /CSRF/.test(data.error || "")) {
    try {
      const s = await api("GET", "/api/auth/session");
      if (s && s.authenticated && s.csrf) { csrf = s.csrf; return api(method, path, body, true); }
    } catch (e) { /* fall through to the original error */ }
    onUnauthorized();
  }
  if (res.status === 401 && !path.startsWith("/api/auth/")) onUnauthorized();
  if (!res.ok) { const e = new Error((data && (data.error || data.message)) || res.statusText); e.data = data; throw e; }
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
    ${bad ? html`<div class="logo-fallback" style=${`background:${m.color}`}>${p.textIcon || m.name.slice(0, 2).toUpperCase()}</div>`
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
// Searchable picker grouped by provider, after 9Router's ModelSelectModal.
// Fast with thousands of models: the search index is built once per state
// load, typing is coalesced to one filter per frame, and only the first
// chunk of matches is rendered (more stream in as you scroll).
const pickIndex = new WeakMap();
function indexFor(data) {
  let ix = pickIndex.get(data);
  if (ix) return ix;
  const byId = new Map(data.providers.map((p) => [p.id, p]));
  const groups = (data.modelGroups || []).map((g) => {
    const p = byId.get(g.provider) || { id: g.provider, name: g.name };
    const head = `${g.name} ${g.prefix} ${g.provider}`.toLowerCase();
    return { g, p, models: g.models.map((m) => ({ id: m.id, label: m.name || splitModel(m.id)[1], custom: m.custom, hay: `${head} ${m.id} ${m.name || ""}`.toLowerCase() })) };
  });
  const combos = data.combos.map((c) => ({ c, hay: `combo ${c.name} ${c.models.join(" ")}`.toLowerCase() }));
  ix = { groups, combos, total: groups.reduce((n, x) => n + x.models.length, 0) };
  pickIndex.set(data, ix);
  return ix;
}
// Space-separated terms must all match; "-term" excludes; "/re/" is a regex.
// Haystacks are pre-lowercased.
function matcher(q) {
  q = q.trim();
  if (!q) return () => true;
  if (q.length > 2 && q.startsWith("/") && q.endsWith("/")) {
    try { const re = new RegExp(q.slice(1, -1), "i"); return (s) => re.test(s); } catch (e) { return () => false; }
  }
  const terms = q.toLowerCase().split(/\s+/);
  return (s) => { s = s.toLowerCase(); return terms.every((t) => (t[0] === "-" ? t.length === 1 || !s.includes(t.slice(1)) : s.includes(t))); };
}
const PICK_CHUNK = 160;
// only: provider ids to list (default all); status(id): "ok" | "fail" for a
// last-request dot; provStatus: provider id -> "ok" | "fail"; onPickMany(ids,
// on) adds a per-provider Select all.
function ModelPicker({ data, selected = [], multi, onPick, onClose, title, combos = true, exclude, only, status, provStatus, onPickMany }) {
  const ix = indexFor(data);
  const [q, setQ] = useState("");
  const [prov, setProv] = useState("");
  const [custom, setCustom] = useState("");
  const [shut, setShut] = useState({});
  const [limit, setLimit] = useState(PICK_CHUNK);
  const timer = useRef(0);
  const listRef = useRef(null);
  const sentinel = useRef(null);
  const onType = (e) => { const v = e.target.value; cancelAnimationFrame(timer.current); timer.current = requestAnimationFrame(() => { setQ(v); setLimit(PICK_CHUNK); if (listRef.current) listRef.current.scrollTop = 0; }); };
  useEffect(() => () => cancelAnimationFrame(timer.current), []);
  const ok = matcher(q);
  const sel = new Set(selected);
  const cs = combos && !prov ? ix.combos.filter((x) => x.c.name !== exclude && ok(x.hay)) : [];
  let budget = limit - cs.length, total = cs.length, shown = cs.length;
  const groups = [];
  const allGroups = only ? ix.groups.filter((x) => only.has(x.g.provider)) : ix.groups;
  const shownTotal = only ? allGroups.reduce((n, x) => n + x.models.length, 0) + (combos ? ix.combos.length : 0) : ix.total;
  for (const x of allGroups) {
    if (prov && x.g.provider !== prov) continue;
    const models = q ? x.models.filter((m) => ok(m.hay)) : x.models;
    if (!models.length) continue;
    total += models.length;
    const closed = shut[x.g.provider] && !q;
    const take = closed ? 0 : Math.max(0, Math.min(models.length, budget));
    budget -= take; shown += take;
    groups.push({ ...x, all: models, count: models.length, models: models.slice(0, take), closed, cut: !closed && take < models.length });
  }
  const more = shown < total && budget <= 0;
  useEffect(() => {
    if (!more || !sentinel.current) return;
    const io = new IntersectionObserver((es) => { if (es.some((e) => e.isIntersecting)) setLimit((l) => l + PICK_CHUNK * 2); }, { root: listRef.current, rootMargin: "300px" });
    io.observe(sentinel.current);
    return () => io.disconnect();
  }, [more, limit, q, prov]);
  const first = cs.length ? cs[0].c.name : groups.find((x) => x.models.length)?.models[0]?.id;
  const chip = (id, label, tag) => { const stt = status && status(id); return html`<button type="button" class="pick-chip ${sel.has(id) ? "on" : ""}" title=${stt ? `${id} · last request ${stt === "ok" ? "succeeded" : "failed"}` : id} onClick=${() => onPick(id)}>
    ${sel.has(id) && html`<${Icon} n="check" />`}${stt && html`<span class="sdot ${stt}"></span>`}<span class="truncate">${label}</span>${tag && html`<span class="tag">${tag}</span>`}</button>`; };
  const pstat = (id) => provStatus && provStatus[id];
  const addCustom = () => { const v = custom.trim(); if (v) { onPick(v); setCustom(""); } };
  return html`<${Modal} wide title=${title || (multi ? "Add models" : "Select model")} icon="neurology" onClose=${onClose}
    foot=${html`<span class="small muted grow">${multi ? `${selected.length} selected · click to add or remove` : "Click a model to choose it · Enter picks the first match"}</span>
      <button class="btn primary" onClick=${onClose}>${multi ? "Done" : "Cancel"}</button>`}>
    <div class="picker">
      <div class="picker-search"><${Icon} n="search" /><input class="input" autofocus placeholder="Search ${shownTotal} models, providers or prefixes… (-exclude, /regex/)" onInput=${onType}
          onKeyDown=${(e) => { if (e.key === "Enter" && first) { e.preventDefault(); onPick(first); } }} />
        <span class="small muted">${total} match${total === 1 ? "" : "es"}</span></div>
      <div class="pick-provs">
        <button type="button" class="pprov ${prov ? "" : "on"}" onClick=${() => setProv("")}>All<span class="count">${shownTotal}</span></button>
        ${allGroups.map((x) => html`<button type="button" key=${x.g.provider} class="pprov ${prov === x.g.provider ? "on" : ""}" title=${`${x.g.name} (${x.g.prefix}/)`}
            onClick=${() => { setProv(prov === x.g.provider ? "" : x.g.provider); setLimit(PICK_CHUNK); }}><${Logo} p=${x.p} xs /><span class="truncate">${x.g.name}</span><span class="count">${x.models.length}</span></button>`)}</div>
      <div class="picker-list" ref=${listRef}>
        ${cs.length > 0 && html`<div class="pick-group"><div class="pick-head"><span class="mtag-ic"><${Icon} n="layers" /></span><span class="strong">Combos</span><span class="count">${cs.length}</span>
          ${onPickMany && html`<span class="grow"></span><button type="button" class="btn ghost sm pick-all" onClick=${() => { const ids = cs.map((x) => x.c.name); onPickMany(ids, !ids.every((i) => sel.has(i))); }}>${cs.every((x) => sel.has(x.c.name)) ? "Clear" : "Select all"}</button>`}</div>
          <div class="pick-chips">${cs.map(({ c }) => chip(c.name, c.name, c.strategy && c.strategy !== "fallback" ? STRATEGY[c.strategy].short : ""))}</div></div>`}
        ${groups.map(({ g, p, models, all, count, closed, cut }) => { const on = all.every((m) => sel.has(m.id)); return html`<div class="pick-group" key=${g.provider}>
          <div class="pick-head-row"><button type="button" class="pick-head" onClick=${() => setShut({ ...shut, [g.provider]: !shut[g.provider] })}>
            <${Logo} p=${p} xs /><span class="strong">${g.name}</span><span class="mono small muted">${g.prefix}/</span>
            ${pstat(g.provider) && html`<span class="sdot ${pstat(g.provider)}" title=${pstat(g.provider) === "ok" ? "An account's last request succeeded" : "The accounts' last requests failed"}></span>`}
            ${g.plugin && html`<${Badge} icon="extension">needs plugin</${Badge}>`}<span class="count">${count}</span>
            <span class="grow"></span><${Icon} n=${closed ? "expand_more" : "expand_less"} /></button>
            ${onPickMany && html`<button type="button" class="btn ghost sm pick-all" title=${`${on ? "Remove" : "Add"} all ${count} ${g.name} model${count === 1 ? "" : "s"}${q ? " matching the search" : ""}`}
              onClick=${() => onPickMany(all.map((m) => m.id), !on)}><${Icon} n=${on ? "remove_done" : "done_all"} />${on ? "Clear" : "Select all"}</button>`}</div>
          ${models.length > 0 && html`<div class="pick-chips">${models.map((m) => chip(m.id, m.label, m.custom ? "custom" : ""))}${cut && html`<span class="small muted" style="align-self:center">+${count - models.length} more…</span>`}</div>`}</div>`; })}
        ${more && html`<div ref=${sentinel} class="small muted" style="text-align:center;padding:10px">Loading more…</div>`}
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

/* ---------------------------------------------------------- speed test */
// Runs fastrouter's bench against its own /v1 endpoints (server-side, with a
// FastRouter API key the browser never sees) and shows live timings. Each
// request is shaped like the chosen IDE or CLI sends it.
const EP_META = { chat: ["Chat Completions", "/v1/chat/completions"], messages: ["Messages", "/v1/messages"], responses: ["Responses", "/v1/responses"], gemini: ["Gemini", "/v1beta/models/…:streamGenerateContent"] };
const EP_SHORT = { chat: "Chat", messages: "Messages", responses: "Responses", gemini: "Gemini" };
const msf = (v) => v == null || isNaN(v) ? "—" : v >= 1000 ? (v / 1000).toFixed(v >= 10000 ? 1 : 2) + " s" : v >= 100 ? Math.round(v) + " ms" : v.toFixed(v >= 10 ? 1 : 2) + " ms";
const ovf = (v) => v == null || isNaN(v) ? "—" : v.toFixed(2) + " ms";
const avg = (xs) => xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : NaN;
const p95 = (xs) => { if (!xs.length) return NaN; const s = xs.slice().sort((a, b) => a - b); return s[Math.ceil(0.95 * s.length) - 1]; };
// Speed bands for time to first token, same as `fastrouter bench`.
const SPEED = [[500, "Really fast", "s1"], [1500, "Fast", "s2"], [3000, "OK", "s3"], [6000, "Slow", "s4"], [Infinity, "Very slow", "s5"]];
const SPEED_HELP = "Time to first token: under 0.5 s Really fast · under 1.5 s Fast · under 3 s OK · under 6 s Slow · 6 s or more Very slow";
const speedOf = (ms) => SPEED.find(([lim]) => ms < lim);
const ovLabel = (ms) => ms < 1 ? "negligible" : ms < 5 ? "tiny" : ms < 20 ? "small" : "noticeable";
function SpeedBadge({ ms, byTotal }) {
  if (ms == null || isNaN(ms)) return html`<span class="muted">—</span>`;
  const [, label, cls] = speedOf(ms);
  return html`<span class="st-speed ${cls}" title=${(byTotal ? "No visible first token, so rated on total time. " : "") + SPEED_HELP}>${label}${byTotal ? "*" : ""}</span>`;
}
function SpeedLegend() {
  return html`<div class="st-bands" title=${SPEED_HELP}><span class="small muted">Speed (TTFT):</span>
    ${SPEED.map(([lim, label, cls], i) => html`<span class="st-speed ${cls}">${label}</span><span class="small muted">${lim === Infinity ? "≥ 6 s" : "< " + msf(lim)}</span>`)}
    <span class="small muted st-bands-ov">${"Overhead: < 1 ms negligible · < 5 ms tiny · < 20 ms small · more is noticeable"}</span></div>`;
}
function ClientLogo({ c }) {
  const [bad, setBad] = useState(false);
  if (!c || !c.logo || bad) return html`<span class="logo xs st-api-ic"><${Icon} n=${c && c.kind === "api" ? "api" : "terminal"} /></span>`;
  return html`<span class="logo xs"><img src=${"logos/" + c.logo} alt="" onError=${() => setBad(true)} /></span>`;
}
// Older runs had no client: map their endpoint (+ Simulate IDE) to one.
const LEGACY_IDE = { chat: "cursor", messages: "claude", responses: "codex" };
const clientId = (r, o) => r.client || (o && o.ide ? LEGACY_IDE[r.endpoint] : "api-" + r.endpoint);
const clientName = (cm, id) => (cm[id] && cm[id].name) || id;
const ST_KEY = "fr-speedtest";

function SpeedTestPage({ data }) {
  const saved = useMemo(() => { try { return JSON.parse(localStorage.getItem(ST_KEY) || "{}"); } catch (e) { return {}; } }, []);
  const [opt, setOpt] = useState(null);
  const [sel, setSel] = useState(saved.models || null);
  const [clients, setClients] = useState(saved.clients || ["cursor"]);
  const [stream, setStream] = useState(saved.stream ?? true);
  const [prompt, setPrompt] = useState(saved.prompt || "tiny");
  const [runs, setRuns] = useState(saved.runs || 3);
  const [effort, setEffort] = useState(saved.effort || "auto");
  const [pick, setPick] = useState(false);
  const [job, setJob] = useState(null);
  const [hist, setHist] = useState(null);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState(null);
  const poll = useRef(0);
  const loadHist = () => api("GET", "/api/admin/bench/history").then((r) => setHist(r.runs), () => setHist([]));
  const watch = (id) => {
    clearTimeout(poll.current);
    const tick = async () => {
      try {
        const v = await api("GET", `/api/admin/bench/${id}`);
        setJob(v);
        if (v.status === "running") poll.current = setTimeout(tick, 600); else loadHist();
      } catch (e) { poll.current = setTimeout(tick, 1500); }
    };
    tick();
  };
  useEffect(() => {
    api("GET", "/api/admin/bench/options").then((o) => {
      setOpt(o);
      setSel((s) => s || o.candidates.filter((c) => c.working && c.kind === "model").slice(0, 3).map((c) => c.id));
      if (o.running) watch(o.running);
      else api("GET", "/api/admin/bench/history").then((r) => { setHist(r.runs); if (r.runs.length) watch(r.runs[0].id); }, () => setHist([]));
    }, (e) => setOpt({ candidates: [], clients: [], providers: [], error: e.message }));
    return () => clearTimeout(poll.current);
  }, []);
  useEffect(() => { try { localStorage.setItem(ST_KEY, JSON.stringify({ models: sel, clients, stream, prompt, runs, effort })); } catch (e) {} }, [sel, clients, stream, prompt, runs, effort]);
  const running = job && job.status === "running";
  const models = sel || [];
  const total = models.length * clients.length * runs;
  const start = async () => {
    setBusy(true);
    try {
      const r = await api("POST", "/api/admin/bench", { models, clients, stream, runs: Number(runs), prompt, effort });
      watch(r.id);
    } catch (e) { toast(e.message, "error"); }
    setBusy(false);
  };
  const stop = () => job && run(() => api("POST", `/api/admin/bench/${job.id}/stop`), "Stopping…");
  if (!opt) return html`<div class="card"><p class="muted">Loading…</p></div>`;
  const cm = Object.fromEntries((opt.clients || []).map((c) => [c.id, c]));
  const agents = (opt.clients || []).filter((c) => c.kind !== "api"), raw = (opt.clients || []).filter((c) => c.kind === "api");
  const working = opt.candidates.filter((c) => c.working && c.kind === "model").map((c) => c.id);
  const only = new Set((opt.providers || []).map((p) => p.id));
  const provStatus = Object.fromEntries((opt.providers || []).filter((p) => p.status).map((p) => [p.id, p.status]));
  const mstat = (id) => { const c = (opt.modelStatus || {})[id]; return c ? (c === 200 ? "ok" : "fail") : ""; };
  const toggleModel = (id) => setSel(models.includes(id) ? models.filter((x) => x !== id) : [...models, id]);
  const pickMany = (ids, on) => setSel(on ? [...models, ...ids.filter((i) => !models.includes(i))] : models.filter((x) => !ids.includes(x)));
  const toggleClient = (id) => setClients(clients.includes(id) ? clients.filter((x) => x !== id) : [...clients, id]);
  const card = (c) => { const on = clients.includes(c.id); return html`<button type="button" key=${c.id} class="st-client ${on ? "on" : ""}" disabled=${running} onClick=${() => toggleClient(c.id)}
      title=${`${c.name} → ${EP_META[c.endpoint] ? EP_META[c.endpoint][1] : c.endpoint}\nUser-Agent: ${c.userAgent}${c.tools ? `\n${c.tools} tools, its own system prompt` : "\n" + (c.note || "")}`}>
    <${ClientLogo} c=${c} /><span class="st-client-txt"><span class="medium truncate">${c.name}</span>
      <span class="small muted truncate">${EP_SHORT[c.endpoint] || c.endpoint}${c.tools ? ` · ${c.tools} tools` : ""}</span></span>
    <span class="st-check"><${Icon} n=${on ? "check_circle" : "radio_button_unchecked"} /></span></button>`; };
  return html`
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="tune" />Configure</h2><p>Each request goes through <span class="mono">${opt.base}</span> exactly like the chosen IDE or CLI calls it.</p></div>
        <${Badge} kind=${opt.key && !/^no /.test(opt.key) ? "success" : "warning"} icon="vpn_key">${opt.key || "no key"}</${Badge}></div>
      <div class="col" style="gap:20px">
        <div class="field"><div class="row between"><label>Test as <span class="muted small">(${clients.length} selected)</span></label>
          <div class="row" style="gap:6px">
            <button class="btn ghost sm" disabled=${running} onClick=${() => setClients(["cursor", "claude", "codex"])}><${Icon} n="star" />Popular</button>
            <button class="btn ghost sm" disabled=${running} onClick=${() => setClients(agents.map((c) => c.id))}><${Icon} n="done_all" />All IDEs & CLIs</button>
            <button class="btn ghost sm" disabled=${running || !clients.length} onClick=${() => setClients([])}>Clear</button></div></div>
          <div class="st-clients">${agents.map(card)}</div>
          <div class="st-sub">Raw API · no system prompt or tools</div>
          <div class="st-clients">${raw.map(card)}</div>
          <div class="hint">Each client sends its own endpoint, headers, user agent, system prompt and tool list (condensed copies of the real ones). Hover a card for details.</div></div>
        <div class="field"><div class="row between"><label>Models & combos <span class="muted small">(${models.length} selected)</span></label>
          <div class="row" style="gap:6px"><button class="btn ghost sm" disabled=${running || !working.length} onClick=${() => setSel(working)}><${Icon} n="bolt" />Working (${working.length})</button>
            <button class="btn ghost sm" disabled=${running || !models.length} onClick=${() => setSel([])}>Clear</button></div></div>
          <div class="st-models-box ${running ? "off" : ""}">
            ${models.map((id) => { const s = mstat(id); return html`<span class="st-mchip" key=${id}>${s && html`<span class="sdot ${s}" title=${s === "ok" ? "Last request succeeded" : "Last request failed"}></span>`}<${ModelTag} data=${data} id=${id} />
              <button type="button" class="st-mx" title="Remove" disabled=${running} onClick=${() => toggleModel(id)}><${Icon} n="close" /></button></span>`; })}
            <button type="button" class="btn outline sm" disabled=${running} onClick=${() => setPick(true)}><${Icon} n="add" />${models.length ? "Add models" : "Choose models"}</button></div>
          ${!only.size && html`<div class="hint">No provider has a usable account yet.</div>`}</div>
        <div class="st-row">
          <${Field} label="Streaming"><${Seg} value=${stream ? "on" : "off"} disabled=${running} onChange=${(v) => setStream(v === "on")} options=${[["on", "On (SSE)"], ["off", "Off (JSON)"]]} /></${Field}>
          <${Field} label="Prompt size"><${Seg} value=${prompt} disabled=${running} onChange=${setPrompt} options=${[["tiny", "Tiny · ~20 tokens"], ["medium", "Medium · ~700 in"]]} /></${Field}>
          <${Field} label="Reasoning effort"><${Seg} value=${effort} disabled=${running} onChange=${setEffort} options=${[["auto", "Auto"], ["none", "Off"], ["low", "Low"], ["medium", "Medium"], ["high", "High"]]} /></${Field}>
          <${Field} label="Runs each"><input class="input" type="number" min="1" max="20" style="width:90px" value=${runs} disabled=${running} onInput=${(e) => setRuns(Math.max(1, Math.min(20, Number(e.target.value) || 1)))} /></${Field}>
        </div>
        <div class="hint" style="margin-top:-10px">Auto: each client sends its usual effort (Codex: medium); raw API requests ask for low so reasoning models still write visible text.</div>
        <div class="row between st-foot">
          <span class="small muted"><${Icon} n="lock" style="font-size:15px;vertical-align:-3px" /> The API key is used on the server only and never sent to this page. Runs one request at a time.</span>
          <div class="row" style="gap:10px"><span class="small muted ${total > 300 ? "danger-text" : ""}">${total} request${total === 1 ? "" : "s"}${total > 300 ? " (max 300)" : ""}</span>
            ${running ? html`<button class="btn danger" onClick=${stop}><${Icon} n="stop_circle" />Stop</button>`
              : html`<button class="btn primary" disabled=${busy || !models.length || !clients.length || total > 300} onClick=${start}><${Icon} n="play_arrow" />Start speed test</button>`}</div></div>
      </div>
    </div>
    ${job ? html`<${SpeedResults} job=${job} cm=${cm} data=${data} />` : html`<div class="empty-line"><${Icon} n="speed" />Pick clients and models, then press Start — results appear here live</div>`}
    <${SpeedHistory} hist=${hist} cm=${cm} current=${job && job.id} running=${running} onView=${watch}
      onDelete=${(r) => setConfirm(r)} onClear=${() => setConfirm({ id: "all" })} />
    ${pick && html`<${ModelPicker} multi title="Choose models to test" data=${data} selected=${models} only=${only} status=${mstat} provStatus=${provStatus}
      onPick=${toggleModel} onPickMany=${pickMany} onClose=${() => setPick(false)} />`}
    ${confirm && html`<${Confirm} title=${confirm.id === "all" ? "Clear speed test history?" : "Delete this run?"} text=${confirm.id === "all" ? "Every stored run is removed." : "This run and its requests are removed."}
      onClose=${() => setConfirm(null)} onYes=${() => run(async () => { await api("DELETE", `/api/admin/bench/${confirm.id}`); if (job && (confirm.id === "all" || confirm.id === job.id)) setJob(null); await loadHist(); }, "Deleted")} />`}`;
}

const NOTEXT = "no visible output (reasoning used the budget)";
const ttftN = (s) => s.ttftRuns ?? (s.ttft && s.ttft.avg > 0 ? s.ok : 0);
function SpeedResults({ job, cm, data }) {
  const o = job.options || {};
  const rs = job.results || [], ok = rs.filter((r) => r.ok);
  const pct = job.total ? Math.round((100 * job.done) / job.total) : 0;
  const ttft = ok.filter((r) => !r.noText).map((r) => r.ttftMs), over = ok.filter((r) => r.timed).map((r) => r.overheadMs), tps = ok.filter((r) => r.tokPerSec > 0).map((r) => r.tokPerSec);
  const stats = [["timer", "TTFT avg · p95", msf(avg(ttft)), html`p95 ${msf(p95(ttft))} · <${SpeedBadge} ms=${avg(ttft)} />`, "blue"],
    ["bolt", "FastRouter overhead", ovf(avg(over)), `p95 ${ovf(p95(over))} · ${isNaN(avg(over)) ? "pre + post" : ovLabel(avg(over))}`, "orange"],
    ["speed", "Tokens / sec", isNaN(avg(tps)) ? "—" : avg(tps).toFixed(1), "average while generating", "green"],
    ["check_circle", "Passed", `${ok.length}/${rs.length}`, rs.length - ok.length ? `${rs.length - ok.length} failed` : "no failures", rs.length - ok.length ? "yellow" : "green"]];
  const savers = [...new Set(rs.map((r) => r.savers).filter(Boolean))].join(", ");
  const st = { running: ["info", "Running"], done: ["success", "Done"], stopped: ["warning", "Stopped"], interrupted: ["warning", "Interrupted"] }[job.status] || ["", job.status];
  // Summary rows grouped by client, in run order.
  const groups = [];
  for (const s of job.summary || []) {
    const id = clientId(s, o);
    let g = groups.find((x) => x.id === id);
    if (!g) groups.push((g = { id, rows: [] }));
    g.rows.push(s);
  }
  const cols = 10;
  const errText = (s) => s.errors ? s.errors.split(", ").map((e) => { const k = e.split(" ")[0]; return CLASS_LABEL[k] ? CLASS_LABEL[k] + e.slice(k.length) : e; }).join(", ") : "failed";
  return html`
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="monitoring" />Results</h2>
          <p>${new Date(job.startedAt).toLocaleString()} · ${groups.map((g) => clientName(cm, g.id)).join(", ") || "—"} · ${o.stream ? "streaming" : "no streaming"} · ${o.prompt} prompt${o.effort && o.effort !== "auto" ? ` · effort ${o.effort}` : ""} · ${o.runs} run${o.runs === 1 ? "" : "s"} each${job.source === "cli" ? " · from CLI" : ""}</p></div>
        <div class="row" style="gap:8px"><${Badge} kind=${st[0]} dot>${st[1]}</${Badge}>
          <a class="btn outline sm" href=${`/api/admin/bench/${job.id}/export?format=json`} download><${Icon} n="download" />JSON</a>
          <a class="btn outline sm" href=${`/api/admin/bench/${job.id}/export?format=csv`} download><${Icon} n="download" />CSV</a></div></div>
      <div class="st-progress"><div style=${`width:${pct}%`} class=${job.status === "running" ? "live" : ""}></div></div>
      <div class="small muted" style="margin:6px 0 18px">${job.done} of ${job.total} requests${job.key ? " · " + job.key : ""}</div>
      ${savers && html`<div class="notice info" style="margin:-8px 0 16px"><${Icon} n="compress" /><span>Token Saver ran on these requests (<span class="mono">${savers}</span>): its system-prompt rules count as extra input tokens, exactly as for your IDE. Turn it off under Token Saver to compare.</span></div>`}
      <div class="grid g4" style="margin-bottom:20px">${stats.map(([ic, l, v, sub, c]) => html`<div class="card sm stat ${c} ${ic === "bolt" ? "st-over-card" : ""}"><span class="label"><${Icon} n=${ic} />${l}</span><span class="value">${v}</span><span class="sub">${sub}</span></div>`)}</div>
      ${groups.length > 0 && html`<div class="st-table-wrap"><table class="table st-table">
        <thead><tr><th>Model</th><th class="num">OK</th><th title=${SPEED_HELP}>Speed</th><th class="num">TTFT avg</th><th class="num">p95</th>
          <th class="num">Total avg</th><th class="num">p95</th><th class="num">Tok/s</th><th class="num st-over">Overhead avg</th><th class="num st-over">p95</th></tr></thead>
        <tbody>${groups.map((g) => { const c = cm[g.id]; return html`
          <tr class="st-group" key=${"g" + g.id}><td colspan=${cols}><div class="row" style="gap:8px"><${ClientLogo} c=${c} /><span class="strong">${clientName(cm, g.id)}</span>
            ${c && html`<span class="mono small muted">${EP_META[c.endpoint] ? EP_META[c.endpoint][1] : c.endpoint}</span><span class="small muted">${c.tools ? `· system prompt + ${c.tools} tools` : "· plain request"}</span>`}</div></td></tr>
          ${g.rows.map((s) => { const n = ttftN(s); return html`<tr key=${g.id + s.model + s.endpoint}>
            <td title=${s.accounts ? "Accounts: " + s.accounts : ""}><${ModelTag} data=${data} id=${s.model} /></td>
            <td class="num" title=${n && n < s.ok ? `${s.ok - n} passing run(s) had ${NOTEXT}` : ""}><span class=${s.ok === s.runs ? "ok-text" : s.ok ? "warn-text" : "danger-text"}>${s.ok}/${s.runs}${n && n < s.ok ? "†" : ""}</span></td>
            ${s.ok ? html`<td>${n ? html`<${SpeedBadge} ms=${s.ttft.avg} />` : html`<${SpeedBadge} ms=${s.total.avg} byTotal />`}</td>
              ${n ? html`<td class="num strong" title=${`min ${msf(s.ttft.min)} · p50 ${msf(s.ttft.p50)} · max ${msf(s.ttft.max)}`}>${msf(s.ttft.avg)}</td><td class="num">${msf(s.ttft.p95)}</td>`
                : html`<td colspan="2" class="small muted st-notext" title="Only hidden reasoning tokens came back, so there is no first visible token. Try effort Low or Off.">— ${NOTEXT}</td>`}
              <td class="num">${msf(s.total.avg)}</td><td class="num">${msf(s.total.p95)}</td><td class="num">${s.tokPerSec ? s.tokPerSec.toFixed(1) : "—"}</td>
              <td class="num st-over"><span class="strong">${ovf(s.overhead.avg)}</span> <span class="st-ovl">${s.overhead.avg || s.overhead.max ? ovLabel(s.overhead.avg) : ""}</span></td><td class="num st-over">${ovf(s.overhead.p95)}</td>`
              : html`<td colspan=${cols - 2} class="small danger-text">${errText(s)}</td>`}</tr>`; })}`; })}</tbody></table></div>
      <${SpeedLegend} />`}
    </div>
    ${rs.length > 0 && html`<div class="card"><div class="card-title"><div><h2><${Icon} n="bar_chart" />Per request</h2><p>Bar = total time. Darker part = time to first token. The orange tick is FastRouter's own overhead (scaled up 10× so it is visible).</p></div></div>
      <${SpeedChart} rs=${rs} cm=${cm} o=${o} />
      <div class="st-table-wrap" style="margin-top:16px"><table class="table st-table">
        <thead><tr><th>#</th><th>Client</th><th>Model</th><th title=${SPEED_HELP}>Result</th><th class="num">TTFT</th><th class="num">Total</th><th class="num">In / out tok</th><th class="num">Tok/s</th><th class="num st-over">Overhead</th><th>Account</th></tr></thead>
        <tbody>${rs.slice().reverse().map((r) => { const id = clientId(r, o); return html`<tr key=${r.seq}>
          <td class="muted">${r.seq}</td><td><div class="row nowrap" style="gap:6px"><${ClientLogo} c=${cm[id]} /><span class="small">${clientName(cm, id)}${r.stream ? "" : " · JSON"}</span></div></td>
          <td><${ModelTag} data=${data} id=${r.model} /></td>
          <td>${r.ok ? html`<${SpeedBadge} ms=${r.noText ? r.totalMs : r.ttftMs} byTotal=${r.noText} />` : html`<span title=${r.message || ""}><${Badge} kind="error" icon="close">${CLASS_LABEL[r.class] || r.class || "fail"}${r.status ? " · " + r.status : ""}</${Badge}></span>`}</td>
          <td class="num" title=${r.noText ? NOTEXT : ""}>${r.ok ? (r.noText ? html`<span class="muted">— <span class="small">no visible output</span></span>` : msf(r.ttftMs)) : "—"}</td><td class="num">${msf(r.totalMs)}</td><td class="num small" title=${r.savers ? "Token Saver: " + r.savers : ""}>${r.ok ? `${r.inTokens || "—"} / ${r.outTokens || "—"}` : "—"}</td><td class="num">${r.ok && r.tokPerSec ? r.tokPerSec.toFixed(1) + (r.estimated ? "*" : "") : "—"}</td>
          <td class="num st-over" title=${r.timed ? `pre ${ovf(r.preMs)} (request in → sent upstream) + post ${ovf(r.postMs)} (upstream ready → first byte to client). Empty-answer hold ${ovf(r.holdMs)} not counted. Upstream answered headers in ${msf(r.upTtfbMs)}.${r.attempts > 1 ? ` ${r.attempts} attempts, failover ${msf(r.failoverMs)}.` : ""}` : "no timing trailer"}>${r.timed ? html`${ovf(r.overheadMs)} <span class="st-ovl">${ovLabel(r.overheadMs)}</span>` : "—"}</td>
          <td class="small muted st-acct" title=${r.account || ""}>${r.account || "—"}</td></tr>`; })}</tbody></table></div>
      ${rs.some((r) => r.estimated) && html`<p class="small muted" style="margin-top:8px">* tokens estimated from characters (the provider sent no usage).</p>`}
      ${rs.some((r) => r.noText) && html`<p class="small muted" style="margin-top:4px">— ${NOTEXT}: only hidden reasoning tokens came back, so TTFT is not counted and the speed is rated on total time (*). Set Reasoning effort to Low or Off to get visible text.</p>`}
    </div>`}`;
}

function SpeedChart({ rs, cm, o }) {
  const [hover, setHover] = useState(-1);
  const W = 900, H = 200, L = 52, R = 12, T = 10, B = 24;
  const max = Math.max(...rs.map((r) => r.totalMs), 1);
  const n = Math.max(rs.length, 12), bw = (W - L - R) / n, Y = (v) => T + (H - T - B) * (1 - Math.min(v, max) / max);
  const h = hover >= 0 && rs[hover];
  return html`<div class="chart-wrap"><svg viewBox="0 0 ${W} ${H}" class="chart" onMouseLeave=${() => setHover(-1)}>
    ${[0, 0.5, 1].map((f) => html`<line x1=${L} x2=${W - R} y1=${Y(max * f)} y2=${Y(max * f)} class="grid-l" /><text x=${L - 8} y=${Y(max * f) + 4} text-anchor="end" class="axis">${msf(max * f)}</text>`)}
    ${rs.map((r, i) => { const x = L + i * bw + bw * 0.15, w = bw * 0.7; return html`<g key=${r.seq} onMouseEnter=${() => setHover(i)}>
      <rect x=${x} y=${T} width=${w} height=${H - T - B} fill="transparent" />
      ${r.ok ? html`<rect x=${x} y=${Y(r.totalMs)} width=${w} height=${H - B - Y(r.totalMs)} rx="3" class="st-bar-total" />
        ${!r.noText && html`<rect x=${x} y=${Y(r.ttftMs)} width=${w} height=${H - B - Y(r.ttftMs)} rx="3" class="st-bar-ttft" />`}
        ${r.timed && html`<rect x=${x} y=${H - B - Math.max(2, (H - T - B) * Math.min(1, (r.overheadMs * 10) / max))} width=${w} height=${Math.max(2, (H - T - B) * Math.min(1, (r.overheadMs * 10) / max))} class="st-bar-over" />`}`
        : html`<rect x=${x} y=${Y(r.totalMs)} width=${w} height=${H - B - Y(r.totalMs)} rx="3" class="st-bar-fail" />`}
      ${(rs.length <= 30 || i % Math.ceil(rs.length / 30) === 0) && html`<text x=${x + w / 2} y=${H - 8} text-anchor="middle" class="axis">${r.seq}</text>`}</g>`; })}
  </svg>${h && html`<div class="chart-tip" style=${`left:${(100 * (L + hover * bw + bw / 2)) / W}%`}>
    <div class="strong">#${h.seq} · ${h.model}</div><div>${clientName(cm, clientId(h, o))} · ${h.ok ? (h.noText ? "pass, no visible output" : "pass · " + speedOf(h.ttftMs)[1]) : CLASS_LABEL[h.class] || h.class}</div>
    <div>TTFT ${h.noText ? "—" : msf(h.ttftMs)} · total ${msf(h.totalMs)}</div><div>overhead ${ovf(h.overheadMs)}</div></div>`}
    <div class="st-legend"><span><i class="st-bar-ttft"></i>Time to first token</span><span><i class="st-bar-total"></i>Generation</span><span><i class="st-bar-over"></i>FastRouter overhead ×10</span><span><i class="st-bar-fail"></i>Failed</span></div></div>`;
}

function SpeedHistory({ hist, cm, current, running, onView, onDelete, onClear }) {
  return html`<div class="card">
    <div class="card-title"><div><h2><${Icon} n="history" />History</h2><p>Stored in FastRouter's database (last 200 runs), including <span class="mono">fastrouter bench</span> runs.</p></div>
      <div class="row" style="gap:8px"><a class="btn outline sm" href="/api/admin/bench/all/export?format=json" download><${Icon} n="download" />All JSON</a>
        <a class="btn outline sm" href="/api/admin/bench/all/export?format=csv" download><${Icon} n="download" />All CSV</a>
        <button class="btn ghost sm" disabled=${running || !hist || !hist.length} onClick=${onClear}><${Icon} n="delete" />Clear</button></div></div>
    ${!hist ? html`<p class="small muted">Loading…</p>` : !hist.length ? html`<div class="empty-line"><${Icon} n="history" />No runs yet</div>`
      : html`<div class="st-table-wrap"><table class="table st-table">
        <thead><tr><th>When</th><th>Source</th><th>Models</th><th>Setup</th><th class="num">Passed</th><th class="num">TTFT avg</th><th class="num st-over">Overhead avg</th><th></th></tr></thead>
        <tbody>${hist.map((h) => { const sm = h.summary || {}, o = h.options || {}, stats = sm.stats || [];
          const okRuns = stats.filter((s) => s.ok), w = okRuns.reduce((a, s) => a + s.ok, 0), tw = okRuns.reduce((a, s) => a + ttftN(s), 0);
          const cl = [...new Set(stats.map((s) => clientId(s, o)))];
          const t = tw ? okRuns.reduce((a, s) => a + s.ttft.avg * ttftN(s), 0) / tw : NaN, ov = w ? okRuns.reduce((a, s) => a + s.overhead.avg * s.ok, 0) / w : NaN;
          return html`<tr key=${h.id} class=${h.id === current ? "st-cur" : ""}>
          <td class="small">${new Date(h.startedAt).toLocaleString()}</td><td><${Badge} kind=${h.source === "cli" ? "info" : ""}>${h.source === "cli" ? "CLI" : "Dashboard"}</${Badge}>${h.status !== "done" && html` <${Badge} kind="warning">${h.status}</${Badge}>`}</td>
          <td class="small mono st-models" title=${(o.models || []).join("\n")}>${(o.models || []).slice(0, 2).join(", ")}${(o.models || []).length > 2 ? ` +${o.models.length - 2}` : ""}</td>
          <td class="small"><div class="row nowrap" style="gap:4px">${cl.slice(0, 4).map((id) => html`<span title=${clientName(cm, id)}><${ClientLogo} c=${cm[id]} /></span>`)}${cl.length > 4 ? html`<span class="muted">+${cl.length - 4}</span>` : ""}
            <span class="muted">${o.stream ? "stream" : "json"} · ${o.prompt} · ×${o.runs}</span></div></td>
          <td class="num">${sm.ok ?? "—"}/${sm.done ?? "—"}</td><td class="num">${msf(t)} ${!isNaN(t) && html`<${SpeedBadge} ms=${t} />`}</td><td class="num st-over">${ovf(ov)}</td>
          <td class="num nowrap"><button class="icon-btn" title="View" onClick=${() => onView(h.id)}><${Icon} n="visibility" /></button>
            <a class="icon-btn" title="Download CSV" href=${`/api/admin/bench/${h.id}/export?format=csv`} download><${Icon} n="download" /></a>
            <button class="icon-btn danger" title="Delete" disabled=${running && h.id === current} onClick=${() => onDelete(h)}><${Icon} n="delete" /></button></td></tr>`; })}</tbody></table></div>`}
  </div>`;
}

/* --------------------------------------------------------------- shell */
const NAV = [
  { id: "endpoint", label: "Dashboard", icon: "api", title: "Endpoint & Overview", sub: "Your local endpoint, connected providers and today's traffic" },
  { id: "providers", label: "Providers", icon: "dns", title: "Providers", sub: "Manage your AI provider connections" },
  { id: "combos", label: "Combos & Vision", icon: "layers", title: "Combos & Vision Adapter", sub: "Named model chains: fallback, round-robin, smart router or fusion" },
  { id: "usage", label: "Usage", icon: "bar_chart", title: "Usage", sub: "Requests, tokens and estimated cost" },
  { id: "speed-test", label: "Speed Test", icon: "speed", title: "Speed Test", sub: "How fast FastRouter answers your IDE and CLI — through the real /v1 endpoints" },
  { id: "quota", label: "Quota Tracker", icon: "data_usage", title: "Quota Tracker", sub: "Track and manage your API quota limits" },
  { id: "keys", label: "API Keys", icon: "vpn_key", title: "API Keys", sub: "Local keys your tools use to call FastRouter" },
  { id: "token-saver", label: "Token Saver", icon: "savings", title: "Token Saver", sub: "Compress prompts and outputs to save tokens" },
  { id: "cli", label: "CLI Tools", icon: "code_blocks", title: "CLI Tools", sub: "Point your coding agents at FastRouter" },
];
const SYSTEM = [
  { id: "skills", label: "Skills", icon: "extension", title: "Agent Skills", sub: "Copy a link and paste to your AI to use FastRouter — no install needed" },
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
/* Endpoint card: Local / Tunnel / Tailscale, like 9Router's API Endpoint. */
const TUNNEL_BENEFITS = [["public", "Access anywhere", "Use FastRouter from any network, no port forwarding"],
  ["share", "Share endpoint", "Hand the URL to a teammate or another computer"],
  ["code_blocks", "Use in Cursor / Cline", "Cloud IDEs and remote agents can reach it"],
  ["lock", "Encrypted", "HTTPS end to end through Cloudflare"]];
function SecurityChecks({ sec, go, onClose }) {
  const items = [[sec.activeKeys > 0, "API key", sec.activeKeys > 0 ? `${sec.activeKeys} active — callers must send one` : "Create one first: without a key FastRouter refuses tunnel traffic", "keys"],
    [sec.passwordSet, "Dashboard password", sec.passwordSet ? "Set" : "Set one in Settings first", "settings"]];
  return html`<div class="ep-checks">${items.map(([ok, l, s, page]) => html`<div class="ep-check ${ok ? "ok" : "bad"}">
    <${Icon} n=${ok ? "check_circle" : "cancel"} fill /><div class="grow"><strong>${l}</strong><span>${s}</span></div>
    ${!ok && html`<button class="btn outline xs" onClick=${() => { onClose && onClose(); go(page); }}>Open</button>`}</div>`)}</div>`;
}
function EpBox({ kind = "", spin, icon, children }) {
  return html`<div class="ep-box ${kind}">${spin ? html`<${Icon} n="progress_activity" cls="spin" />` : icon && html`<${Icon} n=${icon} />`}<span class="grow">${children}</span></div>`;
}
function EpUrl({ url, id, copied, copy }) {
  return html`<input class="input mono grow" readonly value=${url} onFocus=${(e) => e.target.select()} /><${CopyBtn} text=${url} id=${id} copied=${copied} copy=${copy} />`;
}
const PowerBtn = ({ onClick, title }) => html`<button class="icon-btn danger ep-power" title=${title} onClick=${onClick}><${Icon} n="power_settings_new" /></button>`;

function EndpointCard({ data, go }) {
  const [copied, copy] = useCopy();
  const [ep, setEp] = useState(null);
  const [busy, setBusy] = useState({});
  const [err, setErr] = useState({});
  const [modal, setModal] = useState("");
  const [base, setBase] = useState("local");
  const [tsMode, setTsMode] = useState("serve");
  const load = useCallback(() => api("GET", "/api/admin/endpoint").then(setEp).catch(() => {}), []);
  const cf = ep && ep.tunnel, ts = ep && ep.tailscale, sec = (ep && ep.security) || { activeKeys: 0, passwordSet: false };
  const moving = cf && (cf.download.active || (cf.enabled && (cf.state !== "running" || !cf.reachable)));
  useEffect(() => { load(); const t = setInterval(() => !document.hidden && load(), moving ? 2000 : 15000); return () => clearInterval(t); }, [moving]);
  const ready = sec.activeKeys > 0 && sec.passwordSet;
  const act = async (k, path, body, after) => {
    setBusy((b) => ({ ...b, [k]: true })); setErr((e) => ({ ...e, [k]: null }));
    try { const r = await api("POST", path, body || {}); after && after(r); await load(); return true; }
    catch (e) { setErr((x) => ({ ...x, [k]: { msg: e.message, url: e.data && e.data.actionUrl } })); return false; }
    finally { setBusy((b) => ({ ...b, [k]: false })); }
  };
  const local = ep ? ep.local : location.origin;
  const tunnelUrl = cf && cf.enabled && cf.url ? cf.url : "";
  const tsUrl = ts && ts.mode !== "off" ? ts.url : "";
  const bases = [["local", "Local", local], ...(tunnelUrl ? [["tunnel", "Tunnel", tunnelUrl]] : []), ...(tsUrl ? [["tailscale", "Tailscale", tsUrl]] : [])];
  const cur = (bases.find((b) => b[0] === base) || bases[0])[2];
  const exposed = !!(tunnelUrl || tsUrl || (cf && cf.enabled));
  const keys = sec.activeKeys;

  const tunnelRow = () => {
    if (!cf) return html`<${EpBox} spin>Checking…</${EpBox}>`;
    if (cf.download.active) return html`<${EpBox} spin>Downloading cloudflared… ${cf.download.progress}%</${EpBox}>`;
    if (busy.cf) return html`<${EpBox} spin>${cf.enabled ? "Working…" : "Creating tunnel…"}</${EpBox}>`;
    if (cf.enabled && cf.url) return html`<${EpUrl} url=${cf.url + "/v1"} id="tunnel" copied=${copied} copy=${copy} />
      <${Badge} kind=${cf.reachable ? "success" : "warning"} dot>${cf.reachable ? "Live" : cf.state === "reconnecting" ? "Reconnecting" : "Checking"}</${Badge}>
      <${PowerBtn} title="Disable tunnel" onClick=${() => setModal("tunnel-off")} />`;
    if (cf.enabled) return html`<${EpBox} kind="warn" spin>${cf.state === "reconnecting" ? "Tunnel reconnecting…" : "Creating tunnel…"}</${EpBox}>
      <${PowerBtn} title="Disable tunnel" onClick=${() => act("cf", "/api/admin/tunnel/disable")} />`;
    const e = err.cf || (cf.download.error && { msg: "Download failed: " + cf.download.error }) || (cf.error && { msg: cf.error });
    if (!cf.installed) return html`<${EpBox} kind=${e ? "err" : ""} icon=${e ? "error" : "cloud_off"}>${e ? e.msg : "cloudflared is not installed"}</${EpBox}>
      <button class="btn outline sm" title="Downloads cloudflared from Cloudflare's GitHub releases into FastRouter's folder" onClick=${() => act("cf", "/api/admin/tunnel/install")}><${Icon} n="download" />Install</button>`;
    return html`${e ? html`<${EpBox} kind="err" icon="error">${e.msg}</${EpBox}>` : html`<${EpBox} icon="cloud">Off — public HTTPS URL through Cloudflare, no account needed</${EpBox}>`}
      <button class="btn primary sm" onClick=${() => setModal("tunnel")}><${Icon} n="cloud_upload" />Enable</button>`;
  };
  const tailscaleRow = () => {
    if (!ts) return html`<${EpBox} spin>Checking…</${EpBox}>`;
    if (busy.ts) return html`<${EpBox} spin>Talking to Tailscale…</${EpBox}>`;
    const e = err.ts;
    const errBox = e && html`<${EpBox} kind="err" icon="error">${e.msg}</${EpBox}>${e.url && html`<a class="btn outline sm" href=${e.url} target="_blank" rel="noopener noreferrer"><${Icon} n="open_in_new" />Open</a>`}`;
    if (!ts.installed) return html`<${EpBox} icon="vpn_lock">Tailscale is not installed</${EpBox}>
      <a class="btn outline sm" href=${ep.tailscaleInstallUrl} target="_blank" rel="noopener noreferrer" title="Official Tailscale installer — nothing downloads until you click"><${Icon} n="download" />Install</a>`;
    if (!ts.running) return html`<${EpBox} kind="warn" icon="warning">Tailscale is installed but not running — open the Tailscale app</${EpBox}>`;
    if (!ts.loggedIn) return html`${errBox || html`<${EpBox} kind="warn" icon="login">Tailscale is not logged in</${EpBox}>`}
      <button class="btn ts sm" onClick=${() => act("ts", "/api/admin/tailscale/login", {}, (r) => r.authUrl && window.open(r.authUrl, "_blank", "noopener,noreferrer"))}><${Icon} n="login" />Log in</button>`;
    if (ts.mode !== "off") return html`<${EpUrl} url=${ts.url + "/v1"} id="ts" copied=${copied} copy=${copy} />
      <${Badge} kind=${ts.mode === "funnel" ? "warning" : "info"} icon=${ts.mode === "funnel" ? "public" : "lock"}>${ts.mode === "funnel" ? "Public" : "Tailnet"}</${Badge}>
      <${PowerBtn} title="Turn off Tailscale ${ts.mode === "funnel" ? "Funnel" : "Serve"}" onClick=${() => act("ts", "/api/admin/tailscale/disable")} />`;
    return html`${errBox || html`<${EpBox} icon="vpn_lock">Off — <span class="mono">${ts.dnsName}</span>${ts.ips && ts.ips[0] ? " · " + ts.ips[0] : ""}</${EpBox}>`}
      <button class="btn ts sm" onClick=${() => { setTsMode("serve"); setModal("ts"); }}><${Icon} n="vpn_lock" />Enable</button>`;
  };

  return html`
    <div class="card">
      <div class="card-title"><div><h2><${Icon} n="api" />API Endpoint</h2><p>Your FastRouter base URL — on this computer, through a Cloudflare tunnel, or over Tailscale.</p></div>
        <${Badge} kind=${keys ? "success" : "warning"} dot>${keys ? keys + " active API key" + (keys > 1 ? "s" : "") : "No API key — local only"}</${Badge}></div>
      <div class="col" style="gap:8px">
        <div class="ep-row"><span class="ep-tag">Local</span><${EpUrl} url=${local + "/v1"} id="local" copied=${copied} copy=${copy} />
          <${Badge} kind="success" dot>Running</${Badge}></div>
        ${ep && (ep.listensAll && ep.lan.length ? ep.lan.map((u, i) => html`<div class="ep-row"><span class="ep-tag">LAN</span><${EpUrl} url=${u + "/v1"} id=${"lan" + i} copied=${copied} copy=${copy} />
            <${Badge} kind=${keys ? "success" : "warning"} dot>${keys ? "Key required" : "Needs a key"}</${Badge}></div>`)
          : html`<div class="ep-row"><span class="ep-tag off">LAN</span><${EpBox} icon="lan">Other devices can't reach this port — FastRouter listens on ${ep.host || "127.0.0.1"} only. Use Tunnel or Tailscale${ep.lan[0] ? html`, or set <span class="mono">"host": "0.0.0.0"</span> in fastrouter.json for ${ep.lan[0]}` : ""}.</${EpBox}></div>`)}
        <div class="ep-row"><span class="ep-tag ${tunnelUrl ? "" : "off"}">Tunnel</span>${tunnelRow()}</div>
        <div class="ep-row"><span class="ep-tag ${tsUrl ? "" : "off"}">Tailscale</span>${tailscaleRow()}</div>
      </div>
      ${exposed && !ready && html`<div class="ep-warn"><${Icon} n="gpp_maybe" />${keys ? "" : "No API key is active — tunnel requests are refused until you create one. "}${sec.passwordSet ? "" : "No dashboard password is set — set one in Settings."}</div>`}
      ${(exposed || (ts && ts.mode !== "off")) && ep.security.remoteDashboard && html`<div class="ep-warn"><${Icon} n="warning" />The dashboard is reachable through the tunnel (password protected). Turn it off below if you only need the API.</div>`}
      <div class="ep-sub">
        <div class="row" style="justify-content:space-between;gap:10px;flex-wrap:wrap">
          <span class="label small"><${Icon} n="swap_horiz" />Client formats</span>
          ${bases.length > 1 && html`<${Seg} value=${base} options=${bases.map(([v, l]) => [v, l])} onChange=${setBase} />`}</div>
        ${[["OpenAI", cur + "/v1"], ["Anthropic", cur], ["Gemini", cur + "/v1beta"]].map(([l, u]) => html`<div class="ep-row sm">
          <span class="ep-tag fmt">${l}</span><input class="input mono grow" readonly value=${u} /><${CopyBtn} text=${u} id=${"f" + l} copied=${copied} copy=${copy} /></div>`)}
        <div class="row ep-dash"><div class="grow"><strong class="small">Dashboard over tunnel</strong><div class="muted small">Off: Tunnel and Tailscale serve only the API (/v1, /v1beta) with an API key; this page stays on this computer.</div></div>
          <${Toggle} on=${!!(ep && ep.security.remoteDashboard)} disabled=${!ep || (!sec.passwordSet && !ep.security.remoteDashboard)} title=${sec.passwordSet ? "" : "Set a dashboard password first"}
            onChange=${(v) => act("dash", "/api/admin/remote-dashboard", { enabled: v }).then((ok) => !ok && toast("Couldn't change dashboard access", "error"))} /></div>
      </div>
    </div>
    ${modal === "tunnel" && html`<${Modal} title="Enable Cloudflare Tunnel" icon="cloud_upload" onClose=${() => setModal("")}
      foot=${html`<button class="btn ghost" onClick=${() => setModal("")}>Cancel</button>
        <button class="btn primary" disabled=${!ready} onClick=${() => { setModal(""); act("cf", "/api/admin/tunnel/enable"); }}><${Icon} n="cloud_upload" />Enable tunnel</button>`}>
      <div class="ep-benefits">${TUNNEL_BENEFITS.map(([ic, t, s]) => html`<div><${Icon} n=${ic} /><strong>${t}</strong><span>${s}</span></div>`)}</div>
      <p class="muted small">Runs a Cloudflare quick tunnel (<span class="mono">${cf && cf.source === "9router" ? "using 9Router's cloudflared" : "cloudflared"}</span>) to this FastRouter. The trycloudflare.com URL changes each time the tunnel restarts. It restarts by itself if it drops.</p>
      <${SecurityChecks} sec=${sec} go=${go} onClose=${() => setModal("")} /></${Modal}>`}
    ${modal === "tunnel-off" && html`<${Confirm} title="Disable tunnel?" text="The public URL stops working right away. Enabling again gives a new URL." label="Disable"
      onYes=${() => act("cf", "/api/admin/tunnel/disable")} onClose=${() => setModal("")} />`}
    ${modal === "ts" && ts && html`<${Modal} title="Enable Tailscale" icon="vpn_lock" onClose=${() => setModal("")}
      foot=${html`<button class="btn ghost" onClick=${() => setModal("")}>Cancel</button>
        <button class="btn ts" disabled=${!ready} onClick=${() => { setModal(""); act("ts", "/api/admin/tailscale/enable", { mode: tsMode }); }}><${Icon} n="vpn_lock" />Enable ${tsMode === "funnel" ? "Funnel" : "Serve"}</button>`}>
      <${Seg} value=${tsMode} options=${[["serve", "Tailnet only (Serve)"], ["funnel", "Public (Funnel)"]]} onChange=${setTsMode} />
      <p class="muted small" style="margin:12px 0">${tsMode === "serve"
        ? html`Devices on your tailnet reach FastRouter at <span class="mono">${ts.https ? "https://" + ts.dnsName : "http://" + ts.dnsName + ":" + ep.port}</span>.${ts.https ? "" : " Your tailnet has no HTTPS certificates, so Serve uses plain HTTP inside the encrypted tailnet."}`
        : ts.https ? html`Anyone on the internet can reach <span class="mono">https://${ts.dnsName}</span> — an API key is still required.`
        : html`Funnel needs HTTPS certificates. Turn on HTTPS on the DNS page of the Tailscale admin console first.`}
        ${" "}Other Tailscale Serve ports${ts.otherPorts && ts.otherPorts.length ? " (" + ts.otherPorts.join(", ") + ")" : ""} are left as they are.</p>
      <${SecurityChecks} sec=${sec} go=${go} onClose=${() => setModal("")} /></${Modal}>`}`;
}

function EndpointPage({ data, go }) {
  const [usage, setUsage] = useState(null);
  useEffect(() => { api("GET", "/api/usage?window=24h").then(setUsage).catch(() => {}); }, []);
  const origin = location.origin;
  const connected = data.providers.filter((p) => p.accounts.length > 0);
  const accounts = connected.reduce((n, p) => n + p.accounts.length, 0);
  const usable = data.providers.reduce((n, p) => n + p.usable, 0);
  const t = ((usage && usage.models) || []).reduce((a, r) => ({ req: a.req + r.requests, tok: a.tok + r.input_tokens + r.output_tokens, cost: a.cost + r.cost_usd }), { req: 0, tok: 0, cost: 0 });
  const stats = [["dns", "Providers", connected.length, `${data.providers.length} available`],
    ["group", "Accounts", accounts, `${usable} usable right now`],
    ["layers", "Combos", data.combos.length, `${data.models.length} routable models`],
    ["speed", "Requests · 24h", fmt(t.req), t.req ? `${fmt(t.tok)} tokens · ${money(t.cost)}` : "No traffic yet"]];
  return html`
    <${EndpointCard} data=${data} go=${go} />
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
// Providers page, laid out like 9Router's: Custom, OAuth, Free Tier and API Key
// sections, each with Test All; status filter and search. Every provider 9Router
// lists is shown; the ones FastRouter can't route yet say why.
const STATUS_FILTER = [["all", "All"], ["active", "Active"], ["inactive", "Inactive"], ["none", "No connection"]];
const SECTION_OF = { oauth: "oauth", free: "free", freeTier: "free", apikey: "apikey", webCookie: "apikey" };
function provStatus(p) {
  if (p.unsupported) return "unsupported";
  if (p.noAuth && p.kind !== "plugin") return "active";
  if (!p.accounts || p.accounts.length === 0) return "none";
  return p.accounts.every((a) => !a.active) ? "inactive" : "active";
}
function cardStatus(p) {
  if (p.unsupported) return html`<${Badge}>Not supported</${Badge}>`;
  if (p.kind === "plugin") return html`<${Badge} icon="extension">Needs plugin</${Badge}>`;
  if (p.noAuth) return html`<${Badge} kind="success" dot>Ready</${Badge}>`;
  if (p.accounts.length === 0) return html`<span class="muted">No connections</span>`;
  const active = p.accounts.filter((a) => a.active);
  if (active.length === 0) return html`<${Badge} icon="pause_circle">Disabled</${Badge}>`;
  const bad = active.filter((a) => (a.expired && !a.autoRefresh) || (a.status !== "ok" && a.status !== "needs-plugin" && a.status !== "unknown-provider")).length;
  const cooling = active.filter((a) => a.cooling).length;
  const ok = active.length - bad;
  return html`${ok > 0 && html`<${Badge} kind="success" dot>${ok} Connected</${Badge}>`}
    ${cooling > 0 && html`<${Badge} kind="warning" dot>${cooling} Cooling</${Badge}>`}
    ${bad > 0 && html`<${Badge} kind="error" dot>${bad} Error</${Badge}>`}`;
}
function TestResults({ res, data, onClose }) {
  const name = (id) => { const p = data.providers.find((x) => x.id === id); return p ? provMeta(p).name : id; };
  return html`<${Modal} title="Test results" icon="play_arrow" onClose=${onClose} foot=${html`<button class="btn primary" onClick=${onClose}>Done</button>`}>
    ${res.summary.total === 0 ? html`<p class="small muted">No active accounts in this section to test.</p>` : html`
    <p class="small" style="margin-bottom:12px"><b>${res.summary.passed}</b> of ${res.summary.total} passed${res.summary.failed ? html` · <span style="color:var(--red-fg)">${res.summary.failed} failed</span>` : ""}. Tests use free read-only calls (models list or usage API); no tokens are spent.</p>
    <div class="col" style="gap:6px">${res.results.map((r) => html`<div class="test-row">
      <span class="sdot" style=${`background:${r.ok ? "var(--success)" : "var(--danger, #ef4444)"}`}></span>
      <span class="medium">${name(r.provider)}</span><span class="muted small truncate">${r.name}</span><span class="grow"></span>
      <span class="small ${r.ok ? "muted" : ""}" style=${r.ok ? "" : "color:var(--red-fg)"}>${r.message}${r.status ? ` (${r.status})` : ""}</span><span class="small muted mono">${r.ms}ms</span></div>`)}</div>`}
  </${Modal}>`;
}
function Unsupported({ p, onClose, onAdd }) {
  return html`<${Modal} title=${p.name} icon="info" onClose=${onClose}
    foot=${html`<button class="btn ghost" onClick=${onClose}>Close</button><button class="btn primary" onClick=${onAdd}><${Icon} n="add" />Add OpenAI Compatible</button>`}>
    <div class="row" style="gap:12px;margin-bottom:12px"><${Logo} p=${p} lg /><div><div class="strong">${p.name}</div><div class="small muted">Listed by 9Router · not routable by FastRouter yet</div></div></div>
    <p class="small">${p.reason}</p>
    ${p.keyUrl && html`<p class="small" style="margin-top:10px">API keys: <a href=${p.keyUrl} target="_blank" rel="noreferrer">${p.keyUrl}</a></p>`}</${Modal}>`;
}
function ProvidersPage({ data, reload, go, query }) {
  const [modal, setModal] = useState(/(^|&)add\b/.test(query) ? { t: "account" } : null);
  const [q, setQ] = useState("");
  const [filter, setFilter] = useState("all");
  const [showAll, setShowAll] = useState(false);
  const [testing, setTesting] = useState("");
  const [results, setResults] = useState(null);
  const ok = matcher(q);
  const toggleAll = (p, on) => run(async () => { for (const a of p.accounts) await api("PATCH", `/api/admin/accounts/${a.id}`, { active: on }); await reload(); }, on ? "Provider enabled" : "Provider disabled");
  const test = async (category) => {
    setTesting(category);
    try { setResults(await api("POST", "/api/admin/providers/test", { category })); } catch (e) { toast(e.message, "error"); }
    setTesting("");
  };
  const all = [...data.providers.filter((p) => !p.kind.endsWith("compatible")),
    ...(data.others || []).map((o) => ({ ...o, unsupported: true, accounts: [], kind: "other" }))];
  const visible = (p) => ok(`${provMeta(p).name} ${p.id} ${p.prefix || ""}`) && (filter === "all" || provStatus(p) === filter);
  const connected = (p) => (p.accounts.some((a) => a.active) || (p.noAuth && p.kind !== "plugin") ? 0 : 1);
  const byPriority = (a, b) => (a.priority || 999) - (b.priority || 999) || connected(a) - connected(b) || a.unsupported - b.unsupported || provMeta(a).name.localeCompare(provMeta(b).name);
  const byName = (a, b) => (b.accounts.length > 0) - (a.accounts.length > 0) || provMeta(a).name.localeCompare(provMeta(b).name);
  const group = (sec) => all.filter((p) => SECTION_OF[p.category || "apikey"] === sec);
  const oauth = group("oauth").filter(visible).sort(byPriority);
  const free = group("free").filter(visible).sort((a, b) => (b.noAuth ? 1 : 0) - (a.noAuth ? 1 : 0) || byPriority(a, b));
  const apikeyAll = group("apikey").filter(visible).sort(byName);
  const searching = q.trim() !== "" || filter !== "all";
  const apikey = searching || showAll ? apikeyAll : apikeyAll.slice(0, 20);
  const nodes = data.providers.filter((p) => p.kind.endsWith("compatible") && visible(p));
  const open = (p) => (p.unsupported ? setModal({ t: "unsupported", p }) : go("providers/" + encodeURIComponent(p.id)));
  const card = (p) => html`<div class="card xs hover pcard" key=${p.id} onClick=${() => open(p)}
      style=${(p.accounts.length && p.accounts.every((a) => !a.active)) || p.unsupported ? "opacity:.55" : ""} title=${p.unsupported ? p.reason : ""}>
    <div class="prov-card"><div class="row grow" style="gap:12px;min-width:0"><${Logo} p=${p} />
      <div class="grow" style="min-width:0"><div class="prov-name truncate">${provMeta(p).name}</div>
        <div class="prov-meta">${cardStatus(p)}</div></div></div>
      ${p.accounts.length > 0 && p.kind !== "plugin" && html`<span onClick=${(e) => e.stopPropagation()}><${Toggle} sm on=${p.accounts.some((a) => a.active)} title="Enable or disable every account" onChange=${(v) => toggleAll(p, v)} /></span>`}
    </div></div>`;
  const testBtn = (cat, label) => html`<button class="btn outline sm" disabled=${!!testing} title=${`Test all ${label} connections`} onClick=${() => test(cat)}>
    <${Icon} n=${testing === cat ? "progress_activity" : "play_arrow"} cls=${testing === cat ? "spin" : ""} />${testing === cat ? "Testing…" : "Test All"}</button>`;
  const section = (title, list, cat, label, extra) => list.length > 0 && html`<section>
    <div class="sec-head"><h2>${title}</h2>${testBtn(cat, label)}</div>
    <div class="prov-grid">${list.map(card)}</div>${extra}</section>`;
  const total = all.length;
  return html`
    <div class="prov-bar">
      <div class="picker-search" style="width:320px"><${Icon} n="search" /><input class="input" placeholder="Search ${total} providers…" value=${q} onInput=${(e) => setQ(e.target.value)} /></div>
      <span class="grow"></span>
      <select class="input select-sm" value=${filter} onChange=${(e) => setFilter(e.target.value)} aria-label="Filter providers by connection status">
        ${STATUS_FILTER.map(([v, l]) => html`<option value=${v}>${l}</option>`)}</select></div>
    <section>
      <div class="sec-head"><h2>Custom Providers (OpenAI/Anthropic Compatible)</h2>
        <div class="row" style="gap:8px">
          <button class="btn primary sm" onClick=${() => setModal({ t: "node", type: "anthropic-compatible" })}><${Icon} n="add" />Add Anthropic Compatible</button>
          <button class="btn outline sm" onClick=${() => setModal({ t: "node", type: "openai-compatible" })}><${Icon} n="add" />Add OpenAI Compatible</button>
          ${nodes.length > 0 && testBtn("custom", "custom")}</div></div>
      ${nodes.length ? html`<div class="prov-grid">${nodes.map((p) => html`<div class="card xs hover pcard" key=${p.id} onClick=${() => go("providers/" + encodeURIComponent(p.id))}
          style=${p.accounts.length && p.accounts.every((a) => !a.active) ? "opacity:.55" : ""}>
        <div class="prov-card"><div class="row grow" style="gap:12px;min-width:0"><${Logo} p=${p} />
          <div class="grow" style="min-width:0"><div class="prov-name truncate">${p.name}</div>
            <div class="prov-meta">${cardStatus(p)}<${Badge}>${p.kind === "anthropic-compatible" ? "Messages" : p.apiType === "responses" ? "Responses" : "Chat"}</${Badge}><span class="prefix-tag mono" title=${`Models are called as ${p.prefix}/model` + (p.defaultModel ? ` · default ${p.defaultModel}` : "")}>${p.prefix}/</span></div></div></div>
          ${p.accounts.length > 0 && html`<span onClick=${(e) => e.stopPropagation()}><${Toggle} sm on=${p.accounts.some((a) => a.active)} onChange=${(v) => toggleAll(p, v)} /></span>`}</div></div>`)}</div>`
        : html`<div class="empty-line"><${Icon} n="extension" />No custom providers — use buttons above to add OpenAI/Anthropic compatible endpoints</div>`}
    </section>
    ${section("OAuth Providers", oauth, "oauth", "OAuth")}
    ${section("Free Tier Providers", free, "free", "Free")}
    ${section("API Key Providers", apikey, "apikey", "API Key", !searching && apikeyAll.length > 20 && html`<button class="show-all" onClick=${() => setShowAll(!showAll)}>
      <${Icon} n=${showAll ? "expand_less" : "expand_more"} />${showAll ? "Show less" : `Show all ${apikeyAll.length} providers`}</button>`)}
    ${!oauth.length && !free.length && !apikey.length && !nodes.length && html`<div class="empty-line"><${Icon} n="search_off" />No providers match your search or filters</div>`}
    ${modal && modal.t === "account" && html`<${AccountForm} data=${data} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "node" && html`<${NodeForm} type=${modal.type} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "unsupported" && html`<${Unsupported} p=${modal.p} onClose=${() => setModal(null)} onAdd=${() => setModal({ t: "node", type: "openai-compatible" })} />`}
    ${results && html`<${TestResults} res=${results} data=${data} onClose=${() => setResults(null)} />`}`;
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

// Custom endpoint form, laid out like 9Router's AddCompatibleModal: name, prefix,
// API style, base URL, key (for the check), optional model ID, then Check API.
const slug = (s) => s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 32);
const prefixProblem = (v) => !v ? "" : /\//.test(v) ? "No slash — it becomes prefix/model" : /\s/.test(v) ? "No spaces"
  : !/^[a-z0-9][a-z0-9_-]{0,31}$/.test(v) ? "Lower-case letters, digits, - or _ (max 32)" : "";
function NodeForm({ node, type, onClose, onSaved }) {
  const [f, setF] = useState(node ? { name: node.name, prefix: node.prefix, baseUrl: node.baseUrl, type: node.kind, apiType: node.apiType || "chat", modelId: node.defaultModel || "" }
    : { name: "", prefix: "", baseUrl: "", type: type || "openai-compatible", apiType: "chat", modelId: "" });
  const [key, setKey] = useState("");
  const [keepKey, setKeepKey] = useState(true);
  const [touched, setTouched] = useState(!!node);
  const [pfx, setPfx] = useState({ ok: true, msg: "" });
  const [chk, setChk] = useState(null);
  const [checking, setChecking] = useState(false);
  const [busy, setBusy] = useState(false);
  const anth = f.type === "anthropic-compatible";
  const saved = node && node.accounts && node.accounts.some((a) => a.active && a.keyHint);
  const set = (k) => (e) => { const v = e && e.target ? e.target.value : e; setF((o) => ({ ...o, [k]: v })); if (k !== "name") setChk(null); };
  const setName = (e) => { const v = e.target.value; setF((o) => ({ ...o, name: v, prefix: touched ? o.prefix : slug(v) })); };
  const local = prefixProblem(f.prefix.trim());
  useEffect(() => {
    const v = f.prefix.trim();
    if (!v || local) { setPfx({ ok: !local, msg: local }); return; }
    const t = setTimeout(() => api("GET", `/api/admin/nodes/prefix?prefix=${encodeURIComponent(v)}${node ? "&except=" + encodeURIComponent(node.id) : ""}`)
      .then((r) => setPfx({ ok: r.ok, msg: r.ok ? "" : r.message }), () => {}), 250);
    return () => clearTimeout(t);
  }, [f.prefix]);
  const check = async () => {
    setChecking(true); setChk(null);
    try {
      const r = await api("POST", "/api/admin/nodes/check", { type: f.type, apiType: f.apiType, baseUrl: f.baseUrl, apiKey: key, modelId: f.modelId.trim(), prefix: f.prefix.trim(), nodeId: node ? node.id : "" });
      setChk(r);
    } catch (x) { setChk({ ok: false, class: "network", message: String(x.message || x) }); }
    setChecking(false);
  };
  const save = async (e) => {
    if (e) e.preventDefault();
    setBusy(true);
    const body = { name: f.name.trim(), prefix: f.prefix.trim(), baseUrl: f.baseUrl.trim(), modelId: f.modelId.trim() };
    if (!anth) body.apiType = f.apiType;
    try {
      if (node) await run(() => api("PATCH", `/api/admin/nodes/${encodeURIComponent(node.id)}`, body), "Endpoint saved");
      else {
        const r = await run(() => api("POST", "/api/admin/nodes", { ...body, type: f.type }), key && keepKey ? "Endpoint added with its key" : "Endpoint added — now add an account to it");
        if (key && keepKey) await api("POST", "/api/admin/accounts", { provider: r.id, name: "default", apiKey: key, active: true });
      }
      setKey(""); await onSaved(); onClose();
    } catch (x) { setBusy(false); }
  };
  const can = f.name.trim() && f.prefix.trim() && f.baseUrl.trim() && pfx.ok && !local;
  const canCheck = f.baseUrl.trim() && (key || saved) && !checking;
  const req = (l) => html`${l}<span class="req">*</span>`;
  const opt = (l) => html`${l}<span class="opt">optional</span>`;
  const pre = f.prefix.trim() || (anth ? "ac-prod" : "oc-prod");
  return html`<${Modal} title=${node ? "Edit endpoint" : anth ? "Add Anthropic Compatible" : "Add OpenAI Compatible"} icon=${node ? "edit" : "add_link"} onClose=${onClose}
    foot=${html`<button class="btn ghost" onClick=${onClose}>Cancel</button><button class="btn primary" disabled=${busy || !can} onClick=${save}><${Icon} n="save" />${node ? "Save" : "Add endpoint"}</button>`}>
    <form onSubmit=${save} class="col node-form" style="gap:16px">
      ${!node && html`<${Field} label="Type"><${Seg} value=${f.type} onChange=${(v) => { set("type")(v); }} options=${[["openai-compatible", "OpenAI compatible"], ["anthropic-compatible", "Anthropic compatible"]]} /></${Field}>`}
      <div class="grid g2">
        <${Field} label=${req("Name")} hint="A friendly label for this endpoint."><input class="input" value=${f.name} maxlength="100" placeholder=${anth ? "Anthropic Compatible (Prod)" : "OpenAI Compatible (Prod)"} onInput=${setName} /></${Field}>
        <${Field} label=${req("Prefix")} hint=${pfx.msg || local ? html`<span class="danger-text">${local || pfx.msg}</span>` : html`Call models as <code>${pre}/model</code>`}>
          <div class="input-wrap"><input class="input mono ${local || !pfx.ok ? "invalid" : ""}" value=${f.prefix} maxlength="32" placeholder=${anth ? "ac-prod" : "oc-prod"} spellcheck="false"
            onInput=${(e) => { setTouched(true); set("prefix")(e); }} />
            ${f.prefix.trim() && html`<span class="input-adorn ${local || !pfx.ok ? "bad" : "good"}"><${Icon} n=${local || !pfx.ok ? "error" : "check_circle"} /></span>`}</div></${Field}></div>
      ${!anth && html`<${Field} label="API style" hint=${f.apiType === "responses" ? "Requests go to /responses." : "Requests go to /chat/completions."}><${Seg} value=${f.apiType} onChange=${set("apiType")} options=${[["chat", "Chat Completions"], ["responses", "Responses"]]} /></${Field}>`}
      <${Field} label=${req("Base URL")} hint=${anth ? "Up to /v1 — FastRouter adds /messages and /models." : "Up to /v1 — FastRouter adds /models and the request path."}>
        <input class="input mono" value=${f.baseUrl} placeholder=${anth ? "https://api.anthropic.com/v1" : "https://api.openai.com/v1"} spellcheck="false" onInput=${set("baseUrl")} /></${Field}>
      <div class="grid g2">
        <${Field} label=${node ? opt("API key (for check)") : "API key (for check)"} hint=${node ? (saved ? "Blank uses the saved key. Keys are never shown." : "No saved key yet — add an account after saving.") : "Used for Check API. Never shown again."}>
          <${SecretInput} value=${key} onInput=${(v) => { setKey(v); setChk(null); }} placeholder=${node && saved ? "Saved key " + node.accounts.find((a) => a.active && a.keyHint).keyHint : anth ? "sk-ant-…" : "sk-…"} /></${Field}>
        <${Field} label=${opt("Model ID")} hint="Default model for tests and prefix/default. Empty uses the first listed model.">
          <input class="input mono" value=${f.modelId} maxlength="200" placeholder=${anth ? "e.g. claude-sonnet-4-5" : "e.g. gpt-4.1-mini"} spellcheck="false" onInput=${set("modelId")} /></${Field}></div>
      ${!node && key && html`<label class="row small" style="gap:8px;cursor:pointer"><input type="checkbox" checked=${keepKey} onChange=${(e) => setKeepKey(e.target.checked)} />Save this key as the endpoint's first account (encrypted)</label>`}
      <div class="check-box ${chk ? (chk.ok ? "ok" : "bad") : ""}">
        <div class="row" style="gap:12px">
          <div class="row" style="gap:8px;min-width:0;flex-wrap:wrap">
            <button type="button" class="btn outline sm" disabled=${!canCheck} onClick=${check}><${Icon} n=${checking ? "progress_activity" : "network_check"} cls=${checking ? "spin" : ""} />${checking ? "Checking…" : "Check API"}</button>
            ${!chk && !checking && html`<span class="small muted">${canCheck ? "Lists models, or sends one tiny request. Nothing is saved." : "Enter a base URL and key to check."}</span>`}
            ${chk && html`<${Badge} kind=${chk.ok ? "success" : "error"} icon=${chk.ok ? "check_circle" : "cancel"}>${chk.ok ? "Valid" : "Invalid"}</${Badge}>`}
            ${chk && chk.ms > 0 && html`<span class="badge"><${Icon} n="timer" />${chk.ms} ms</span>`}
            ${chk && !chk.ok && chk.class && html`<${Badge} kind="warning">${CLASS_LABEL[chk.class] || chk.class}</${Badge}>`}</div>
</div>
        ${chk && html`<div class="small check-msg">${chk.message ? chk.message.charAt(0).toUpperCase() + chk.message.slice(1) : ""}
          ${chk.method && html`<span class="muted"> · ${chk.method === "models" ? "GET /models" : "tiny request" + (chk.usedModel ? " to " + chk.usedModel : "")}${chk.status ? " · HTTP " + chk.status : ""}${chk.keySource === "saved" ? " · saved key" : ""}</span>`}
          ${chk.modelFound === false && html`<div class="warn-text">Model ID "${f.modelId.trim()}" is not in the list.</div>`}
          ${chk.prefixOk === false && html`<div class="danger-text">${chk.prefixMessage}</div>`}</div>`}
        ${chk && chk.sample && chk.sample.length > 0 && html`<div class="chips">${chk.sample.map((m) => html`<button type="button" class="chip mono ${f.modelId.trim() === m ? "on" : ""}" title="Use as Model ID" onClick=${() => setF((o) => ({ ...o, modelId: m }))}>${m}</button>`)}
          ${chk.models > chk.sample.length && html`<span class="small muted">+${chk.models - chk.sample.length} more</span>`}</div>`}
      </div>
    </form></${Modal}>`;
}

// 9Router's Risk Notice (shared/constants/providersDisplay.js RISK_NOTICE).
const RISK_NOTICE = "This provider uses a subscription / OAuth session that is not officially licensed for proxy or router use. The account may be restricted or banned. Use at your own risk.";
const CLASS_LABEL = { auth: "auth / access denied", banned: "banned", quota: "quota", rate_limit: "rate limit", model: "model", bad_request: "bad request", overloaded: "overloaded", upstream: "provider error", network: "network", timeout: "timeout", empty: "empty" };

function ProviderDetail({ data, id, reload, go, query }) {
  const p = data.providers.find((x) => x.id === id);
  const [modal, setModal] = useState(/(^|&)add\b/.test(query) ? { t: "add" } : null);
  const [order, setOrder] = useState(null);
  const [sel, setSel] = useState({});
  const [tests, setTests] = useState({});
  const [sum, setSum] = useState(null);
  const stopRef = useRef(false);
  if (!p) return html`<div class="card empty"><p>Provider not found.</p><button class="btn outline" onClick=${() => go("providers")}>Back</button></div>`;
  const accts = order || p.accounts;
  const meta = provMeta(p);
  const isLogin = p.kind === "login";
  const canAdd = p.kind === "apikey" || p.kind.endsWith("compatible");
  const strat = (data.settings.providerStrategies || {})[p.id] || {};
  const rr = strat.fallbackStrategy === "round-robin";
  const patch = (a, body, ok) => run(async () => { await api("PATCH", `/api/admin/accounts/${a.id}`, body); await reload(); }, ok);
  const setStrategy = (fallbackStrategy, sticky) => run(async () => {
    await api("PATCH", "/api/admin/settings", { providerStrategies: { [p.id]: { fallbackStrategy, stickyRoundRobinLimit: sticky || 0 } } }); await reload();
  }, "Routing updated");
  const moveTo = async (i, j) => {
    if (j < 0 || j >= accts.length) return;
    const next = accts.slice(); const [x] = next.splice(i, 1); next.splice(j, 0, x); setOrder(next);
    try { await run(() => api("POST", "/api/admin/accounts/reorder", { provider: p.id, ids: next.map((a) => a.id) }), "Priority updated"); await reload(); } catch (e) {}
    setOrder(null);
  };
  const add = () => (isLogin ? (p.risk ? setModal({ t: "risk" }) : setModal({ t: "signin", conn: "" })) : setModal({ t: "add" }));
  const testOne = async (a) => {
    setTests((t) => ({ ...t, [a.id]: { running: true } }));
    try {
      const r = await api("POST", "/api/admin/providers/test", { mode: "chat", provider: p.id, account: a.id });
      const x = r.results[0] || { ok: false, message: "account not routable (inactive or not signed in)" };
      setTests((t) => ({ ...t, [a.id]: x })); return x;
    } catch (e) { const x = { ok: false, message: e.message }; setTests((t) => ({ ...t, [a.id]: x })); return x; }
  };
  const oneByOne = async () => {
    stopRef.current = false;
    const list = accts.filter((a) => a.active);
    const s0 = { total: list.length, completed: 0, passed: 0, failed: 0, running: true };
    setSum(s0); setTests({});
    for (const a of list) {
      if (stopRef.current) { s0.stopped = true; break; }
      s0.current = a.name; setSum({ ...s0 });
      const x = await testOne(a);
      s0.completed++; x.ok ? s0.passed++ : s0.failed++; setSum({ ...s0 });
    }
    s0.running = false; s0.current = ""; setSum({ ...s0 }); reload();
  };
  const selected = accts.filter((a) => sel[a.id]);
  const allSel = accts.length > 0 && selected.length === accts.length;
  const ready = (a) => a.active && !a.expired && (a.status === "ok" || a.status === "needs-plugin" || a.status === "unknown-provider");
  const statusPill = (a) => !a.active ? html`<${Badge} dot>disabled</${Badge}>`
    : a.status === "banned" ? html`<${Badge} kind="error" icon="cancel">banned</${Badge}>` : a.cooling ? html`<${Badge} kind="warning" dot>cooling</${Badge}>`
    : a.expired ? html`<${Badge} kind="warning" dot>expired${a.autoRefresh ? " · refreshing" : ""}</${Badge}>`
    : ready(a) ? html`<${Badge} kind="success" dot>active</${Badge}>` : html`<${Badge} kind="error" dot>${a.status === "needs-reauth" ? "sign in again" : a.status}</${Badge}>`;
  const testPill = (a) => {
    const t = tests[a.id];
    if (t && t.running) return html`<${Badge} kind="info" icon="progress_activity">testing</${Badge}>`;
    if (t) return t.ok ? html`<${Badge} kind="success">success · ${t.ms}ms</${Badge}>` : html`<${Badge} kind="error" title=${t.message}>failed${t.class ? " · " + (CLASS_LABEL[t.class] || t.class) : ""}</${Badge}>`;
    if (a.lastStatus) return a.lastStatus < 300 ? html`<${Badge} kind="success">success</${Badge}>` : html`<${Badge} kind="error">failed · ${a.lastStatus}</${Badge}>`;
    return null;
  };
  return html`
    <nav class="crumbs"><a href="#/providers">Providers</a><${Icon} n="chevron_right" /><span class="row" style="gap:6px"><${Logo} p=${p} xs />${meta.name}</span></nav>
    <div class="pd-head">
      <${Logo} p=${p} lg />
      <div class="grow" style="min-width:0">
        <div class="row" style="gap:12px;flex-wrap:wrap"><h1 class="pd-title">${meta.name}</h1>
          ${(p.website || meta.key) && html`<a class="pd-link" href=${meta.key && !isLogin ? meta.key : p.website || meta.key} target="_blank" rel="noopener noreferrer"><${Icon} n="open_in_new" />${meta.key && !isLogin ? "Get API Key" : "Sign up / Learn more"}</a>`}</div>
        <div class="muted">${p.accounts.length} connection${p.accounts.length === 1 ? "" : "s"} · model prefix <span class="mono prefix-tag">${p.prefix}/</span>${p.defaultModel ? html` · default model <span class="mono">${p.defaultModel}</span>` : ""}${p.baseUrl ? html` · <span class="mono">${p.baseUrl}</span>` : ""}</div>
      </div>
      ${p.kind.endsWith("compatible") && html`<div class="row" style="gap:8px"><button class="btn outline" onClick=${() => setModal({ t: "node" })}><${Icon} n="edit" />Edit endpoint</button>
        <button class="btn ghost" onClick=${() => setModal({ t: "delnode" })}><${Icon} n="delete" />Delete</button></div>`}
    </div>
    ${p.risk && html`<div class="risk"><${Icon} n="warning" /><p><b>Risk Notice:</b> ${RISK_NOTICE}</p></div>`}
    ${p.kind === "plugin" && html`<div class="notice warn"><${Icon} n="extension" />${meta.name} talks a custom binary protocol FastRouter does not ship yet. Imported accounts are kept and skipped at runtime.</div>`}

    <div class="card">
      <div class="row between wrap" style="gap:12px;margin-bottom:14px">
        <h2 class="pd-h2">Connections</h2>
        <div class="row wrap" style="gap:12px">
          ${selected.length > 0 && html`<button class="btn danger sm" onClick=${() => setModal({ t: "delsel" })}><${Icon} n="delete" />Delete Selected (${selected.length})</button>`}
          ${accts.length > 0 && html`<button class="btn outline sm" disabled=${sum && sum.running} onClick=${oneByOne}><${Icon} n=${sum && sum.running ? "progress_activity" : "sync"} cls=${sum && sum.running ? "spin" : ""} />${sum && sum.running ? "Testing Connection One-by-One…" : "Test Connection One-by-One"}</button>`}
          ${sum && sum.running && html`<button class="btn ghost sm" onClick=${() => { stopRef.current = true; }}><${Icon} n="stop" />Stop</button>`}
          <div class="row" style="gap:8px"><span class="small muted medium">Round Robin</span><${Toggle} sm on=${rr} onChange=${(v) => setStrategy(v ? "round-robin" : "fallback", strat.stickyRoundRobinLimit)} />
            ${rr && html`<span class="small muted">Sticky:</span><input class="input sticky-in" type="number" min="1" value=${strat.stickyRoundRobinLimit || data.settings.stickyRoundRobinLimit || 3}
              onChange=${(e) => setStrategy("round-robin", Math.max(1, parseInt(e.target.value, 10) || 1))} />`}</div>
        </div>
      </div>
      ${sum && html`<div class="sumbar"><span>Total: ${sum.total}</span><span>Completed: ${sum.completed}</span><span style="color:var(--green-fg)">Passed: ${sum.passed}</span><span style=${sum.failed ? "color:var(--red-fg)" : ""}>Failed: ${sum.failed}</span>
        ${sum.stopped && html`<span style="color:var(--amber-fg)">Stopped</span>`}${sum.running && sum.current && html`<span>Running: ${sum.current}</span>`}
        <span class="grow"></span><span class="muted">One "Say OK" request (5 tokens max) per account</span></div>`}
      ${accts.length === 0 ? html`<div class="row between wrap" style="gap:12px">
          <div class="row" style="gap:12px"><div class="bubble sm"><${Icon} n=${isLogin ? "lock" : "key"} /></div><p class="small muted">No connections yet</p></div>
          ${(isLogin || canAdd) && html`<button class="btn primary sm" onClick=${add}><${Icon} n="add" />${isLogin ? "Add Connection" : "Add API Key"}</button>`}</div>`
      : html`
        <label class="selall"><input type="checkbox" checked=${allSel} onChange=${() => setSel(allSel ? {} : Object.fromEntries(accts.map((a) => [a.id, true])))} />Select All</label>
        ${accts.map((a, i) => html`<div class="crow ${a.active ? "" : "off"}" key=${a.id}>
          <input type="checkbox" checked=${!!sel[a.id]} onChange=${() => setSel({ ...sel, [a.id]: !sel[a.id] })} />
          <div class="arrows"><button disabled=${i === 0} title="Move up" onClick=${() => moveTo(i, i - 1)}><${Icon} n="keyboard_arrow_up" /></button>
            <button disabled=${i === accts.length - 1} title="Move down" onClick=${() => moveTo(i, i + 1)}><${Icon} n="keyboard_arrow_down" /></button></div>
          <${Icon} n=${a.authType === "oauth" || isLogin ? "lock" : "key"} cls="muted" />
          <div class="grow" style="min-width:0">
            <div class="medium truncate">${a.email || a.name}</div>
            ${a.email && a.name && a.email !== a.name && html`<div class="small muted truncate">${a.name}</div>`}
            <div class="row wrap" style="gap:6px;margin-top:5px">${statusPill(a)}
              <${Badge}>${a.authType === "oauth" || isLogin ? "OAuth" : "API Key"}</${Badge}>
              <span class="small muted">#${i + 1}</span>
              ${a.region && html`<${Badge} kind="info">${a.region}</${Badge}>`}
              ${testPill(a)}
            </div>
            ${tests[a.id] && !tests[a.id].ok && !tests[a.id].running ? html`<${ErrText} text=${tests[a.id].message} />`
              : a.lastError && html`<${ErrText} text=${(a.status === "banned" ? "Banned until you sign in again or a manual test passes. " : "") + a.lastError} />`}
          </div>
          <div class="row" style="gap:2px">
            ${isLogin && html`<button class="act" onClick=${() => setModal({ t: "signin", conn: a.id })}><${Icon} n="login" /><span>Re-auth</span></button>`}
            <button class="act" onClick=${() => testOne(a)} disabled=${tests[a.id] && tests[a.id].running}><${Icon} n="science" /><span>Test</span></button>
            <button class="act" onClick=${() => setModal({ t: "edit", a })}><${Icon} n="edit" /><span>Edit</span></button>
            <button class="act danger" onClick=${() => setModal({ t: "del", a })}><${Icon} n="delete" /><span>Delete</span></button>
          </div>
          <${Toggle} sm on=${a.active} title=${a.active ? "Disable connection" : "Enable connection"} onChange=${(v) => patch(a, { active: v }, v ? "Connection enabled" : "Connection disabled")} />
        </div>`)}
        ${(isLogin || canAdd) && html`<div style="margin-top:14px"><button class="btn primary sm" onClick=${add}><${Icon} n="add" />Add</button></div>`}`}
    </div>
    <${ModelsCard} data=${data} p=${p} reload=${reload} firstAccount=${(accts.find((a) => a.active) || {}).id} />

    ${modal && modal.t === "add" && html`<${AccountForm} data=${data} provider=${p} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "edit" && html`<${AccountForm} data=${data} account=${modal.a} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "node" && html`<${NodeForm} node=${p} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "risk" && html`<${Confirm} title="Risk Notice" text=${RISK_NOTICE} danger label="I Understand, Continue" onClose=${() => setModal(null)}
      onYes=${async () => { setTimeout(() => setModal({ t: "signin", conn: "" }), 0); }} />`}
    ${modal && modal.t === "signin" && html`<${SignIn} provider=${p} conn=${modal.conn} onClose=${() => setModal(null)} onSaved=${reload} />`}
    ${modal && modal.t === "del" && html`<${Confirm} title="Delete connection?" text=${`"${modal.a.email || modal.a.name}" and its stored credentials will be removed. This can't be undone.`} onClose=${() => setModal(null)}
      onYes=${() => run(async () => { await api("DELETE", `/api/admin/accounts/${modal.a.id}`); await reload(); }, "Connection deleted")} />`}
    ${modal && modal.t === "delsel" && html`<${Confirm} title="Delete selected connections?" text=${`${selected.length} connection(s) and their stored credentials will be removed.`} onClose=${() => setModal(null)}
      onYes=${() => run(async () => { for (const a of selected) await api("DELETE", `/api/admin/accounts/${a.id}`); setSel({}); await reload(); }, "Connections deleted")} />`}
    ${modal && modal.t === "delnode" && html`<${Confirm} title="Delete endpoint?" text=${`"${p.name}" and its ${p.accounts.length} account(s) will be removed. Combos that use ${p.prefix}/ models will skip them.`} onClose=${() => setModal(null)}
      onYes=${() => run(async () => { await api("DELETE", `/api/admin/nodes/${encodeURIComponent(p.id)}`); await reload(); go("providers"); }, "Endpoint deleted")} />`}`;
}

// Full error text: wraps, long ones collapse to 3 lines with a "more" toggle.
function ErrText({ text }) {
  const [open, setOpen] = useState(false);
  const long = text.length > 220;
  return html`<div class="err-text small"><div class=${long && !open ? "clamp" : ""} title=${text}>${text}</div>
    ${long && html`<button class="linkbtn" onClick=${() => setOpen(!open)}>${open ? "Show less" : "Show more"}</button>`}</div>`;
}

// Capability icons for a model id (best effort from the name, like 9Router's badges).
function capsOf(id) {
  const l = id.toLowerCase(), out = [];
  if (/(thinking|high|medium|low|reason|opus|pro|r1|-max|codex|o[134])/.test(l)) out.push(["psychology", "Reasoning"]);
  if (/(gemini|gpt-[456]|claude|vl|vision|omni|4o|kimi-k[23]|grok-4)/.test(l)) out.push(["visibility", "Vision"]);
  if (!/(embed|image|veo|tts)/.test(l)) out.push(["build", "Tools"]);
  return out;
}

function ModelsCard({ data, p, reload, firstAccount }) {
  const [info, setInfo] = useState(null);
  const [copied, copy] = useCopy();
  const [tests, setTests] = useState({});
  const load = () => api("GET", `/api/admin/providers/${encodeURIComponent(p.id)}/models`).then(setInfo, () => setInfo({ models: [], prefix: p.prefix, thinking: "auto" }));
  useEffect(() => { load(); }, [p.id]);
  if (!info) return html`<div class="card pd-models"><h2 class="pd-h2">Available Models</h2><p class="small muted" style="margin-top:8px">Loading…</p></div>`;
  const on = info.models.filter((m) => !m.disabled), off = info.models.filter((m) => m.disabled);
  const setDisabled = (ids, ok) => run(async () => { await api("PATCH", "/api/admin/settings", { disabledModels: { [p.id]: ids } }); await load(); await reload(); }, ok);
  const disable = (m) => setDisabled([...off.map((x) => x.id), m.id], "Model disabled");
  const enable = (m) => setDisabled(off.filter((x) => x.id !== m.id).map((x) => x.id), "Model enabled");
  const setThinking = (mode) => run(async () => { await api("PATCH", "/api/admin/settings", { providerThinking: { [p.id]: mode } }); await load(); await reload(); }, "Thinking updated");
  const test = async (m) => {
    setTests((t) => ({ ...t, [m.id]: "run" }));
    try {
      const r = await api("POST", "/api/admin/providers/test", { mode: "chat", provider: p.id, account: firstAccount || "", model: m.id });
      const x = r.results[0];
      setTests((t) => ({ ...t, [m.id]: x && x.ok ? "ok" : "error" }));
      if (!x) toast("No active account to test with", "error"); else if (!x.ok) toast(`${m.id}: ${x.message}`, "error");
    } catch (e) { setTests((t) => ({ ...t, [m.id]: "error" })); toast(e.message, "error"); }
  };
  const chip = (m) => {
    const full = `${info.prefix}/${m.id}`, st = tests[m.id];
    return html`<div class="mchip ${st === "ok" ? "ok" : st === "error" ? "bad" : ""}" key=${m.id}>
      <${Icon} n=${st === "ok" ? "check_circle" : st === "error" ? "cancel" : "smart_toy"} cls="mchip-ic" />
      <div class="grow" style="min-width:0"><code class="mchip-id" title=${m.upstream ? `upstream: ${m.upstream}` : full}>${full}</code>
        <div class="mchip-sub"><i class="truncate">${m.name}</i>${capsOf(m.id).map(([n, t]) => html`<span title=${t}><${Icon} n=${n} /></span>`)}${m.custom && html`<span class="tagc">custom</span>`}</div></div>
      <div class="mchip-act">
        <button title="Test" disabled=${st === "run" || !firstAccount} onClick=${() => test(m)}><${Icon} n=${st === "run" ? "progress_activity" : "science"} cls=${st === "run" ? "spin" : ""} /></button>
        <button title="Copy" onClick=${() => copy(full, m.id)}><${Icon} n=${copied === m.id ? "check" : "content_copy"} /></button>
        <button title="Disable this model" class="x" onClick=${() => disable(m)}><${Icon} n="close" /></button>
      </div></div>`;
  };
  return html`<div class="card pd-models">
    <div class="row between wrap" style="gap:12px;margin-bottom:14px">
      <h2 class="pd-h2">Available Models <span class="muted small" style="font-weight:400">${on.length}</span></h2>
      <div class="row" style="gap:10px">
        <label class="row small muted" style="gap:6px"><${Icon} n="psychology" />Thinking:
          <select class="input select-sm" value=${info.thinking || "auto"} onChange=${(e) => setThinking(e.target.value)}>${THINK.map(([v, l]) => html`<option value=${v}>${l}</option>`)}</select></label>
        ${on.length > 0 && html`<button class="btn outline sm" onClick=${() => setDisabled(info.models.map((m) => m.id), "All models disabled")}><${Icon} n="visibility_off" />Disable All</button>`}
        ${off.length > 0 && html`<button class="btn ghost sm" onClick=${() => setDisabled([], "All models enabled")}><${Icon} n="restart_alt" />Enable All</button>`}
      </div></div>
    ${info.models.length === 0 ? html`<p class="small muted">No model list for this provider. Any model id works as <span class="mono">${info.prefix}/model-id</span>.</p>`
      : html`<div class="mgrid">${on.map(chip)}</div>`}
    ${off.length > 0 && html`<details class="moff"><summary class="small muted">${off.length} disabled model${off.length === 1 ? "" : "s"}</summary>
      <div class="row wrap" style="gap:6px;margin-top:8px">${off.map((m) => html`<button class="chip" title="Enable" onClick=${() => enable(m)}><span class="mono">${m.id}</span><${Icon} n="add" /></button>`)}</div></details>`}
    ${!firstAccount && info.models.length > 0 && html`<p class="small muted" style="margin-top:10px">Add an active connection to test models.</p>`}
  </div>`;
}

function SignIn({ provider, conn, onClose, onSaved }) {
  const [flow, setFlow] = useState(null);
  const [err, setErr] = useState("");
  const [paste, setPaste] = useState("");
  const [busy, setBusy] = useState(false);
  const [copied, copy] = useCopy();
  useEffect(() => {
    let stop = false, t;
    api("POST", `/api/login/${provider.id}/start`, { connectionId: conn || "" }).then((f) => {
      setFlow(f);
      if (f.kind === "paste") return;
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
  const submit = async () => {
    setBusy(true);
    try {
      const g = await api("POST", `/api/login/flows/${flow.id}/callback`, flow.kind === "paste" ? { text: paste } : { url: paste });
      setFlow(g); setPaste(""); if (g.status === "done") { toast("Signed in"); await onSaved(); }
    } catch (e) { toast(e.message, "error"); }
    setBusy(false);
  };
  const name = provMeta(provider).name;
  const importApp = async () => {
    setBusy(true);
    try {
      const g = await api("POST", `/api/login/flows/${flow.id}/callback`, { text: "import" });
      setFlow(g); if (g.status === "done") { toast("Signed in"); await onSaved(); }
    } catch (e) { toast(e.message, "error"); }
    setBusy(false);
  };
  const isCursor = provider.id === "cursor";
  const pasteHint = isCursor ? "Or paste the access token (cursorAuth/accessToken) and machine id (storage.serviceMachineId) from Cursor's state.vscdb, one per line" : provider.id === "vertex" ? "Paste the service-account JSON key file" : provider.id === "claude" ? "Paste the code Claude shows (code#state), or the localhost address" : "Paste the address your browser ended on (http://localhost…/callback?code=…)";
  return html`<${Modal} title=${`Connect ${name}`} icon="login" onClose=${onClose} foot=${html`<button class="btn ghost" onClick=${onClose}>${flow && flow.status === "done" ? "Done" : "Close"}</button>`}>
    ${err && html`<div class="notice err"><${Icon} n="error" />${err}</div>`}
    ${!flow && !err && html`<p class="muted row" style="gap:8px"><${Icon} n="progress_activity" cls="spin" />Starting sign-in…</p>`}
    ${flow && flow.status === "done" && html`<div class="notice ok"><${Icon} n="check_circle" />Connected. The account is ready to use.</div>`}
    ${flow && flow.status === "error" && html`<div class="notice err"><${Icon} n="error" />${flow.message}</div>`}
    ${flow && flow.status === "pending" && flow.kind === "device" && html`<div class="col" style="gap:14px">
      <ol class="steps"><li>Open the ${name} page below and sign in.</li><li>${flow.userCode ? "Enter or confirm this code:" : "Approve access for FastRouter."}</li><li>Come back here — this window updates by itself.</li></ol>
      ${flow.userCode && html`<div class="devcode"><span class="mono">${flow.userCode}</span><button class="icon-btn" title="Copy code" onClick=${() => copy(flow.userCode, "uc")}><${Icon} n=${copied === "uc" ? "check" : "content_copy"} /></button></div>`}
      <a class="btn primary" href=${flow.url} target="_blank" rel="noopener noreferrer"><${Icon} n="open_in_new" />Open ${name} sign-in</a>
      <p class="small muted row" style="gap:6px"><${Icon} n="sync" cls="spin" style="font-size:16px" />Waiting for you to approve…</p></div>`}
    ${flow && flow.status === "pending" && flow.kind === "browser" && html`<div class="col" style="gap:14px">
      <ol class="steps"><li>Open the official ${name} sign-in and approve access.</li><li>You are sent back to FastRouter and this window finishes by itself.</li><li>If the browser can't reach this machine (remote dashboard), paste the final address below.</li></ol>
      <a class="btn primary" href=${flow.url} target="_blank" rel="noopener noreferrer"><${Icon} n="open_in_new" />Open sign-in page</a>
      ${flow.hint && html`<p class="small muted">${flow.hint}</p>`}
      <${Field} label="Paste callback (optional)"><div class="row"><input class="input mono grow" value=${paste} placeholder=${pasteHint} onInput=${(e) => setPaste(e.target.value)} />
        <button class="btn secondary" disabled=${!paste || busy} onClick=${submit}>Submit</button></div></${Field}>
      <p class="small muted row" style="gap:6px"><${Icon} n="sync" cls="spin" style="font-size:16px" />Waiting for sign-in…</p></div>`}
    ${flow && flow.status === "pending" && flow.kind === "paste" && isCursor && html`<div class="col" style="gap:10px;margin-bottom:12px">
      <p class="small">Cursor has no web sign-in for other apps. FastRouter imports the session of the Cursor IDE signed in on this PC (like 9Router's auto-import) and re-reads it when it nears expiry.</p>
      <button class="btn primary" disabled=${busy} onClick=${importApp}><${Icon} n="download" />${busy ? "Importing…" : "Import from Cursor on this PC"}</button></div>`}
    ${flow && flow.status === "pending" && flow.kind === "paste" && html`<div class="col" style="gap:12px">
      <p class="small">${pasteHint}. ${isCursor ? "It is stored encrypted and never shown again." : "It is stored encrypted and never shown again; FastRouter mints short-lived access tokens from it."}</p>
      <textarea class="input mono" rows=${isCursor ? 4 : 7} spellcheck="false" value=${paste} placeholder=${isCursor ? "eyJhbGciOi…\nd6eec8e1-…" : '{"type": "service_account", ...}'} onInput=${(e) => setPaste(e.target.value)}></textarea>
      <button class="btn primary" disabled=${!paste || busy} onClick=${submit}><${Icon} n="upload_file" />${busy ? "Checking…" : "Connect"}</button></div>`}
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
        <div class="topo-legend">${u && Object.keys(u.active || {}).length ? html`<span><span class="live-dot"></span>live</span>` : ""}<span><i class="l-last"></i>last request</span><span><i class="l-live"></i>in flight</span><span><i class="l-err"></i>failed</span></div></div>
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

// SVG topology after 9Router's ProviderTopology: FastRouter in the middle, every
// provider as a pill (logo + full name) evenly spaced on an ellipse that fills the
// card. The ellipse grows until no two pills overlap; the view then fits it.
// Edges end at the pill borders: orange = last request, red = its latest call
// failed, dashed and moving = in flight.
let measureCtx;
function textW(s, font) {
  measureCtx = measureCtx || document.createElement("canvas").getContext("2d");
  measureCtx.font = font;
  return measureCtx.measureText(s).width;
}
const NAME_FONT = '600 12.5px Inter, ui-sans-serif, system-ui, sans-serif', SUB_FONT = '400 10.5px Inter, ui-sans-serif, system-ui, sans-serif';
// Point where the ray from a box's centre towards (dx, dy) leaves the box.
function boxEdge(hw, hh, dx, dy) {
  const t = Math.min(dx ? hw / Math.abs(dx) : Infinity, dy ? hh / Math.abs(dy) : Infinity);
  return [dx * t, dy * t];
}
function topoLayout(items, W, H, hub) {
  const n = items.length, gap = 14;
  if (!n) return { pos: [], box: [-W / 2, -H / 2, W, H] };
  const maxHW = Math.max(...items.map((it) => it.w / 2)), maxHH = Math.max(...items.map((it) => it.h / 2));
  let rx = Math.max(hub.w / 2 + maxHW + 60, W / 2 - maxHW - 24), ry = Math.max(hub.h / 2 + maxHH + 50, H / 2 - maxHH - 22);
  const sep = (a, pa, b, pb) => Math.abs(pa[0] - pb[0]) >= (a.w + b.w) / 2 + gap || Math.abs(pa[1] - pb[1]) >= (a.h + b.h) / 2 + gap;
  const at = (t) => [rx * Math.cos(t), ry * Math.sin(t)];
  const hubOK = (it, q) => Math.abs(q[0]) >= it.w / 2 + hub.w / 2 + gap || Math.abs(q[1]) >= it.h / 2 + hub.h / 2 + gap;
  // Walk round the ellipse giving each pill the smallest angle that clears its
  // neighbour, then share the spare angle out evenly. Grow the ellipse if a lap
  // isn't enough.
  let pos = [];
  for (let k = 0; k < 80; k++) {
    const th = [-Math.PI / 2];
    for (let i = 1; i < n; i++) {
      let t = th[i - 1] + 0.01;
      while (t < th[0] + 2 * Math.PI && !sep(items[i - 1], at(th[i - 1]), items[i], at(t))) t += 0.005;
      th.push(t);
    }
    const need = n > 1 ? th[n - 1] - th[0] : 0;
    // the wrap-around gap between the last and first pill needs room too
    let wrap = 0.01;
    if (n > 1) while (wrap < 2 * Math.PI && !sep(items[n - 1], at(th[0] + 2 * Math.PI - wrap), items[0], at(th[0]))) wrap += 0.005;
    const used = need + (n > 1 ? wrap : 0);
    if (used <= 2 * Math.PI) {
      const spare = (2 * Math.PI - used) / Math.max(n, 1);
      const gaps = th.map((t, i) => (i ? t - th[i - 1] : 0));
      let t = -Math.PI / 2;
      pos = items.map((it, i) => { if (i) t += gaps[i] + spare; return at(t); });
      if (pos.every((q, i) => hubOK(items[i], q))) break;
    }
    rx *= 1.04; ry *= 1.04;
  }
  let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
  pos.forEach(([x, y], i) => { x0 = Math.min(x0, x - items[i].w / 2); x1 = Math.max(x1, x + items[i].w / 2); y0 = Math.min(y0, y - items[i].h / 2); y1 = Math.max(y1, y + items[i].h / 2); });
  // Fit: pad, keep the card's aspect, never zoom in past 1:1 (text stays its real size).
  let bw = Math.max(x1 - x0 + 40, W), bh = Math.max(y1 - y0 + 40, H);
  if (bw / bh > W / H) bh = (bw * H) / W; else bw = (bh * W) / H;
  const cx = (x0 + x1) / 2, cy = (y0 + y1) / 2;
  return { pos, rx, ry, box: [cx - bw / 2, cy - bh / 2, bw, bh] };
}
function Topology({ data, u }) {
  const H = 460;
  const wrap = useRef(null);
  const [W, setW] = useState(900);
  const [, setFontTick] = useState(0);
  const [view, setView] = useState({ k: 1, x: 0, y: 0 });
  const [drag, setDrag] = useState(null);
  useEffect(() => {
    const el = wrap.current; if (!el) return;
    const ro = new ResizeObserver(() => setW(Math.max(420, Math.round(el.clientWidth))));
    ro.observe(el);
    if (document.fonts) document.fonts.ready.then(() => setFontTick((t) => t + 1));
    return () => ro.disconnect();
  }, []);
  const rows = (u && u.providers) || [];
  const stats = Object.fromEntries(rows.map((r) => [r.key, r]));
  const recent = (u && u.recent) || [];
  const last = recent[0];
  const active = (u && u.active) || {};
  const failed = new Set(), seen = new Set();
  for (const r of recent) { if (seen.has(r.provider)) continue; seen.add(r.provider); if (r.status >= 400 || r.status === 0) failed.add(r.provider); }
  const nodes = data.providers.filter((p) => p.accounts.length > 0 || stats[p.id]);
  const liveN = nodes.reduce((a, p) => a + (active[p.id] || 0), 0);
  const hub = { w: liveN ? 172 : 150, h: 46 };
  const items = nodes.map((p) => {
    const m = provMeta(p), st = stats[p.id];
    const sub = st ? `${fmt(st.requests)} req · ${fmt(st.input_tokens + st.output_tokens)} tok` : "";
    const live = active[p.id] > 0;
    const w = Math.ceil(Math.max(textW(m.name, NAME_FONT), sub ? textW(sub, SUB_FONT) : 0)) + 58 + (live ? 14 : 0);
    const state = live ? "live" : failed.has(p.id) ? "err" : last && last.provider === p.id ? "last" : st ? "used" : "idle";
    return { p, m, st, sub, live, w, h: sub ? 46 : 36, state };
  });
  const L = topoLayout(items, W, H, hub);
  const [bx, by, bw, bh] = L.box;
  const vw = bw / view.k, vh = bh / view.k;
  const vb = `${bx + (bw - vw) / 2 - view.x} ${by + (bh - vh) / 2 - view.y} ${vw} ${vh}`;
  const zoom = (f) => setView((v) => ({ ...v, k: Math.min(4, Math.max(0.3, v.k * f)) }));
  const fit = () => setView({ k: 1, x: 0, y: 0 });
  const down = (e) => { if (e.button !== 0) return; setDrag({ sx: e.clientX, sy: e.clientY, x: view.x, y: view.y, f: vw / e.currentTarget.getBoundingClientRect().width }); };
  const moveP = (e) => { if (drag) setView((v) => ({ ...v, x: drag.x + (e.clientX - drag.sx) * drag.f, y: drag.y + (e.clientY - drag.sy) * drag.f })); };
  // Draw idle edges first so highlighted ones sit on top.
  const order = { idle: 0, used: 1, last: 2, err: 3, live: 4 };
  const edges = items.map((it, i) => ({ it, i })).sort((a, b) => order[a.it.state] - order[b.it.state]);
  return html`<div class="topo-wrap" ref=${wrap}>
    <svg viewBox=${vb} style=${`height:${H}px`} class="topo-svg ${drag ? "grabbing" : ""}" preserveAspectRatio="xMidYMid meet"
      onPointerDown=${down} onPointerMove=${moveP} onPointerUp=${() => setDrag(null)} onPointerLeave=${() => setDrag(null)}
      onWheel=${(e) => { e.preventDefault(); zoom(e.deltaY < 0 ? 1.1 : 1 / 1.1); }}>
      <defs><linearGradient id="hubg" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#fb923c" /><stop offset="1" stop-color="#ea580c" /></linearGradient></defs>
      ${items.length > 0 && html`<ellipse cx="0" cy="0" rx=${L.rx} ry=${L.ry} class="topo-ring" />`}
      ${edges.map(({ it, i }) => {
        const [x, y] = L.pos[i];
        const [sx, sy] = boxEdge(hub.w / 2 + 3, hub.h / 2 + 3, x, y);
        const [ex, ey] = boxEdge(it.w / 2 + 3, it.h / 2 + 3, -x, -y);
        const d = `M${sx.toFixed(1)},${sy.toFixed(1)} L${(x + ex).toFixed(1)},${(y + ey).toFixed(1)}`;
        return html`<g key=${"e" + it.p.id} class="topo-edge ${it.state}"><path d=${d} />
          ${it.live && html`<circle r="3.5" class="topo-spark"><animateMotion dur="1.1s" repeatCount="indefinite" path=${d} /></circle>`}</g>`;
      })}
      ${items.map((it, i) => {
        const [x, y] = L.pos[i];
        const { p, m, st } = it;
        const tip = `${m.name} (${p.prefix}/)` + (st ? ` · ${st.requests} requests · ${fmt(st.input_tokens)} in / ${fmt(st.output_tokens)} out` + (st.failures ? ` · ${st.failures} failed` : "") : " · no requests in this period")
          + (it.live ? ` · ${active[p.id]} in flight` : "") + (it.state === "err" ? " · latest call failed" : it.state === "last" ? " · last request" : "");
        const tx = -it.w / 2 + 44;
        return html`<g key=${"n" + p.id} class="topo-pnode ${it.state}" transform="translate(${x.toFixed(1)} ${y.toFixed(1)})" style=${`--pc:${m.color}`}>
          <title>${tip}</title>
          <rect x=${-it.w / 2} y=${-it.h / 2} width=${it.w} height=${it.h} rx="10" />
          <rect x=${-it.w / 2 + 9} y="-13" width="26" height="26" rx="7" fill=${m.color + "1f"} stroke="none" class="ic" />
          <image href=${m.logo} x=${-it.w / 2 + 12} y="-10" width="20" height="20" />
          <text x=${tx} y=${it.sub ? -3 : 4.5} class="nm">${m.name}</text>
          ${it.sub && html`<text x=${tx} y="12" class="sub">${it.sub}</text>`}
          ${it.live && html`<circle cx=${it.w / 2 - 12} cy="0" r="4" class="topo-live" />`}</g>`;
      })}
      <g class="topo-hub ${liveN ? "on" : ""}">
        <rect x=${-hub.w / 2} y=${-hub.h / 2} width=${hub.w} height=${hub.h} rx="12" fill="url(#hubg)" />
        <path d="M13 2L3 14h7l-1 8 10-12h-7z" transform=${`translate(${-hub.w / 2 + 14} -12)`} fill="#fff" />
        <text x=${-hub.w / 2 + 40} y="5" class="hub-t">FastRouter</text>
        ${liveN > 0 && html`<rect x=${hub.w / 2 - 34} y="-10" width="22" height="20" rx="10" fill="#fde047" /><text x=${hub.w / 2 - 23} y="4" text-anchor="middle" class="hub-n">${liveN}</text>`}
      </g>
      ${nodes.length === 0 && html`<text x="0" y="60" text-anchor="middle" class="hub-s">Add a provider account to see it here</text>`}
    </svg>
    <div class="topo-controls" role="group" aria-label="Zoom">
      <button title="Zoom in" aria-label="Zoom in" onClick=${() => zoom(1.25)}><${Icon} n="add" /></button>
      <button title="Zoom out" aria-label="Zoom out" onClick=${() => zoom(1 / 1.25)}><${Icon} n="remove" /></button>
      <button title="Fit view" aria-label="Fit view" onClick=${fit}><${Icon} n="fit_screen" /></button></div>
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
// Quota Tracker, after 9Router's ProviderLimits: per-account cards with each
// quota window's remaining share and reset countdown, read from the provider's
// own usage API (Codex, GLM, DeepSeek, OpenCode Go, OpenRouter).
const qColor = (pct) => (pct < 30 ? "red" : pct < 70 ? "yellow" : "green");
function countdown(iso, now) {
  if (!iso) return "";
  let s = Math.max(0, Math.round((Date.parse(iso) - now) / 1000));
  const d = Math.floor(s / 86400); s -= d * 86400;
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60);
  return d > 0 ? `${d}d ${h}h` : h > 0 ? `${h}h ${m}m` : `${m}m`;
}
const qMoney = (v, unit) => (unit === "USD" ? "$" : unit === "CNY" ? "¥" : "") + (+v).toFixed(2) + (unit === "USD" || unit === "CNY" ? "" : " " + unit);
const qRemaining = (a) => { const r = a.quotas.filter((x) => x.remainingPct >= 0); return r.length ? Math.min(...r.map((x) => x.remainingPct)) : -1; };
const qSoonest = (a) => Math.min(Infinity, ...a.quotas.filter((x) => x.resetAt).map((x) => Date.parse(x.resetAt)));
function QuotaRow({ q, now, hidden, onHide }) {
  const balance = q.remainingPct < 0;
  const pct = balance ? 100 : Math.round(q.remainingPct);
  const c = balance ? "neutral" : qColor(pct);
  return html`<div class="qrow ${hidden ? "hid" : ""}">
    <div class="qrow-top"><span class="qdot ${c}"></span><span class="medium grow truncate">${q.name}</span>
      <span class="small muted">${balance ? (q.limit > 0 ? `${qMoney(q.used, q.unit)} / ${qMoney(q.limit, q.unit)}` : q.unit === "%" ? "" : `${qMoney(q.used, q.unit)} ${q.name.startsWith("Balance") ? "left" : "used"}`)
        : q.unit === "%" ? `${Math.round(q.used)}% / 100% used` : `${qMoney(q.used, q.unit)} / ${qMoney(q.limit, q.unit)}`}</span>
      <button class="icon-btn xs" title=${hidden ? "Show this quota" : "Hide this quota"} onClick=${onHide}><${Icon} n=${hidden ? "visibility_off" : "visibility"} /></button></div>
    ${!balance && html`<div class="qbar ${c}"><i style=${`width:${Math.min(100, pct)}%`}></i></div>
      <div class="qrow-foot"><span class="qpct ${c}">${pct}% remaining</span>${q.resetAt && html`<span class="small muted"><${Icon} n="schedule" />in ${countdown(q.resetAt, now)}</span>`}</div>`}
  </div>`;
}
function QuotaCard({ a, p, now, hidden, toggleHide, busy, onRefresh, onEdit, onDelete, onToggle }) {
  const shown = a.quotas.filter((q) => !hidden[a.id + "|" + q.name]);
  const hiddenCount = a.quotas.length - shown.length;
  const [showHidden, setShowHidden] = useState(false);
  return html`<div class="card sm qcard" style=${a.active ? "" : "opacity:.6"}>
    <div class="qcard-head"><${Logo} p=${p} />
      <div class="grow" style="min-width:0"><div class="row" style="gap:6px"><span class="strong truncate">${provMeta(p).name}</span>${a.plan && html`<${Badge} kind="info">${a.plan}</${Badge}>`}</div>
        <div class="small muted truncate">${a.name}</div></div>
      <button class="icon-btn" title="Refresh this account" disabled=${busy || !a.supported} onClick=${onRefresh}><${Icon} n="refresh" cls=${busy ? "spin" : ""} /></button>
      <button class="icon-btn" title="Edit account" onClick=${onEdit}><${Icon} n="edit" /></button>
      <button class="icon-btn danger" title="Delete account" onClick=${onDelete}><${Icon} n="delete" /></button>
      <${Toggle} sm on=${a.active} title=${a.active ? "Turn this account off" : "Turn this account on"} onChange=${onToggle} /></div>
    ${a.supported && a.quotas.length > 0 && html`<div class="qcount"><span>${a.quotas.length} quota${a.quotas.length === 1 ? "" : "s"}</span>
      ${hiddenCount > 0 && html`<button class="link small" onClick=${() => setShowHidden(!showHidden)}>${showHidden ? "Hide" : "Show"} ${hiddenCount} hidden</button>`}</div>`}
    ${a.error && html`<div class="qmsg err"><${Icon} n="error" />${a.error}</div>`}
    ${!a.supported && html`<div class="qmsg"><${Icon} n="info" />${a.note}</div>`}
    <div class="col" style="gap:12px">${(showHidden ? a.quotas : shown).map((q) => html`<${QuotaRow} key=${q.name} q=${q} now=${now} hidden=${!!hidden[a.id + "|" + q.name]} onHide=${() => toggleHide(a.id + "|" + q.name)} />`)}</div>
  </div>`;
}
function QuotaPage({ data, reload, go }) {
  const [res, setRes] = useState(null);
  const [busy, setBusy] = useState({});
  const [prov, setProv] = useState("all");
  const [acct, setAcct] = useState("all");
  const [expiring, setExpiring] = useState(false);
  const [auto, setAuto] = useState(() => localStorage.getItem("fr-quota-auto") === "1");
  const [left, setLeft] = useState(60);
  const [now, setNow] = useState(Date.now());
  const [hidden, setHidden] = useState(() => { try { return JSON.parse(localStorage.getItem("fr-quota-hidden") || "{}"); } catch (e) { return {}; } });
  const [confirm, setConfirm] = useState(null);
  const load = async (refresh, id) => {
    setBusy((b) => ({ ...b, [id || "*"]: true }));
    try {
      const r = await api("GET", `/api/admin/quota?${refresh ? "refresh=1&" : ""}${id ? "account=" + encodeURIComponent(id) : ""}`);
      setRes((old) => (id && old ? { ...old, accounts: old.accounts.map((a) => r.accounts.find((x) => x.id === a.id) || a) } : r));
    } catch (e) { toast(e.message, "error"); }
    setBusy((b) => ({ ...b, [id || "*"]: false })); setLeft(60);
  };
  useEffect(() => { load(false); }, []);
  useEffect(() => { const t = setInterval(() => { setNow(Date.now()); if (auto) setLeft((l) => l - 1); }, 1000); return () => clearInterval(t); }, [auto]);
  useEffect(() => { if (auto && left <= 0) load(true); }, [left, auto]);
  const toggleHide = (k) => { const h = { ...hidden }; if (h[k]) delete h[k]; else h[k] = true; setHidden(h); localStorage.setItem("fr-quota-hidden", JSON.stringify(h)); };
  const setAutoP = (v) => { setAuto(v); setLeft(60); localStorage.setItem("fr-quota-auto", v ? "1" : "0"); };
  const provOf = (a) => data.providers.find((p) => p.id === a.provider) || { id: a.provider, name: a.provider };
  if (!res) return html`<div class="skeleton"></div>`;
  const accts = res.accounts.filter((a) => (prov === "all" || a.provider === prov) && (acct === "all" || (acct === "active") === a.active));
  const list = [...accts].sort((a, b) => b.supported - a.supported || (expiring ? qSoonest(a) - qSoonest(b) : 0));
  const provIds = [...new Set(res.accounts.map((a) => a.provider))];
  const empty = res.accounts.filter((a) => a.active && qRemaining(a) === 0);
  const avail = res.accounts.filter((a) => !a.active && qRemaining(a) > 0);
  const setActive = (ids, on) => run(async () => { for (const id of ids) await api("PATCH", `/api/admin/accounts/${id}`, { active: on }); await reload(); await load(false); },
    `${ids.length} account${ids.length === 1 ? "" : "s"} turned ${on ? "on" : "off"}`);
  return html`
    ${res.demo && html`<div class="notice warn"><${Icon} n="science" />Demo data: these quota numbers are samples (FASTROUTER_DEMO_QUOTA=1), not your real usage.</div>`}
    <div class="qbar-tools">
      <select class="input select-sm" value=${prov} onChange=${(e) => setProv(e.target.value)} aria-label="Provider">
        <option value="all">All providers</option>${provIds.map((id) => html`<option value=${id}>${provMeta(provOf({ provider: id })).name}</option>`)}</select>
      <select class="input select-sm" value=${acct} onChange=${(e) => setAcct(e.target.value)} aria-label="Accounts">
        <option value="all">All accounts</option><option value="active">Active</option><option value="inactive">Turned off</option></select>
      <button class="btn outline sm ${expiring ? "on" : ""}" aria-pressed=${expiring} onClick=${() => setExpiring(!expiring)}><${Icon} n="hourglass_bottom" />Expiring first</button>
      <button class="btn outline sm" disabled=${!empty.length} title="Turn off active accounts whose quota is used up" onClick=${() => setConfirm({ on: false, ids: empty.map((a) => a.id) })}><${Icon} n="toggle_off" />Turn off Empty${empty.length ? ` (${empty.length})` : ""}</button>
      <button class="btn outline sm" disabled=${!avail.length} title="Turn on accounts that are off but have quota left" onClick=${() => setConfirm({ on: true, ids: avail.map((a) => a.id) })}><${Icon} n="toggle_on" />Turn on Available${avail.length ? ` (${avail.length})` : ""}</button>
      <span class="grow"></span>
      <label class="row small" style="gap:8px"><${Toggle} sm on=${auto} onChange=${setAutoP} />Auto-refresh${auto && html`<span class="muted mono">${left}s</span>`}</label>
      <button class="icon-btn" title="Refresh all" disabled=${busy["*"]} onClick=${() => load(true)}><${Icon} n="refresh" cls=${busy["*"] ? "spin" : ""} /></button></div>
    ${list.length === 0 ? html`<div class="card"><div class="empty"><div class="bubble"><${Icon} n="cloud_off" /></div><h3 class="strong">No accounts</h3><p class="small muted">Add a provider account to track its quota.</p></div></div>`
    : html`<div class="qgrid">${list.map((a) => html`<${QuotaCard} key=${a.id} a=${a} p=${provOf(a)} now=${now} hidden=${hidden} toggleHide=${toggleHide} busy=${busy[a.id] || busy["*"]}
        onRefresh=${() => load(true, a.id)} onEdit=${() => go("providers/" + encodeURIComponent(a.provider))}
        onDelete=${() => setConfirm({ del: a })} onToggle=${(v) => setActive([a.id], v)} />`)}</div>`}
    ${confirm && confirm.del && html`<${Confirm} title="Delete account?" text=${`Delete ${confirm.del.name} (${provMeta(provOf(confirm.del)).name})? This removes its saved credential.`}
      onClose=${() => setConfirm(null)} onYes=${() => run(async () => { await api("DELETE", `/api/admin/accounts/${confirm.del.id}`); await reload(); await load(false); }, "Account deleted")} />`}
    ${confirm && confirm.ids && html`<${Confirm} danger=${!confirm.on} label=${confirm.on ? "Turn on" : "Turn off"} title=${confirm.on ? "Turn on available accounts?" : "Turn off empty accounts?"}
      text=${`${confirm.ids.length} account${confirm.ids.length === 1 ? "" : "s"} will be turned ${confirm.on ? "on" : "off"}.`} onClose=${() => setConfirm(null)} onYes=${() => setActive(confirm.ids, confirm.on)} />`}`;
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
// Ported from 9Router's CLI Tools page: a grid of tools, each with its own setup
// page. Status comes from reading each tool's config (read-only); files change only
// when Apply / Reset is clicked, and the server backs every file up first.
// API keys never reach the browser: the page sends a key id and the server resolves it.
const KEY = "YOUR_FASTROUTER_KEY";
function cliPill(t) {
  const s = t.status || {};
  if (t.kind === "guide") return html`<${Badge} kind="info" icon="menu_book">Guide</${Badge}>`;
  if (!s.installed) return html`<${Badge} dot>Not installed</${Badge}>`;
  if (s.connected) return html`<${Badge} kind="success" dot>Connected</${Badge}>`;
  return html`<${Badge} kind="warning" dot>Not configured</${Badge}>`;
}
function ToolLogo({ t, lg }) {
  const [bad, setBad] = useState(false);
  return html`<div class="logo ${lg ? "lg" : ""}">${bad ? html`<div class="logo-fallback" style="background:var(--primary)">${t.name.slice(0, 2).toUpperCase()}</div>`
    : html`<img src=${"logos/" + t.logo} alt="" onError=${() => setBad(true)} />`}</div>`;
}
function useCliTools() {
  const [cli, setCli] = useState(null);
  const load = useCallback(() => api("GET", "/api/admin/cli-tools").then(setCli, (e) => setCli({ error: e.message })), []);
  useEffect(() => { load(); }, []);
  return [cli, load];
}
function CliPage({ data, go, query }) {
  const [cli, load] = useCliTools();
  if (!cli) return html`<div class="skeleton"></div><div class="skeleton"></div>`;
  if (cli.error) return html`<div class="notice err"><${Icon} n="error" />${cli.error}</div>`;
  const n = cli.tools.filter((t) => t.status && t.status.connected).length;
  return html`
    <div class="row between" style="margin-bottom:14px"><p class="muted small">Point your coding tools at FastRouter. Each tool's config is read to show its status; nothing is written until you click Apply on its page.</p>
      <div class="row" style="gap:8px"><${Badge} kind="success">${n} connected</${Badge}><button class="btn ghost sm" onClick=${load}><${Icon} n="refresh" />Refresh</button></div></div>
    <div class="tool-grid">${cli.tools.map((t) => html`<a class="card xs hover tool cli-card" href=${"#/cli/" + t.id} key=${t.id}>
      <${ToolLogo} t=${t} />
      <div class="grow" style="min-width:0"><div class="medium">${t.name}</div>${cliPill(t)}</div>
      <${Icon} n="chevron_right" cls="muted" /></a>`)}</div>`;
}
const fill = (s, v) => (s || "").replaceAll("{{baseUrl}}", v.baseUrl).replaceAll("{{apiKey}}", v.apiKey).replaceAll("{{model}}", v.model || "MODEL_ID");
function CopyRow({ value, id, copied, copy }) {
  return html`<div class="copy-row"><code class="mono">${value}</code><${CopyBtn} text=${value} id=${id} copied=${copied} copy=${copy} /></div>`;
}
function CliToolPage({ data, id, go }) {
  const [cli, load] = useCliTools();
  const [ep, setEp] = useState("");
  const [keyId, setKeyId] = useState(null);
  const [model, setModel] = useState(null);
  const [roles, setRoles] = useState(null);
  const [audioOn, setAudioOn] = useState(null);
  const [audioModel, setAudioModel] = useState(null);
  const [rolesOpen, setRolesOpen] = useState(null);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(null);
  const [confirm, setConfirm] = useState(false);
  const [copied, copy] = useCopy();
  const back = html`<div class="crumbs"><a href="#/cli"><${Icon} n="arrow_back" /> Back to CLI Tools</a></div>`;
  if (!cli) return html`${back}<div class="skeleton"></div>`;
  if (cli.error) return html`${back}<div class="notice err"><${Icon} n="error" />${cli.error}</div>`;
  const t = cli.tools.find((x) => x.id === id);
  if (!t) return html`${back}<div class="card empty"><p>Unknown tool.</p></div>`;
  const s = t.status || {};
  const endpoint = ep || cli.endpoints[0];
  const keys = cli.keys || [];
  const kid = keyId ?? (keys[0] ? keys[0].id : "");
  const mdl = model ?? (s.connected ? s.model || "" : "");
  const rl = roles ?? (s.connected ? { ...(s.roles || {}) } : {});
  const aOn = audioOn ?? !!s.audio;
  const aModel = audioModel ?? (s.audio || "");
  const showRoles = rolesOpen ?? !!t.rolesOpen;
  const reset = () => { setModel(null); setRoles(null); setAudioOn(null); setAudioModel(null); setKeyId(null); };
  const apply = async () => {
    setBusy(true);
    try {
      const body = { endpoint, keyId: kid, model: mdl, roles: rl };
      if (t.audio) body.audio = { enabled: aOn, model: aOn ? aModel : "" };
      const r = await run(() => api("POST", `/api/admin/cli-tools/${t.id}/apply`, body));
      toast(r.written.length ? `${t.name} now uses FastRouter` : "Already up to date");
      setDone(r); reset(); await load();
    } catch (e) {} finally { setBusy(false); }
  };
  const doReset = async () => {
    const r = await run(() => api("POST", `/api/admin/cli-tools/${t.id}/reset`, {}));
    toast(r.written.length ? `Removed FastRouter from ${t.name}` : "Nothing to reset");
    setDone(r); reset(); await load();
  };
  const vals = { baseUrl: endpoint + "/v1", apiKey: KEY, model: mdl };
  const keySelect = html`<select class="input mono" value=${kid} onChange=${(e) => setKeyId(e.target.value)}>
    ${keys.length === 0 && html`<option value="">No API keys (local clients allowed without one)</option>`}
    ${keys.map((k) => html`<option value=${k.id}>${k.name || "key"} · ${k.masked}</option>`)}</select>`;
  const head = html`<div class="row" style="gap:14px;margin-bottom:16px"><${ToolLogo} t=${t} lg />
    <div class="grow"><h2 style="margin:0">${t.name}</h2><p class="muted small" style="margin:2px 0 0">${t.description}</p></div>
    ${t.docs && html`<a class="btn ghost sm" href=${t.docs} target="_blank" rel="noopener noreferrer"><${Icon} n="open_in_new" />Docs</a>`}</div>`;
  const notes = (t.notes || []).map((n) => html`<div class="notice ${n.type === "warning" ? "warn" : n.type === "error" ? "err" : "info"}"><${Icon} n=${n.type === "info" ? "info" : "warning"} /><span>${n.text}</span></div>`);
  const statusCard = html`<div class="card">
    <div class="row between"><div class="row" style="gap:10px"><span class="medium">Status</span>${cliPill(t)}
      ${s.other && html`<${Badge} kind="warning" icon="warning">Points at ${s.other}</${Badge}>`}</div>
      <button class="icon-btn" title="Re-check" onClick=${load}><${Icon} n="refresh" /></button></div>
    ${s.configPath && html`<div class="small muted mono" style="margin-top:8px;word-break:break-all"><${Icon} n="folder" style="font-size:14px;vertical-align:-2px" /> ${s.configPath}</div>`}
    ${t.kind === "config" && !s.installed && html`<div class="notice warn" style="margin-top:10px"><${Icon} n="warning" /><span>${t.name} was not found on this computer. You can still apply the config now and install it later.</span></div>`}
    ${t.install && !s.installed && html`<div style="margin-top:10px"><div class="small muted" style="margin-bottom:4px">Install</div><${CopyRow} value=${t.install} id="inst" copied=${copied} copy=${copy} /></div>`}
    ${notes.length > 0 && html`<div class="col" style="gap:8px;margin-top:12px">${notes}</div>`}
  </div>`;
  if (t.kind === "guide") {
    let n = 0;
    return html`${back}${head}${statusCard}
      <div class="card"><div class="card-title"><div><h2><${Icon} n="menu_book" />Setup guide</h2><p>Follow these steps in ${t.name}.</p></div>
        <div class="field" style="min-width:240px;margin:0"><label>Endpoint</label><select class="input mono" value=${endpoint} onChange=${(e) => setEp(e.target.value)}>${cli.endpoints.map((u) => html`<option value=${u}>${u}</option>`)}</select></div></div>
        <ol class="steps">${(t.steps || []).map((st, i) => html`<li key=${i}><span class="step-n">${++n}</span><div class="grow">
          <div class="medium">${st.title}</div>${st.desc && html`<div class="small muted">${st.desc}</div>`}
          ${st.value && html`<${CopyRow} value=${fill(st.value, vals)} id=${"s" + i} copied=${copied} copy=${copy} />`}
          ${st.type === "apiKey" && html`<div class="small muted" style="margin:6px 0 4px">Use one of your FastRouter keys (shown once when created, see <a href="#/keys">API Keys</a>)${keys.length ? "" : "; with no keys any value works locally"}.</div>`}
          ${st.type === "model" && html`<div style="margin-top:6px"><${ModelSelect} data=${data} value=${mdl} onChange=${setModel} placeholder="Select a model" /></div>
            ${mdl && html`<${CopyRow} value=${mdl} id=${"m" + i} copied=${copied} copy=${copy} />`}`}
        </div></li>`)}</ol>
        ${t.code && html`<pre class="code" style="margin-top:12px">${fill(t.code, vals)}<${CopyBtn} text=${fill(t.code, vals)} id="code" copied=${copied} copy=${copy} /></pre>
          ${t.code.includes("{{apiKey}}") && html`<div class="small muted" style="margin-top:6px">Replace <code>${KEY}</code> with your key.</div>`}`}
      </div>`;
  }
  return html`${back}${head}${statusCard}
    <div class="card cli-form">
      <${Field} label="Select Endpoint"><select class="input mono" value=${endpoint} onChange=${(e) => setEp(e.target.value)}>${cli.endpoints.map((u) => html`<option value=${u}>${u}</option>`)}</select></${Field}>
      <div class="small muted cli-current">Current: ${s.baseUrl ? html`<code class="mono">${s.baseUrl}</code>` : "not set"}${s.model ? html` · model <code class="mono">${s.model}</code>` : ""}</div>
      <${Field} label="API Key" hint="The key is written into the tool's config by FastRouter; it is never sent to this page.">${keySelect}</${Field}>
      <${Field} label=${t.needsModel ? "Default Model" : "Default Model (optional)"}><${ModelSelect} data=${data} value=${mdl} onChange=${setModel} placeholder="Select a model" /></${Field}>
      ${t.audio && html`<div class="field"><label class="check"><input type="checkbox" checked=${aOn} onChange=${(e) => setAudioOn(e.target.checked)} /> Audio STT (speech-to-text through FastRouter)</label>
        ${aOn && html`<div style="margin-top:8px"><${ModelSelect} data=${data} value=${aModel} onChange=${setAudioModel} placeholder="Select a transcription model" /></div>`}</div>`}
      ${(t.roles || []).length > 0 && html`<div class="roles">
        <button type="button" class="roles-head" onClick=${() => setRolesOpen(!showRoles)}><${Icon} n=${showRoles ? "expand_less" : "expand_more"} /><span class="medium">${t.rolesTitle || "Model Roles (optional)"}</span>
          <span class="small muted">${Object.values(rl).filter(Boolean).length} set</span></button>
        ${showRoles && html`<div class="roles-body">${t.roles.map((r) => html`<div class="role-row" key=${r.id}><div class="small medium">${r.label}</div>
          <${ModelSelect} data=${data} value=${rl[r.id] || ""} onChange=${(v) => setRoles({ ...rl, [r.id]: v })} placeholder="inherit default" /></div>`)}</div>`}</div>`}
      <div class="row" style="justify-content:flex-end;gap:8px;margin-top:16px">
        <button class="btn outline" disabled=${busy || (!s.connected && !s.configPath)} onClick=${() => setConfirm(true)}><${Icon} n="restart_alt" />Reset</button>
        <button class="btn primary" disabled=${busy || (t.needsModel && !mdl) || (t.audio && aOn && !aModel)} onClick=${apply}><${Icon} n="check" />Apply</button></div>
      ${done && (done.written.length > 0 || done.backups.length > 0) && html`<div class="notice ok" style="margin-top:12px"><${Icon} n="backup" /><div>
        <div>Wrote ${done.written.map((f, i) => html`${i ? ", " : ""}<code class="mono">${f}</code>`)}</div>
        ${done.backups.length > 0 && html`<div>Originals backed up to ${done.backups.map((f, i) => html`${i ? ", " : ""}<code class="mono">${f}</code>`)}</div>`}
        <div class="small">Restart ${t.name} to pick up the change.</div></div></div>`}
    </div>
    ${confirm && html`<${Confirm} title=${"Reset " + t.name + "?"} text="Removes FastRouter's entries from this tool's config. The current file is backed up first." label="Reset" onYes=${doReset} onClose=${() => setConfirm(false)} />`}`;
}

/* ------------------------------------------------------------ settings */
// Token Saver, laid out like 9Router's. Headroom can be installed from here:
// nothing is installed until the Install button is clicked.
const CAVEMAN_LV = [["lite", "Lite", "Drop filler, keep grammar"], ["full", "Full", "Drop articles, fragments OK"], ["ultra", "Ultra", "Telegraphic, max compression"]];
const PONYTAIL_LV = [["lite", "Lite", "Build asked, name lazier option"], ["full", "Full", "Ladder enforced: stdlib/native first"], ["ultra", "Ultra", "YAGNI extremist, deletion first"]];
const EXTRA_TITLE = { code: "tree-sitter AST compression for code responses", ml: "Kompress-v2 HF model for prose/agentic traces (~+1GB)" };
function SegLv({ value, levels, onChange, disabled }) {
  return html`<div class="lv"><div class="lv-seg">${levels.map(([id, label, desc]) => html`<button type="button" title=${desc} class=${value === id ? "on" : ""} disabled=${disabled} onClick=${() => onChange(id)}>${label}</button>`)}</div>
    <div class="lv-desc">${(levels.find((l) => l[0] === value) || levels[1])[2]}</div></div>`;
}
function HeadroomSetup({ s, hr, refresh, save, locked, onClose }) {
  const [url, setUrl] = useState(s.headroomUrl);
  const [act, setAct] = useState("");
  const call = async (path, body, ok) => { setAct(path); try { await run(() => api("POST", "/api/admin/headroom/" + path, body || {}), ok); } catch (e) {} setAct(""); refresh(true); };
  const installing = hr && hr.busy;
  return html`<${Modal} title=${hr && hr.running ? "Headroom" : "Setup Headroom"} icon="compress" onClose=${onClose}
    foot=${html`<button class="btn ghost" onClick=${() => refresh(true)}><${Icon} n="refresh" />Recheck</button><button class="btn primary" onClick=${onClose}>Done</button>`}>
    ${!hr ? html`<p class="small muted">Checking…</p>` : html`<div class="col" style="gap:14px">
      <div class="row" style="gap:8px"><span class="small muted">Status:</span><span class="chip ${hr.running ? "ok" : "warn"}">${installing ? "Installing…" : hr.running ? "Running" : !hr.localUrl ? "External" : hr.installed ? "Stopped" : "Not installed"}</span>
        ${hr.version && html`<span class="small muted">headroom-ai v${hr.version}${hr.managed ? " · FastRouter venv" : ""}</span>`}</div>
      <${Field} label="Headroom URL" hint="FastRouter sends context to <URL>/v1/compress before routing. Local URLs can be started from here.">
        <div class="row" style="gap:8px"><input class="input mono grow" value=${url} disabled=${locked("headroomUrl")} onInput=${(e) => setUrl(e.target.value)} />
          <button class="btn outline sm" disabled=${url === s.headroomUrl || locked("headroomUrl")} onClick=${() => save({ headroomUrl: url }, "URL saved")}>Save</button></div></${Field}>
      ${hr.running ? (hr.pid ? html`<button class="btn outline" disabled=${!!act} onClick=${() => call("stop", null, "Headroom stopped")}><${Icon} n="stop_circle" />${act === "stop" ? "Stopping…" : "Stop Headroom"}</button>`
          : html`<p class="small" style="color:var(--green-fg)">Headroom proxy is reachable. You can enable the token saver.</p>`)
        : hr.installed && hr.canStart ? html`<button class="btn primary" disabled=${!!act} onClick=${() => call("start", null, "Headroom started")}><${Icon} n="play_arrow" />${act === "start" ? "Starting…" : "Start Headroom"}</button>`
        : hr.installed ? html`<p class="small muted">Start Headroom yourself at the configured URL, then click Recheck.</p>`
        : !hr.python && !hr.commands.length ? html`<p class="small" style="color:var(--amber-fg)">Python 3.10 or newer is required. Install it from <a href="https://www.python.org/downloads/" target="_blank" rel="noreferrer">python.org</a> (tick “Add python.exe to PATH”), then click Recheck.</p>`
        : html`<div class="col" style="gap:8px"><p class="small medium">Install runs these commands (into FastRouter's own folder, not your system Python):</p>
            <pre class="code-box">${hr.commands.join("\n")}</pre>
            <button class="btn primary" disabled=${installing || !!act} onClick=${() => call("install", { extras: [] }, "Installing Headroom…")}><${Icon} n="download" />${installing ? "Installing… (a few minutes)" : "Install Headroom"}</button></div>`}
      ${(installing || hr.log) && html`<pre class="code-box log">${hr.log || "Starting…"}</pre>`}
      ${hr.error && html`<p class="small" style="color:var(--red-fg)">${hr.error}</p>`}
    </div>`}</${Modal}>`;
}
function TokenSaverPage({ data, reload }) {
  const s = data.settings;
  const locked = (k) => s.lockedByConfig.includes(k);
  const save = (body, ok = "Settings saved") => run(async () => { await api("PATCH", "/api/admin/settings", body); await reload(); }, ok);
  const [hr, setHr] = useState(null);
  const [setup, setSetup] = useState(false);
  const [pending, setPending] = useState([]);
  const [prompt, setPrompt] = useState(s.customPrompt || "");
  const refresh = useCallback(async (fresh) => { try { setHr(await api("GET", "/api/admin/headroom" + (fresh ? "?fresh=1" : ""))); } catch (e) {} }, []);
  useEffect(() => { refresh(false); }, []);
  useEffect(() => { if (!hr || !hr.busy) return; const t = setTimeout(() => refresh(true), 1500); return () => clearTimeout(t); }, [hr]);
  const hrLabel = !hr ? "Checking…" : hr.busy ? "Installing…" : hr.running ? "Running" : !hr.localUrl ? "External" : hr.installed ? "Stopped" : "Not installed";
  const extrasCall = (path, body, ok) => run(async () => { await api("POST", "/api/admin/headroom/" + path, body); setPending([]); await refresh(true); }, ok);
  const toggleExtra = async (extra, on) => {
    await save(extra === "code" ? { headroomCodeAware: on } : { headroomKompress: on }, `[${extra}] ${on ? "on" : "off"}`);
    if (hr && hr.running && hr.pid) await run(() => api("POST", "/api/admin/headroom/restart", {}), "Headroom restarted with the new setting");
  };
  const Lock = ({ k }) => locked(k) && html`<span class="lock" title="Set in the config file"><${Icon} n="lock" />config file</span>`;
  const link = (href, t) => html`<a class="ts-link" href=${href} target="_blank" rel="noreferrer">(${t})</a>`;
  return html`<div class="card ts-card">
    <h2 class="ts-title"><${Icon} n="bolt" />Token Saver</h2>
    <div class="ts-row">
      <div class="grow"><div class="ts-t">Compress tool output ${link("https://github.com/rtk-ai/rtk", "RTK")}<${Lock} k="rtkEnabled" /></div>
        <div class="ts-d">git/grep/ls/tree/logs → 60-90% fewer input tokens</div></div>
      <${Toggle} on=${s.rtkEnabled} disabled=${locked("rtkEnabled")} onChange=${(v) => save({ rtkEnabled: v }, v ? "RTK on" : "RTK off")} /></div>
    <div class="ts-row">
      <div class="grow"><div class="ts-t">Compress context ${link("https://github.com/chopratejas/headroom", "Headroom")}
          <span class="chip ${hr && hr.running ? "ok" : "warn"}">${hrLabel}</span>
          <button class="link small" onClick=${() => { setSetup(true); refresh(true); }}>${hr && hr.running ? "Manage" : "Setup"}</button><${Lock} k="headroomEnabled" /></div>
        <div class="ts-d">Compress prompts via /v1/compress before routing to the model</div>
        ${hr && hr.installed && html`<div class="ts-extras">
          <span class="small muted">Compression extras${hr.version ? ` · v${hr.version}` : ""}:</span>
          ${["code", "ml"].map((x) => hr.extras[x] ? html`<span class="xchip on" title=${EXTRA_TITLE[x]}>
              <${Toggle} sm on=${x === "code" ? s.headroomCodeAware : s.headroomKompress !== false} disabled=${hr.busy} onChange=${(v) => toggleExtra(x, v)} /><b>[${x}]</b>
              <button class="link small danger" disabled=${!!hr.busy} onClick=${() => extrasCall("uninstall", { extra: x }, `Uninstalling [${x}]…`)}>Uninstall</button></span>`
            : html`<label class="xchip" title=${EXTRA_TITLE[x]}><input type="checkbox" checked=${pending.includes(x)} disabled=${!!hr.busy}
                onChange=${(e) => setPending(e.target.checked ? [...pending, x] : pending.filter((y) => y !== x))} /><b>[${x}]</b></label>`)}
          ${pending.length > 0 && html`<button class="btn primary sm" disabled=${!!hr.busy} onClick=${() => extrasCall("install", { extras: pending }, "Installing…")}>
            ${hr.busy ? "Installing…" : `Install [proxy,${pending.join(",")}]`}</button>`}
          ${hr.busy && html`<pre class="code-box log" style="width:100%">${hr.log || "Starting…"}</pre>`}</div>`}</div>
      <${Toggle} on=${s.headroomEnabled} disabled=${locked("headroomEnabled")} onChange=${(v) => save({ headroomEnabled: v }, v ? "Headroom on" : "Headroom off")} /></div>
    <div class="ts-row">
      <div class="grow"><div class="ts-t">Compress LLM output ${link("https://github.com/JuliusBrussee/caveman", "Caveman")}<${Lock} k="caveman" /></div>
        <div class="ts-d">Terse-style system prompt → ~65% fewer output tokens (up to 87%)</div></div>
      <${SegLv} value=${s.cavemanLevel} levels=${CAVEMAN_LV} disabled=${locked("caveman")} onChange=${(v) => save({ cavemanLevel: v })} />
      <${Toggle} on=${s.cavemanEnabled} disabled=${locked("caveman")} onChange=${(v) => save({ cavemanEnabled: v }, v ? "Caveman on" : "Caveman off")} /></div>
    <div class="ts-row">
      <div class="grow"><div class="ts-t">Lazy senior dev ${link("https://github.com/DietrichGebert/ponytail", "Ponytail")}<${Lock} k="ponytail" /></div>
        <div class="ts-d">Bias the model toward minimal code: YAGNI, reuse stdlib, deletion over addition</div></div>
      <${SegLv} value=${s.ponytailLevel} levels=${PONYTAIL_LV} disabled=${locked("ponytail")} onChange=${(v) => save({ ponytailLevel: v })} />
      <${Toggle} on=${s.ponytailEnabled} disabled=${locked("ponytail")} onChange=${(v) => save({ ponytailEnabled: v }, v ? "Ponytail on" : "Ponytail off")} /></div>
    <div class="ts-row last">
      <div class="grow"><div class="ts-t">Custom system prompt</div>
        <div class="ts-d">Appends your own instruction to every request, independently of the savers above</div>
        ${s.customPromptEnabled && html`<div class="col" style="gap:8px;margin-top:10px">
          <textarea class="input" rows="4" maxlength="8000" placeholder="e.g. Always answer in British English." value=${prompt} onInput=${(e) => setPrompt(e.target.value)}></textarea>
          <div class="row" style="gap:8px"><span class="small muted grow">${prompt.trim().length} characters</span>
            <button class="btn primary sm" disabled=${prompt === (s.customPrompt || "")} onClick=${() => save({ customPrompt: prompt }, "Custom prompt saved")}><${Icon} n="save" />Save prompt</button></div></div>`}</div>
      <${Toggle} on=${s.customPromptEnabled} aria-label="Custom system prompt" onChange=${(v) => save({ customPromptEnabled: v }, v ? "Custom prompt on" : "Custom prompt off")} /></div>
  </div>
  ${setup && html`<${HeadroomSetup} s=${s} hr=${hr} refresh=${refresh} save=${save} locked=${locked} onClose=${() => setSetup(false)} />`}`;
}

/* -------------------------------------------------------------- skills */
// Agent Skills, after 9Router's: copy a SKILL.md link and paste it to any AI agent.
const SKILL_META = {
  fastrouter: { name: "FastRouter (Entry)", icon: "hub", desc: "Setup + index of all capabilities. Start here — covers base URL, auth, model discovery, prefixes, combos and links to every capability skill." },
  "fastrouter-chat": { name: "Chat", icon: "chat", endpoint: "/v1/chat/completions", desc: "Chat / code-gen via the OpenAI format with streaming, tools and images." },
  "fastrouter-messages": { name: "Anthropic Messages", icon: "forum", endpoint: "/v1/messages", desc: "Claude-style API for Claude Code and Anthropic SDKs, plus count_tokens." },
  "fastrouter-responses": { name: "Responses", icon: "reply_all", endpoint: "/v1/responses", desc: "OpenAI Responses API for the Codex CLI and Agents SDK." },
  "fastrouter-gemini": { name: "Gemini", icon: "auto_awesome", endpoint: "/v1beta/models/{model}:generateContent", desc: "Gemini generateContent for Gemini SDKs and the Gemini CLI." },
  "fastrouter-models": { name: "Models & Combos", icon: "layers", endpoint: "/v1/models", desc: "Model ids, provider prefixes, combos and their strategies." },
};
const SKILL_ORDER = ["fastrouter", "fastrouter-chat", "fastrouter-messages", "fastrouter-responses", "fastrouter-gemini", "fastrouter-models"];
const RAW_BASE = "https://raw.githubusercontent.com/nesaon03/fastrouter/refs/heads/main/skills";
function SkillsPage() {
  const [src, setSrc] = useState(() => localStorage.getItem("fr-skill-src") || "local");
  const [copied, copy] = useCopy();
  const setS = (v) => { setSrc(v); localStorage.setItem("fr-skill-src", v); };
  const url = (id) => (src === "local" ? `${location.origin}/skills/${id}/SKILL.md` : `${RAW_BASE}/${id}/SKILL.md`);
  const entry = `Read this skill and use it: ${url("fastrouter")}`;
  return html`<div class="skills">
    <div class="card sm">
      <div class="row" style="gap:8px;margin-bottom:8px"><span class="small muted grow">Paste this to your AI:</span>
        <${Seg} value=${src} onChange=${setS} options=${[["local", "This PC"], ["github", "GitHub"]]} /></div>
      <div class="paste-box"><span class="grow">Read this skill and use it:<br />${url("fastrouter")}</span><${CopyBtn} text=${entry} id="entry" copied=${copied} copy=${copy} /></div>
      <p class="tiny muted" style="margin-top:8px">${src === "local" ? "Served by FastRouter itself, so agents on this PC can read it with no sign-in." : "nesaon03/fastrouter is a private repo: these links only work for agents that can read it."}</p></div>
    ${SKILL_ORDER.map((id) => { const m = SKILL_META[id]; const entryCard = id === "fastrouter"; return html`<div class="card sm skill ${entryCard ? "entry" : ""}" key=${id}>
      <div class="skill-ic"><${Icon} n=${m.icon} /></div>
      <div class="grow" style="min-width:0">
        <div class="row" style="gap:8px;flex-wrap:wrap"><span class="strong">${m.name}</span>${entryCard ? html`<span class="start-here">START HERE</span>` : html`<code class="ep">${m.endpoint}</code>`}</div>
        <div class="small muted">${m.desc}</div>
        <div class="row tiny muted" style="gap:6px;margin-top:4px"><span class="mono truncate">${url(id)}</span><a href=${url(id)} target="_blank" rel="noreferrer" title="Open"><${Icon} n="open_in_new" /></a></div></div>
      <button class="btn primary sm" onClick=${() => copy(url(id), id)}><${Icon} n=${copied === id ? "check" : "content_copy"} />${copied === id ? "Copied" : "Copy link"}</button></div>`; })}
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
const PAGES = { endpoint: EndpointPage, providers: ProvidersPage, combos: CombosPage, usage: UsagePage, quota: QuotaPage, "speed-test": SpeedTestPage, keys: KeysPage, cli: CliPage, settings: SettingsPage, "token-saver": TokenSaverPage, "console-log": ConsoleLogPage, skills: SkillsPage };
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
  else if (page === "cli" && sub) body = html`<${CliToolPage} ...${props} id=${decodeURIComponent(sub)} />`;
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
