"use strict";

// Platform names used by the release assets. Keep these in sync with the
// build matrix in .github/workflows/release.yml.

const OS_NAMES = { win32: "windows", linux: "linux", darwin: "darwin" };
const ARCH_NAMES = { x64: "amd64", arm64: "arm64" };

function assetInfo(version, platform = process.platform, arch = process.arch) {
  const os = OS_NAMES[platform];
  const cpu = ARCH_NAMES[arch];
  if (!os || !cpu) {
    throw new Error(
      `FastRouter has no build for ${platform}/${arch}. Supported: ` +
        "windows/x64, linux/x64, linux/arm64, darwin/x64, darwin/arm64."
    );
  }
  const ext = platform === "win32" ? ".exe" : "";
  return {
    os,
    arch: cpu,
    ext,
    name: `fastrouter_${version}_${os}_${cpu}${ext}`,
    binary: `fastrouter${ext}`,
  };
}

module.exports = { assetInfo, OS_NAMES, ARCH_NAMES };
