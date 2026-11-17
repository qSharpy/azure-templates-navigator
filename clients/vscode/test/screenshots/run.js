'use strict';
// Documentation screenshots: launches a real VS Code on samples/ with a clean
// profile and runs tour.js inside it, which drives the UI through commands and
// captures the window after each step. macOS only (screencapture + a small
// Swift helper); the app running this needs Screen Recording permission.
//
//   node test/screenshots/run.js              # every shot -> ../../docs/images/vscode/
//   node test/screenshots/run.js hover graph  # only these
const fs = require('fs');
const os = require('os');
const path = require('path');
const { execFileSync } = require('child_process');
const { runTests } = require('@vscode/test-electron');

const repo = path.resolve(__dirname, '../../../..');
const out = path.join(repo, 'docs', 'images', 'vscode');
const windowid = path.join(os.tmpdir(), 'atn-windowid');

(async () => {
  if (process.platform !== 'darwin') throw new Error('Screenshots need macOS (screencapture).');
  if (!fs.existsSync(windowid)) execFileSync('swiftc', ['-O', path.join(repo, 'scripts', 'screenshots', 'windowid.swift'), '-o', windowid]);
  fs.mkdirSync(out, { recursive: true });

  // A clean profile, so the shots show VS Code's defaults and nothing personal.
  const userData = fs.mkdtempSync(path.join(os.tmpdir(), 'atn-shots-'));
  fs.mkdirSync(path.join(userData, 'User'), { recursive: true });
  fs.writeFileSync(path.join(userData, 'User', 'settings.json'), JSON.stringify({
    'window.title': 'ATN-SHOT ${activeEditorShort}',
    'workbench.colorTheme': 'Default Dark Modern',
    'workbench.startupEditor': 'none',
    'workbench.tips.enabled': false,
    'workbench.secondarySideBar.defaultVisibility': 'hidden',
    'workbench.layoutControl.enabled': false,
    'chat.disableAIFeatures': true,
    'chat.commandCenter.enabled': false,
    'editor.minimap.enabled': false,
    'editor.fontSize': 14,
    'window.zoomLevel': 1,
    'editor.hover.delay': 100,
    'security.workspace.trust.enabled': false,
    'update.mode': 'none',
    'telemetry.telemetryLevel': 'off',
    'extensions.ignoreRecommendations': true,
    'git.enabled': false,
    'window.commandCenter': false,
  }, null, 2));

  try {
    await runTests({
      extensionDevelopmentPath: path.resolve(__dirname, '../..'),
      extensionTestsPath: path.resolve(__dirname, 'tour.js'),
      launchArgs: [path.join(repo, 'samples'), '--disable-extensions', '--user-data-dir=' + userData],
      extensionTestsEnv: { ATN_SHOTS_OUT: out, ATN_WINDOWID: windowid, ATN_SHOTS_ONLY: process.argv.slice(2).join(',') },
    });
  } catch (err) {
    console.error('Screenshots failed', err);
    process.exit(1);
  }
})();
