import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";

const out = process.argv[2] || "backend/internal/acpadapter/acp-adapters.json";
const specPath = process.argv[3] || ".github/acp-adapter-assets.json";
const spec = JSON.parse(readFileSync(specPath, "utf8"));
const releaseAssets = new Map();

const manifest = {
  adapters: {},
};

for (const [name, adapter] of Object.entries(spec.adapters)) {
  manifest.adapters[name] = { version: adapter.tag.replace(/^v/, ""), assets: {} };
  for (const [platform, wanted] of Object.entries(adapter.assets)) {
    const asset = releaseAssetMap(wanted.repo ?? adapter.repo, wanted.tag ?? adapter.tag).get(wanted.name);
    if (!asset) {
      throw new Error(`${adapter.repo}@${adapter.tag} is missing ${wanted.name}`);
    }
    manifest.adapters[name].assets[platform] = {
      url: asset.browser_download_url,
      sha256: digestSHA256(asset),
      binary: wanted.binary,
      ...(wanted.env ? { env: wanted.env } : {}),
    };
  }
}

mkdirSync(dirname(out), { recursive: true });
writeFileSync(out, `${JSON.stringify(manifest, null, 2)}\n`);

function releaseAssetMap(repo, tag) {
  const key = `${repo}@${tag}`;
  if (!releaseAssets.has(key)) {
    const raw = execFileSync("gh", ["api", `repos/${repo}/releases/tags/${tag}`, "--jq", ".assets"], {
      encoding: "utf8",
    });
    const release = JSON.parse(raw);
    releaseAssets.set(key, new Map(release.map((asset) => [asset.name, asset])));
  }
  return releaseAssets.get(key);
}

function digestSHA256(asset) {
  if (!asset.digest?.startsWith("sha256:")) {
    throw new Error(`${asset.name} is missing a GitHub SHA-256 digest`);
  }
  return asset.digest.slice("sha256:".length);
}
