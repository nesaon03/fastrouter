// Regenerates vectors.json from 9Router's own implementation (MIT).
// usage: node gen-vectors.mjs /path/to/9router/repo > vectors.json
const repo = process.argv[2];
const { compressMessages } = await import(`${repo}/open-sse/rtk/index.js`);
const { injectSystemPrompt } = await import(`${repo}/open-sse/rtk/systemInject.js`);

const rep = (n, f) => Array.from({ length: n }, (_, i) => f(i)).join("\n");
const texts = {
  gitLog: rep(80, i => `commit ${(1000000 + i).toString(16)}abcdef0\nAuthor: Dev <dev@example.com>\nDate:   Mon Jan 1 00:00:00 2026\n\n    Change number ${i}\n\n    longer body text that should be dropped ${i}\n`),
  gitDiff: "diff --git a/x.go b/x.go\nindex 1..2 100644\n--- a/x.go\n+++ b/x.go\n@@ -1,3 +1,300 @@\n" + rep(300, i => (i % 3 ? "+" : " ") + `line ${i}`) + "\ndiff --git a/y.go b/y.go\n@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file",
  gitStatus: "On branch main\nChanges not staged for commit:\n" + rep(30, i => `\tmodified:   src/file${i}.go`) + "\nUntracked files:\n\tfoo.txt\n",
  porcelain: "## main...origin/main\n" + rep(40, i => [" M", "A ", "??", "MM", "UU"][i % 5] + ` path/to/file${i}.txt`),
  build: rep(40, i => `   Compiling crate${i} v0.1.${i}`) + "\nwarning: unused variable `x`\n --> src/main.rs:3:9\n  |\n3 |     let x = 1;\n\nerror[E0425]: cannot find value\n --> src/lib.rs:1:1\n\n    Finished dev [unoptimized] target(s) in 3.2s\n" + rep(6, i => `npm warn deprecated pkg${i}@1.0.0: old`),
  grep: rep(60, i => `src/file${i % 4}.go:${i + 1}:    match here ${i}`),
  find: rep(80, i => `./dir${i % 25}/sub/file${i}.txt`),
  tree: ".\n" + rep(250, i => `├── dir${i}\n│   └── file${i}.go`) + "\n\n250 directories, 250 files\n",
  ls: "total 120\n" + rep(40, i => `${i % 4 ? "-rw-r--r--" : "drwxr-xr-x"}  1 user staff  ${i * 1000} Jan ${(i % 28) + 1} 12:00 ${i % 4 ? `file${i}.${["go", "js", "md", "txt", "py", "rs", "c"][i % 7]}` : `dir${i}`}`) + "\ndrwxr-xr-x 1 user staff 0 Jan 1 12:00 node_modules",
  search: "Result of search in '/repo' (total 60 files):\n" + rep(60, i => `- src/pkg${i % 23}/file${i}.ts`),
  dedup: rep(200, i => (i % 10 < 7 ? "same repeated log line" : `distinct ${i}`)) + "\n\n\n\nend",
  trunc: rep(400, i => `x${i}`),
  short: "too short",
  plain: "word ".repeat(200),
};

const out = { rtk: [], inject: [] };
for (const [name, text] of Object.entries(texts)) {
  const body = { messages: [{ role: "tool", tool_call_id: "1", content: text }] };
  compressMessages(body, true);
  out.rtk.push({ name, in: text, out: body.messages[0].content });
}
const P = "PROMPT TEXT";
const shapes = {
  rtkShapes: [null, { messages: [
    { role: "user", content: [{ type: "tool_result", tool_use_id: "a", content: texts.grep }, { type: "tool_result", is_error: true, content: texts.grep }] },
    { role: "user", content: [{ type: "tool_result", content: [{ type: "text", text: texts.find }, { type: "image" }] }] },
    { role: "tool", content: [{ type: "text", text: texts.trunc }] },
  ] }],
  rtkResponses: [null, { input: [{ type: "function_call_output", call_id: "c", output: texts.dedup },
    { type: "function_call_output", output: [{ type: "input_text", text: texts.gitDiff }] }, { type: "message", role: "user", content: "hi" }] }],
};
for (const [name, [, body]] of Object.entries(shapes)) {
  const before = JSON.stringify(body);
  compressMessages(body, true);
  out.rtk.push({ name, body: JSON.parse(before), want: body });
}
const cases = [
  ["anthropic", "claude-none", { model: "m", messages: [] }],
  ["anthropic", "claude-string", { system: "be nice", messages: [] }],
  ["anthropic", "claude-empty", { system: "", messages: [] }],
  ["anthropic", "claude-dup", { system: `be nice\n\n${P}`, messages: [] }],
  ["anthropic", "claude-array-cache", { system: [{ type: "text", text: "a" }, { type: "text", text: "b", cache_control: { type: "ephemeral" } }, { type: "text", text: "c" }] }],
  ["anthropic", "claude-array", { system: [{ type: "text", text: "a" }] }],
  ["gemini", "gemini-none", { contents: [] }],
  ["gemini", "gemini-parts", { systemInstruction: { parts: [{ text: "x" }] }, contents: [] }],
  ["gemini", "gemini-snake", { system_instruction: { parts: [] }, contents: [] }],
  ["openai", "chat-none", { messages: [{ role: "user", content: "hi" }] }],
  ["openai", "chat-empty", { messages: [] }],
  ["openai", "chat-sys", { messages: [{ role: "user", content: "hi" }, { role: "system", content: "sys" }] }],
  ["openai", "chat-dev-array", { messages: [{ role: "developer", content: [{ type: "text", text: "d" }] }] }],
  ["openai", "chat-dup", { messages: [{ role: "system", content: P }] }],
  ["openai", "chat-null", { messages: [{ role: "system", content: null }] }],
  ["openai", "instructions", { instructions: "base", messages: [{ role: "user", content: "x" }] }],
  ["responses", "responses-none", { input: [{ type: "message", role: "user", content: "hi" }] }],
  ["responses", "responses-dev", { input: [{ type: "message", role: "developer", content: [{ type: "input_text", text: "d" }] }] }],
  ["responses", "responses-string", { input: "hello" }],
  ["responses", "responses-instr-empty", { instructions: "", input: [] }],
];
const fmt = { anthropic: "claude", gemini: "gemini", openai: "openai", responses: "openai-responses" };
for (const [f, name, body] of cases) {
  const before = JSON.stringify(body);
  injectSystemPrompt(body, fmt[f], P);
  out.inject.push({ format: f, name, body: JSON.parse(before), want: body });
}
console.log(JSON.stringify(out, null, 1));
