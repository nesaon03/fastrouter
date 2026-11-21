"use strict";

// Downloads the release binary that matches this package version and platform.
// Runs as npm's postinstall and again from the launcher if the binary is gone
// (for example after `npm install --ignore-scripts`).
//
// A failed download is not fatal: the launcher retries on first run and prints
// an actionable error. Set FASTROUTER_STRICT_INSTALL=1 to fail the install
// instead, FASTROUTER_SKIP_DOWNLOAD=1 to skip it, or FASTROUTER_DOWNLOAD_BASE
// to use a mirror.

const crypto = require("crypto");
const fs = require("fs");
const path = require("path");
const { assetInfo } = require("./lib/platform");

const pkg = require("../package.json");

function vendorDir() {
  return path.join(__dirname, "vendor");
}

function binaryPath(version = pkg.version) {
  return path.join(vendorDir(), assetInfo(version).binary);
}

function releaseBase(version = pkg.version) {
  return `https://github.com/zenpo/fastrouter/releases/download/v${version}`;
}

async function download(url) {
  const res = await fetch(url, { redirect: "follow" });
  if (!res.ok) throw new Error(`HTTP ${res.status} for ${url}`);
  return Buffer.from(await res.arrayBuffer());
}

function sha256(buf) {
  return crypto.createHash("sha256").update(buf).digest("hex");
}

// checksumFor reads GoReleaser/`sha256sum` output: "<hex>  <name>" or
// "<hex> *<name>".
function checksumFor(text, name) {
  for (const line of String(text).split(/\r?\n/)) {
    const fields = line.trim().split(/\s+/);
    if (fields.length >= 2 && (fields[1] === name || fields[1] === `*${name}`)) {
      return fields[0].toLowerCase();
    }
  }
  return "";
}

async function install(opts = {}) {
  const version = opts.version || pkg.version;
  const info = assetInfo(version, opts.platform, opts.arch);
  const base = opts.baseUrl || process.env.FASTROUTER_DOWNLOAD_BASE || releaseBase(version);
  const dest = opts.dest || path.join(vendorDir(), info.binary);
  if (opts.force !== true && fs.existsSync(dest)) return dest;

  const bin = await download(`${base}/${info.name}`);
  // checksums.txt is part of every release. When a mirror omits it the download
  // still works, but a mismatch always fails.
  let checksums = "";
  try {
    checksums = (await download(`${base}/checksums.txt`)).toString("utf8");
  } catch (e) {
    checksums = "";
  }
  if (checksums) {
    const want = checksumFor(checksums, info.name);
    if (!want) throw new Error(`checksums.txt has no entry for ${info.name}`);
    const got = sha256(bin);
    if (got !== want) throw new Error(`checksum mismatch for ${info.name}: got ${got}, want ${want}`);
  }

  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.writeFileSync(dest, bin, { mode: 0o755 });
  return dest;
}

async function main() {
  if (process.env.FASTROUTER_SKIP_DOWNLOAD === "1") {
    console.log("fastrouter: binary download skipped (FASTROUTER_SKIP_DOWNLOAD=1)");
    return;
  }
  try {
    const dest = await install();
    console.log(`fastrouter: installed ${path.basename(dest)}`);
  } catch (e) {
    console.warn(`fastrouter: could not download the binary (${e.message}).`);
    console.warn("fastrouter: the launcher will retry on first run; set FASTROUTER_DOWNLOAD_BASE for a mirror.");
    if (process.env.FASTROUTER_STRICT_INSTALL === "1") process.exitCode = 1;
  }
}

if (require.main === module) main();

module.exports = { install, checksumFor, sha256, binaryPath, vendorDir, releaseBase };
