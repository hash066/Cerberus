// Builds cerberusd into src-tauri/binaries/cerberusd-<target-triple>[.exe] so
// Tauri bundles it as a sidecar (bundle.externalBin). Invoked by tauri's
// beforeBuild/beforeDev hooks (see tauri.conf.json). Cross-platform: run via node.
//
// The daemon is the exact same cmd/cerberusd binary users would run by hand — the
// desktop app just ships it and auto-launches it, so the mesh is up the moment
// the app opens (zero setup for beta testers).
import { execFileSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url)); // tray/
const repoRoot = join(here, ".."); // repo root
const binDir = join(here, "src-tauri", "binaries");
mkdirSync(binDir, { recursive: true });

// Tauri names sidecars <name>-<target-triple>[.exe]; the triple must match what
// tauri is building for. rustc's host triple is that value.
const rv = execFileSync("rustc", ["-vV"], { encoding: "utf8" });
const m = rv.match(/^host:\s*(.+)$/m);
if (!m) {
  console.error("build-sidecar: could not determine rustc host triple from `rustc -vV`");
  process.exit(1);
}
const triple = m[1].trim();
const ext = process.platform === "win32" ? ".exe" : "";
const out = join(binDir, `cerberusd-${triple}${ext}`);

console.log(`build-sidecar: go build ./cmd/cerberusd -> ${out}`);
execFileSync("go", ["build", "-o", out, "./cmd/cerberusd"], {
  cwd: repoRoot,
  stdio: "inherit",
  env: { ...process.env, CGO_ENABLED: "0" },
});
console.log("build-sidecar: done");
