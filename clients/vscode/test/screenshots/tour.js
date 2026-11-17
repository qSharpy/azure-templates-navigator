'use strict';
// Runs inside VS Code (see run.js). Each shot sets the UI up through commands,
// then captures the window into ATN_SHOTS_OUT/<name>.png.
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');
const vscode = require('vscode');

const OUT = process.env.ATN_SHOTS_OUT;
const ONLY = (process.env.ATN_SHOTS_ONLY || '').split(',').filter(Boolean);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const cmd = (id, ...args) => vscode.commands.executeCommand(id, ...args);

async function waitFor(fn, ms = 20000) {
  const end = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() > end) throw new Error('timeout');
    await sleep(200);
  }
}

/**
 * Captures the VS Code window (only it, without its shadow), then crops it to
 * `crop` ({x, y, w, h} as fractions of the window, below the title bar) and
 * reduces it to a palette PNG.
 */
async function capture(file, crop, settle = 700) {
  await sleep(settle);
  await cmd('notifications.clearAll');
  await sleep(300);
  const id = execFileSync(process.env.ATN_WINDOWID, ['ATN-SHOT']).toString().trim();
  execFileSync('screencapture', ['-x', '-o', '-l' + id, file]);
  const [W, H] = execFileSync('magick', ['identify', '-format', '%w %h', file]).toString().split(' ').map(Number);
  const title = Math.round(56 * W / 2880); // the title bar, 28pt
  const c = Object.assign({ x: 0, y: 0, w: 1, h: 1 }, crop);
  const geo = `${Math.round(c.w * W)}x${Math.round(c.h * (H - title))}+${Math.round(c.x * W)}+${title + Math.round(c.y * (H - title))}`;
  execFileSync('magick', [file, '-crop', geo, '+repage', '-strip', '-colors', '256', file]);
}

let root;
const file = rel => vscode.Uri.file(path.join(root, rel));

/** Opens a file with the cursor on the first line matching `find` (or `line`), centred. */
async function open(rel, find, { column = 0, line } = {}) {
  const doc = await vscode.workspace.openTextDocument(file(rel));
  const editor = await vscode.window.showTextDocument(doc, { preview: false });
  let l = line || 0;
  if (find) l = doc.getText().split('\n').findIndex(t => t.includes(find));
  if (find && column === 'match') column = doc.lineAt(l).text.indexOf(find);
  const pos = new vscode.Position(Math.max(l, 0), column || 0);
  editor.selection = new vscode.Selection(pos, pos);
  editor.revealRange(new vscode.Range(pos, pos), vscode.TextEditorRevealType.InCenter);
  return editor;
}

async function reset() {
  await cmd('workbench.action.resetViewLocations'); // undoes sidebarView/onlyView moves
  await cmd('workbench.action.closeQuickOpen');
  await cmd('closeReferenceSearch');
  await cmd('workbench.action.closeAllEditors');
  await cmd('workbench.action.closePanel');
  await cmd('workbench.action.closeAuxiliaryBar');
  await cmd('notifications.clearAll');
}

/** Waits until the editor's code lenses include one whose title matches. */
async function lens(editor, re) {
  return waitFor(async () => {
    const l = await cmd('vscode.executeCodeLensProvider', editor.document.uri, 20);
    return l && l.find(x => x.command && re.test(x.command.title));
  });
}

/**
 * Shows one view alone in its sidebar container: the container's other views move to another
 * container (reset() puts them back with resetViewLocations).
 */
async function onlyView(id, others, elsewhere) {
  await cmd('vscode.moveViews', { viewIds: others, destinationId: elsewhere });
  await cmd(`${id}.focus`);
}
const OUR_VIEWS = ['templateTree', 'pipelinesView', 'graphView', 'diagnosticsPanel', 'repositoriesView', 'statsView', 'feedbackView']
  .map(v => `azure-templates-navigator.${v}`);
const sidebarView = id => onlyView(`azure-templates-navigator.${id}`,
  OUR_VIEWS.filter(v => v !== `azure-templates-navigator.${id}`), 'workbench.view.explorer');

/** Types text at the cursor a few characters at a time, capturing a frame after each chunk. */
async function typeOut(editor, text, frame, chunk = 4) {
  for (let i = 0; i < text.length; i += chunk) {
    await editor.edit(e => e.insert(editor.selection.active, text.slice(i, i + chunk)));
    await frame(150);
  }
}

// Each shot: set the UI up (setUp may capture GIF frames with frame()), then the crop to keep
// (fractions of the window below the title bar). The PNG is the last state; shots that capture
// frames also get <name>.gif. Tips use the shots named like their ids (shared/tips/tips.json).
const SHOTS = {
  // The verdict above a pipeline's first line.
  pipelineVerdict: { crop: { w: 0.5, h: 0.3 }, settle: 2500, async setUp() {
    await cmd('workbench.action.closeSidebar');
    const editor = await open('azure-pipelines.yml', null, { line: 0 });
    await lens(editor, /queued|before running/);
  } },
  // Clicking the verdict: every problem in the pipeline and its templates.
  checkPipeline: { crop: { x: 0.1, w: 0.8, h: 0.45 }, settle: 1500, async setUp() {
    await cmd('workbench.action.closeSidebar');
    const editor = await open('azure-pipelines.yml', null, { line: 0 });
    await lens(editor, /queued|before running/);
    cmd('azure-templates-navigator.showPipelineProblems', { uri: editor.document.uri.toString() }); // resolves on pick
  } },
  // Every pipeline's verdict, failing first.
  pipelinesView: { crop: { w: 0.42, h: 0.75 }, settle: 2500, async setUp() {
    await open('azure-pipelines.yml', null, { line: 0 });
    await sidebarView('pipelinesView');
  } },
  // My changes: a template parameter renamed (in memory), so the pipelines calling it get new
  // errors on top of the ones they already had.
  myChanges: { crop: { w: 0.42, h: 0.6 }, settle: 3000, async setUp() {
    const editor = await open('templates/run-tests.yml', '- name: testProject');
    const l = editor.selection.active.line;
    const c = editor.document.lineAt(l).text.indexOf('testProject') + 'testProject'.length;
    await editor.edit(e => e.insert(new vscode.Position(l, c), 'Path'));
    await cmd('azure-templates-navigator.showMyChanges');
    await sidebarView('pipelinesView');
  }, async cleanUp() {
    await cmd('workbench.action.files.revert');
    await cmd('azure-templates-navigator.showAllPipelines');
  } },
  // Hover on a template call: its parameters.
  hoverParameters: { crop: { x: 0.05, y: 0.12, w: 0.62, h: 0.62 }, settle: 1500, async setUp() {
    await cmd('workbench.action.closeSidebar');
    await open('azure-pipelines.yml', 'templates/build-dotnet.yml', { column: 'match' });
    await sleep(800);
    await cmd('editor.action.showHover');
  } },
  // Hover on $(variable): its value and where it is set.
  hoverVariable: { crop: { x: 0.05, y: 0.2, w: 0.62, h: 0.45 }, settle: 1500, async setUp() {
    await cmd('workbench.action.closeSidebar');
    const editor = await open('azure-pipelines.yml', 'buildConfiguration: $(buildConfiguration)');
    const l = editor.selection.active.line;
    const c = editor.document.lineAt(l).text.indexOf('$(buildConfiguration)') + 3;
    editor.selection = new vscode.Selection(l, c, l, c);
    await sleep(800);
    await cmd('editor.action.showHover');
  } },
  // Completing a template path inserts its required parameters.
  completeRequired: { crop: { x: 0.05, y: 0.15, w: 0.7, h: 0.55 }, settle: 1200, gif: true, async setUp(frame) {
    await cmd('workbench.action.closeSidebar');
    const editor = await open('azure-pipelines.yml', '- template: templates/run-tests.yml');
    const l = editor.selection.active.line;
    const indent = editor.document.lineAt(l).text.match(/^\s*/)[0];
    await editor.edit(e => e.insert(new vscode.Position(l, 0), `${indent}\n`));
    editor.selection = new vscode.Selection(l, indent.length, l, indent.length);
    await frame(400);
    await typeOut(editor, '- template: templates/noti', frame);
    await cmd('editor.action.triggerSuggest');
    await frame(1200);
    await frame(400);
    await cmd('acceptSelectedSuggestion');
  }, async cleanUp() { await cmd('workbench.action.files.revert'); } },
  // A quick fix: "did you mean".
  quickFixes: { crop: { x: 0.05, y: 0.15, w: 0.7, h: 0.6 }, settle: 1200, gif: true, async setUp(frame) {
    await cmd('workbench.action.closeSidebar');
    await open('extends/teams/search/azure-pipelines.yml', 'dependson:', { column: 'match' });
    await sleep(1500);
    await frame(600);
    await cmd('editor.action.showHover');
    await frame(1500);
    await cmd('editor.action.quickFix');
    await frame(1500);
    await cmd('acceptSelectedCodeAction');
    // The hover still shows the fixed problem until the cursor moves.
    const editor = vscode.window.activeTextEditor;
    const p = editor.selection.active.translate(1, 0);
    editor.selection = new vscode.Selection(p, p);
    await cmd('editor.action.hideHover').then(() => {}, () => {});
  }, async cleanUp() { await cmd('workbench.action.files.revert'); } },
  // "N usages" above a template, and Find All References on it. (Not the lens's peek: its title
  // shows the absolute path of the folder, which would put the machine's home folder in the docs.)
  findUsages: { crop: { w: 0.75, h: 0.6 }, settle: 2500, async setUp() {
    const editor = await open('extends/platform-templates/pipelines/secure-base.yml', 'parameters:');
    await lens(editor, /usage/);
    await cmd('references-view.findReferences');
  } },
  // Call hierarchy: every pipeline that reaches a template.
  callHierarchy: { crop: { w: 0.75, h: 0.6 }, settle: 2500, async setUp() {
    await open('extends/platform-templates/pipelines/secure-base.yml', 'parameters:');
    await cmd('references-view.showCallHierarchy');
  } },
  // Outline: stages, jobs, steps and template calls.
  outline: { crop: { w: 0.75, h: 0.75 }, settle: 1500, async setUp() {
    await open('extends/teams/search/azure-pipelines.yml', null, { line: 0 });
    await cmd('workbench.view.explorer');
    await onlyView('outline', ['workbench.explorer.fileView', 'timeline'], 'workbench.view.scm');
    await sleep(500);
    await cmd('outline.expand').then(() => {}, () => {});
  } },
  // The Dependencies tree of the current file.
  dependencies: { crop: { w: 0.42, h: 0.75 }, settle: 2500, async setUp() {
    await open('azure-pipelines.yml', null, { line: 0 });
    await sidebarView('templateTree');
  } },
  // The template graph, around the current file.
  templateGraph: { crop: { w: 1, h: 1 }, settle: 3500, async setUp() {
    await cmd('workbench.action.closeSidebar');
    await open('extends/platform-templates/pipelines/secure-base.yml', null, { line: 0 });
    await cmd('azure-templates-navigator.expandTemplateGraph');
  } },
  // The Diagnostics view: every problem in the workspace.
  diagnostics: { crop: { w: 0.42, h: 0.75 }, settle: 2500, async setUp() {
    await open('azure-pipelines.yml', null, { line: 0 });
    await sidebarView('diagnosticsPanel');
  } },
  // Repositories that templates come from.
  otherRepositories: { crop: { w: 0.42, h: 0.75 }, settle: 2500, async setUp() {
    await open('cross-repo-pipeline.yml', null, { line: 0 });
    await sidebarView('repositoriesView');
    await sleep(1500);
    await cmd('list.focusFirst'); // expand the first repository: the templates used from it
    await cmd('list.expand');
  } },
  // Stats for template owners.
  stats: { crop: { w: 0.42, h: 0.85 }, settle: 2500, async setUp() {
    await open('azure-pipelines.yml', null, { line: 0 });
    await sidebarView('statsView');
  } },
  // Search Templates…
  searchTemplates: { crop: { x: 0.15, w: 0.7, h: 0.55 }, settle: 1500, async setUp() {
    await cmd('workbench.action.closeSidebar');
    cmd('azure-templates-navigator.searchTemplates'); // resolves on pick
  } },
  // Help and Feedback: Get Started, What's New, report a bug…
  helpAndFeedback: { crop: { w: 0.42, h: 0.5 }, settle: 1500, async setUp() {
    await sidebarView('feedbackView');
  } },
  // The Get Started walkthrough, on its first step.
  walkthrough: { crop: {}, settle: 2000, async setUp() {
    await cmd('workbench.action.closeSidebar'); // narrower, and the walkthrough drops its media column
    const category = 'bogdanbujor.azure-templates-navigator#getStarted';
    await cmd('workbench.action.openWalkthrough', { category, step: `${category}#pipelineVerdict` }, false);
  } },
};

async function run() {
  root = vscode.workspace.workspaceFolders[0].uri.fsPath;
  const ext = vscode.extensions.getExtension('bogdanbujor.azure-templates-navigator');
  await ext.activate();
  await sleep(2500);
  for (const [name, shot] of Object.entries(SHOTS)) {
    if (ONLY.length && !ONLY.includes(name)) continue;
    await reset();
    const frames = [];
    const frame = async (ms = 0) => {
      const f = path.join(require('os').tmpdir(), `atn-frame-${name}-${String(frames.length).padStart(2, '0')}.png`);
      await capture(f, shot.crop, ms);
      frames.push(f);
    };
    await shot.setUp(frame);
    await capture(path.join(OUT, `${name}.png`), shot.crop, shot.settle);
    if (shot.gif) {
      // 60% size keeps the GIF light; the last frame stays up longer.
      const args = ['-delay', '70', ...frames, '-delay', '300', path.join(OUT, `${name}.png`), '-loop', '0', '-resize', '60%', '-fuzz', '3%', '-layers', 'Optimize', path.join(OUT, `${name}.gif`)];
      execFileSync('magick', args);
      for (const f of frames) fs.rmSync(f);
    }
    if (shot.cleanUp) await shot.cleanUp();
    console.log('shot', name);
  }
  await reset();
}

module.exports = { run };
