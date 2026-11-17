'use strict';

// Copies the shared webview, the tips' screenshots (docs/images/vscode) and the
// prebuilt `atn` binaries into this extension, and writes the Get Started
// walkthrough into package.json. Never edit media/ or bin/ by hand — they are
// build outputs.
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

// ── Tips: shared/tips/tips.json → the Get Started walkthrough ─────────────
// The steps live in package.json (VS Code reads walkthroughs only from there),
// so this script rewrites that section; edit tips.json, not the steps.
const WALKTHROUGH_ID = 'getStarted';
const tips = JSON.parse(fs.readFileSync(path.join(repo, 'shared', 'tips', 'tips.json'), 'utf8')).tips;
const pkgPath = path.join(here, 'package.json');
// A tip's screenshot, when there is one: docs/images/vscode/<id>.gif or .png (test/screenshots/run.js).
const tipImage = t => ['gif', 'png'].map(ext => path.join(repo, 'docs', 'images', 'vscode', `${t.id}.${ext}`)).find(f => fs.existsSync(f));
/** The tip's file under media/tips/: its screenshot, or else a markdown code block of its example. */
const tipMedia = t => `${t.id}${tipImage(t) ? path.extname(tipImage(t)) : '.md'}`;

function commandLink(t) {
  if (!t.vscodeCommand) return '';
  const args = t.vscodeArgs === undefined ? '' : `?${encodeURIComponent(JSON.stringify([t.vscodeArgs]))}`;
  return `\n[${t.vscodeButton}](command:${t.vscodeCommand}${args})`;
}

function walkthrough() {
  return {
    id: WALKTHROUGH_ID,
    title: 'Get Started with Azure Templates Navigator',
    description: 'What the extension does for your Azure Pipelines YAML, one feature at a time.',
    steps: tips.map(t => ({
      id: t.id,
      title: t.title,
      // Walkthrough steps render bold and links but print `code` literally: quote it instead.
      description: t.vscode.replace(/`([^`]+)`/g, '"$1"') + commandLink(t),
      media: tipImage(t)
        ? { image: `media/tips/${tipMedia(t)}`, altText: t.title }
        : { markdown: `media/tips/${tipMedia(t)}` },
      ...(t.vscodeCommand ? { completionEvents: [`onCommand:${t.vscodeCommand}`] } : {}),
    })),
  };
}

function walkthroughInSync(pkg) {
  const w = (pkg.contributes.walkthroughs || [])[0];
  return JSON.stringify(w) === JSON.stringify(walkthrough());
}

if (check) {
  const bin = path.join(here, 'bin');
  const ok = fs.existsSync(path.join(here, 'media', 'graph.html')) && fs.existsSync(bin) && fs.readdirSync(bin).length > 0
    && tips.every(t => fs.existsSync(path.join(here, 'media', 'tips', tipMedia(t))));
  if (!ok) {
    console.error('Missing media/ or bin/. Run ../../scripts/build.sh (or `npm run assets` after building the server).');
    process.exit(1);
  }
  if (!walkthroughInSync(JSON.parse(fs.readFileSync(pkgPath, 'utf8')))) {
    console.error('package.json walkthrough is out of date with shared/tips/tips.json. Run `npm run assets` and commit package.json.');
    process.exit(1);
  }
  process.exit(0);
}

copyDir(path.join(repo, 'shared', 'webview'), path.join(here, 'media'));

// After the webview copy: copyDir wipes media/.
fs.mkdirSync(path.join(here, 'media', 'tips'), { recursive: true });
for (const t of tips) {
  if (tipImage(t)) {
    fs.copyFileSync(tipImage(t), path.join(here, 'media', 'tips', tipMedia(t)));
    continue;
  }
  const fence = '```';
  fs.writeFileSync(path.join(here, 'media', 'tips', `${t.id}.md`), `${fence}${t.exampleLanguage || ''}\n${t.example}\n${fence}\n`);
}
const pkg = JSON.parse(fs.readFileSync(pkgPath, 'utf8'));
if (!walkthroughInSync(pkg)) {
  pkg.contributes.walkthroughs = [walkthrough()];
  fs.writeFileSync(pkgPath, JSON.stringify(pkg, null, 2) + '\n');
  console.log('Updated the walkthrough in package.json from shared/tips/tips.json');
}

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
