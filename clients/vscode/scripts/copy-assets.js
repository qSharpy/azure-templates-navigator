'use strict';

// Copies the shared webview and the prebuilt `atn` binaries into this
// extension. Never edit media/ or bin/ by hand — they are build outputs.
//
//   node scripts/copy-assets.js                 # all binaries in dist/bin
//   node scripts/copy-assets.js --target darwin-arm64
//   node scripts/copy-assets.js --check         # verify only (prepublish)

const fs = require('fs');
const path = require('path');

const here = path.resolve(__dirname, '..');
const repo = path.resolve(here, '..', '..');
const args = process.argv.slice(2);
const check = args.includes('--check');
const ti = args.indexOf('--target');
const target = ti >= 0 ? args[ti + 1] : process.env.ATN_TARGET;

function copyDir(src, dst) {
  fs.rmSync(dst, { recursive: true, force: true });
  fs.mkdirSync(dst, { recursive: true });
  for (const e of fs.readdirSync(src, { withFileTypes: true })) {
    const s = path.join(src, e.name), d = path.join(dst, e.name);
    if (e.isDirectory()) copyDir(s, d); else fs.copyFileSync(s, d);
  }
}

if (check) {
  const bin = path.join(here, 'bin');
  const ok = fs.existsSync(path.join(here, 'media', 'graph.html')) && fs.existsSync(bin) && fs.readdirSync(bin).length > 0;
  if (!ok) {
    console.error('Missing media/ or bin/. Run ../../scripts/build.sh (or `npm run assets` after building the server).');
    process.exit(1);
  }
  process.exit(0);
}

copyDir(path.join(repo, 'shared', 'webview'), path.join(here, 'media'));

const distBin = path.join(repo, 'dist', 'bin');
if (!fs.existsSync(distBin)) {
  console.error(`No server binaries in ${distBin}. Build them first: ./scripts/build.sh server`);
  process.exit(1);
}
const outBin = path.join(here, 'bin');
fs.rmSync(outBin, { recursive: true, force: true });
for (const t of fs.readdirSync(distBin)) {
  if (target && t !== target) continue;
  copyDir(path.join(distBin, t), path.join(outBin, t));
}
console.log(`Copied webview + binaries (${fs.readdirSync(outBin).join(', ')})`);
