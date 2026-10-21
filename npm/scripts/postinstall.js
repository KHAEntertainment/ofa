#!/usr/bin/env node
// Postinstall: stage the right binary for the user's platform/arch.
// Single-arch for v0.2.0; multi-arch will be optionalDependencies in
// a later release.

const fs = require("node:fs");
const path = require("node:path");

const PLATFORM_DIRS = {
  "darwin-arm64": "darwin-arm64",
  "darwin-x64":   "darwin-x64",
  "linux-arm64":  "linux-arm64",
  "linux-x64":    "linux-x64",
};

const key = `${process.platform}-${process.arch}`;
const subdir = PLATFORM_DIRS[key];

if (!subdir) {
  console.error(`@khaentertainment/ofa: no binary bundled for ${key}.`);
  console.error(`Install Go 1.25+ and run: go install github.com/KHAEntertainment/ofa/cmd/ofa@latest`);
  // Don't fail the install — the user can fall back to `go install`.
  process.exit(0);
}

const src = path.join(__dirname, "..", "platforms", subdir, "ofa");
const dst = path.join(__dirname, "..", "bin", "ofa-bin");

try {
  fs.copyFileSync(src, dst);
  fs.chmodSync(dst, 0o755);
} catch (err) {
  console.error(`@khaentertainment/ofa: failed to stage binary for ${key}: ${err.message}`);
  process.exit(1);
}
