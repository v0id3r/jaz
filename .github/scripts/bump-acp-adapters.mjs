// Bumps the pinned tags of the fork's own adapters (claude, codex, kimi) in
// .github/acp-adapter-assets.json, then regenerates the embedded manifest.
//
//   node bump-acp-adapters.mjs                      # every fork adapter -> its latest release
//   node bump-acp-adapters.mjs --adapter codex      # one adapter -> its latest release
//   node bump-acp-adapters.mjs --adapter codex --tag v2.12.0-jaz.1
//
// Asset names follow the tag: the previous version substring in each pinned
// asset name is replaced with the new one, so a platform whose asset the new
// release does not publish drops out of the spec.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";

const specPath = ".github/acp-adapter-assets.json";
const spec = JSON.parse(readFileSync(specPath, "utf8"));
const forks = ["codex", "claude", "kimi"];

const args = process.argv.slice(2);
const only = argValue("--adapter");
const tag = argValue("--tag");
if (tag && (!only || only === "all")) {
  throw new Error("--tag requires a single --adapter");
}

const names = !only || only === "all" ? forks : [only];
const releaseCache = new Map();
const notes = [];

for (const name of names) {
  const adapter = spec.adapters[name];
  if (!adapter || !forks.includes(name)) {
    throw new Error(`unknown fork adapter: ${name}`);
  }
  const target = tag && names.length === 1 ? tag : latestTag(adapter.repo);
  if (target === adapter.tag) {
    notes.push(`${name}: already pinned at ${target}`);
    continue;
  }
  const version = (t) => t.replace(/^v/, "");
  const release = releaseAssets(adapter.repo, target);
  const assets = {};
  for (const [platform, wanted] of Object.entries(adapter.assets)) {
    const name2 = wanted.name.split(version(adapter.tag)).join(version(target));
    if (!release.has(name2)) {
      notes.push(`${name}: ${platform} has no ${name2} in ${adapter.repo}@${target}, dropping it`);
      continue;
    }
    assets[platform] = { ...wanted, name: name2 };
  }
  if (Object.keys(assets).length === 0) {
    throw new Error(`${name}: ${adapter.repo}@${target} publishes none of the pinned assets`);
  }
  spec.adapters[name] = { ...adapter, tag: target, assets };
  notes.push(`${name}: ${adapter.tag} -> ${target}`);
}

writeFileSync(specPath, `${JSON.stringify(spec, null, 2)}\n`);
execFileSync(process.execPath, [".github/scripts/create-acp-adapters-manifest.mjs"], { stdio: "inherit" });
console.log(notes.join("\n"));

function argValue(flag) {
  const index = args.indexOf(flag);
  return index === -1 ? undefined : args[index + 1];
}

function latestTag(repo) {
  return gh(["api", `repos/${repo}/releases/latest`, "--jq", ".tag_name"]).trim();
}

function releaseAssets(repo, tag2) {
  const key = `${repo}@${tag2}`;
  if (!releaseCache.has(key)) {
    const assets = JSON.parse(gh(["api", `repos/${repo}/releases/tags/${tag2}`, "--jq", ".assets"]));
    releaseCache.set(key, new Map(assets.map((asset) => [asset.name, asset])));
  }
  return releaseCache.get(key);
}

function gh(argv) {
  return execFileSync("gh", argv, { encoding: "utf8", timeout: 60_000 });
}
