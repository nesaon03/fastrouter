// Dumps 9Router's provider registry (MIT, decolua/9router) as JSON for gen_catalog.py.
// Usage: node scripts/dump_registry.mjs <path to a 9router checkout> > reg.json
import { resolve as require_abs } from "node:path";
const mod = await import("file://" + require_abs(process.argv[2] || ".") + "/open-sse/providers/registry/index.js");
const R = mod.default;
const out = R.map(r => {
  const t = r.transport || {};
  const base = t.baseUrl || (t.baseUrls && t.baseUrls[0]) || r.baseUrl || "";
  return { id: r.id, alias: r.alias, uiAlias: r.uiAlias || null, category: r.category, priority: r.priority ?? null, hidden: !!r.hidden,
    name: r.display?.name || r.id, color: r.display?.color || "", textIcon: r.display?.textIcon || "", icon: r.display?.icon || "",
    keyUrl: r.display?.notice?.apiKeyUrl || r.display?.keyUrl || r.keyUrl || "", website: r.display?.website || "",
    deprecated: !!r.display?.deprecated, noAuth: !!(r.noAuth || t.noAuth), authType: r.authType || null, authModes: r.authModes || null,
    hasOAuth: !!r.hasOAuth, serviceKinds: r.serviceKinds || null, format: t.format || null, base,
    usage: !!(r.features?.usage), usageApikey: !!(r.features?.usageApikey), quotaApi: !!t.usage,
    models: (r.models || []).map(m => ({ id: m.id, name: m.name, up: m.upstreamModelId, kind: m.kind || null })) };
});
console.log(JSON.stringify(out));
