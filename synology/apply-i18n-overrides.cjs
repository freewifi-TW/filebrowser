#!/usr/bin/env node
// Applies the fork's translation corrections on top of an upstream locale file.
//
//   node synology/apply-i18n-overrides.cjs <frontend/src/i18n> [overrides dir]
//
// For every "<locale>.overrides.json" (flat "a.b.c": "text" map) the matching
// "<locale>.json" is patched in place. A correction is skipped, never fatal, when
// upstream no longer has the key or when its placeholders no longer match the
// English source - so an upstream change can not break the build or the UI.
const { readFileSync, writeFileSync, readdirSync } = require("node:fs");
const { join } = require("node:path");

const i18nDir = process.argv[2];
const overridesDir = process.argv[3] ?? join(__dirname, "i18n");
if (!i18nDir) {
  console.error("usage: apply-i18n-overrides.cjs <frontend/src/i18n> [overrides dir]");
  process.exit(2);
}

const placeholders = (text) => [...text.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort().join(",");
const lookup = (obj, path) => path.split(".").reduce((node, key) => (node && typeof node === "object" ? node[key] : undefined), obj);

const english = JSON.parse(readFileSync(join(i18nDir, "en.json"), "utf8"));
let failed = false;

for (const file of readdirSync(overridesDir).filter((f) => f.endsWith(".overrides.json"))) {
  const locale = file.replace(/\.overrides\.json$/, "");
  const target = join(i18nDir, `${locale}.json`);
  let messages;
  try {
    messages = JSON.parse(readFileSync(target, "utf8"));
  } catch (err) {
    console.error(`${locale}: cannot read ${target}: ${err.message}`);
    failed = true;
    continue;
  }
  const overrides = JSON.parse(readFileSync(join(overridesDir, file), "utf8"));
  let applied = 0;
  const skipped = [];

  for (const [path, text] of Object.entries(overrides)) {
    const source = lookup(english, path);
    if (typeof source !== "string" || typeof lookup(messages, path) !== "string") {
      skipped.push(`${path} (no longer exists upstream)`);
      continue;
    }
    if (placeholders(source) !== placeholders(text)) {
      skipped.push(`${path} (placeholders changed: {${placeholders(source)}})`);
      continue;
    }
    const keys = path.split(".");
    const leaf = keys.pop();
    keys.reduce((node, key) => node[key], messages)[leaf] = text;
    applied++;
  }

  writeFileSync(target, JSON.stringify(messages, null, 2) + "\n");
  console.log(`${locale}: applied ${applied} corrections, skipped ${skipped.length}`);
  for (const line of skipped) console.log(`  skipped ${line}`);
}

process.exit(failed ? 1 : 0);
