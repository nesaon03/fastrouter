"use strict";

// Offline tests for the npm wrapper: platform mapping, checksum parsing and a
// full install against a local HTTP server. Run with `node --test npm/test.js`.

const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const fs = require("node:fs");
const http = require("node:http");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const { assetInfo } = require("./lib/platform");
const { install, checksumFor } = require("./install");

test("asset names match the release matrix", () => {
  assert.equal(assetInfo("0.6.0", "win32", "x64").name, "fastrouter_0.6.0_windows_amd64.exe");
  assert.equal(assetInfo("0.6.0", "linux", "x64").name, "fastrouter_0.6.0_linux_amd64");
  assert.equal(assetInfo("0.6.0", "linux", "arm64").name, "fastrouter_0.6.0_linux_arm64");
  assert.equal(assetInfo("0.6.0", "darwin", "arm64").name, "fastrouter_0.6.0_darwin_arm64");
  assert.equal(assetInfo("0.6.0", "win32", "x64").binary, "fastrouter.exe");
  assert.equal(assetInfo("0.6.0", "linux", "x64").binary, "fastrouter");
});

test("an unsupported platform is a clear error", () => {
  assert.throws(() => assetInfo("0.6.0", "aix", "ppc64"), /no build for aix\/ppc64/);
});

test("checksumFor accepts both sha256sum spellings", () => {
  const text = ["abc123  fastrouter_0.6.0_linux_amd64", "def456 *fastrouter_0.6.0_windows_amd64.exe", ""].join("\n");
  assert.equal(checksumFor(text, "fastrouter_0.6.0_linux_amd64"), "abc123");
  assert.equal(checksumFor(text, "fastrouter_0.6.0_windows_amd64.exe"), "def456");
  assert.equal(checksumFor(text, "missing"), "");
});

test("install downloads, verifies and marks the binary executable", async () => {
  const version = "9.9.9";
  const info = assetInfo(version, "linux", "x64");
  const payload = Buffer.from("fake-fastrouter-binary");
  const digest = crypto.createHash("sha256").update(payload).digest("hex");
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "fastrouter-npm-"));

  const server = http.createServer((req, res) => {
    if (req.url === `/${info.name}`) {
      res.writeHead(200, { "content-type": "application/octet-stream" });
      res.end(payload);
      return;
    }
    if (req.url === "/checksums.txt") {
      res.writeHead(200, { "content-type": "text/plain" });
      res.end(`${digest}  ${info.name}\n`);
      return;
    }
    res.writeHead(404);
    res.end("no");
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const base = `http://127.0.0.1:${server.address().port}`;

  try {
    const dest = path.join(dir, info.binary);
    await install({ version, platform: "linux", arch: "x64", baseUrl: base, dest, force: true });
    assert.deepEqual(fs.readFileSync(dest), payload);
    if (process.platform !== "win32") {
      assert.ok(fs.statSync(dest).mode & 0o111, "binary should be executable");
    }
    // A second call is a no-op unless forced.
    const before = fs.statSync(dest).mtimeMs;
    await install({ version, platform: "linux", arch: "x64", baseUrl: base, dest });
    assert.equal(fs.statSync(dest).mtimeMs, before);
  } finally {
    server.close();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("a checksum mismatch aborts the install", async () => {
  const version = "9.9.9";
  const info = assetInfo(version, "linux", "x64");
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "fastrouter-npm-"));
  const server = http.createServer((req, res) => {
    if (req.url === `/${info.name}`) {
      res.writeHead(200);
      res.end("payload");
      return;
    }
    if (req.url === "/checksums.txt") {
      res.writeHead(200);
      res.end(`${"0".repeat(64)}  ${info.name}\n`);
      return;
    }
    res.writeHead(404);
    res.end();
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const base = `http://127.0.0.1:${server.address().port}`;
  try {
    await assert.rejects(
      install({ version, platform: "linux", arch: "x64", baseUrl: base, dest: path.join(dir, "fastrouter"), force: true }),
      /checksum mismatch/
    );
    assert.equal(fs.existsSync(path.join(dir, "fastrouter")), false);
  } finally {
    server.close();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
