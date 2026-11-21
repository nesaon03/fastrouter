#!/usr/bin/env node
"use strict";

// Launcher: runs the downloaded FastRouter binary and forwards signals and the
// exit code, so `fastrouter serve` behaves like the binary itself. If the
// postinstall download was skipped, it is retried once here.

const fs = require("fs");
const { spawn } = require("child_process");
const { binaryPath, install } = require("./install");

async function main() {
  let bin = binaryPath();
  if (!fs.existsSync(bin)) {
    console.error("fastrouter: binary missing, downloading the release for this platform...");
    try {
      bin = await install();
    } catch (e) {
      console.error(`fastrouter: download failed: ${e.message}`);
      console.error("fastrouter: reinstall online with `npm i -g fastrouter@latest`, or set FASTROUTER_DOWNLOAD_BASE.");
      process.exit(1);
    }
  }

  const child = spawn(bin, process.argv.slice(2), { stdio: "inherit" });
  for (const sig of ["SIGINT", "SIGTERM"]) {
    process.on(sig, () => {
      try {
        child.kill(sig);
      } catch (e) {
        /* the child is already gone */
      }
    });
  }
  child.on("error", (e) => {
    console.error(`fastrouter: could not start ${bin}: ${e.message}`);
    process.exit(1);
  });
  child.on("exit", (code, signal) => {
    if (signal) process.kill(process.pid, signal);
    else process.exit(code === null ? 1 : code);
  });
}

main();
