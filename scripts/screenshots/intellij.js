#!/usr/bin/env node
'use strict';
// Documentation screenshots for IntelliJ -> docs/images/intellij/<name>.png.
//
// Drives the sandbox started by `./gradlew runIdeForScreenshots --args=<repo>/samples` (clients/intellij)
// through JetBrains' UI-test robot server (127.0.0.1:8082), which runs JavaScript (Rhino) inside the IDE:
// open a file at a line, run an IDE action by id, pick a tool window tab. Nothing is typed, so it needs no
// keyboard or accessibility access. It captures only that IDE's windows (the main frame plus any popup on
// it), so other apps never end up in a shot. macOS only; needs Screen Recording permission.
//
//   node scripts/screenshots/intellij.js              # every shot
//   node scripts/screenshots/intellij.js tipsDialog   # only these
const fs = require('fs');
const os = require('os');
const path = require('path');
const { execFileSync } = require('child_process');

const repo = path.resolve(__dirname, '..', '..');
const out = path.join(repo, 'docs', 'images', 'intellij');
const samples = path.join(repo, 'samples');
const windowid = path.join(os.tmpdir(), 'atn-windowid');
const ROBOT = 'http://127.0.0.1:8082';
const PLUGIN_ID = 'com.bogdanbujor.azure-templates-navigator';
const sleep = ms => new Promise(r => setTimeout(r, ms));

// ── Robot server ────────────────────────────────────────────────────────────
// Helpers every script can use. P: the project; editor(); open(rel, line, col); action(id[, component]);
// cls(name): a class from this plugin; tab(panelClass): select a tab of the Azure Templates tool window.
const PRELUDE = `
var P = com.intellij.openapi.project.ProjectManager.getInstance().getOpenProjects()[0];
var AM = com.intellij.openapi.actionSystem.ActionManager.getInstance();
function editor() { return com.intellij.openapi.fileEditor.FileEditorManager.getInstance(P).getSelectedTextEditor(); }
function open(rel, line, col) {
  var vf = com.intellij.openapi.vfs.LocalFileSystem.getInstance().refreshAndFindFileByPath(${JSON.stringify(samples)} + "/" + rel);
  new com.intellij.openapi.fileEditor.OpenFileDescriptor(P, vf, line || 0, col || 0).navigate(true);
  var e = editor();
  e.getScrollingModel().scrollToCaret(com.intellij.openapi.editor.ScrollType.CENTER_UP);
  return e;
}
function action(id, component) {
  var c = component || editor().getContentComponent();
  AM.tryToExecute(AM.getAction(id), null, c, "screenshots", true);
}
function cls(name) {
  var plugin = com.intellij.ide.plugins.PluginManagerCore.getPlugin(com.intellij.openapi.extensions.PluginId.getId(${JSON.stringify(PLUGIN_ID)}));
  return java.lang.Class.forName("com.bogdanbujor.azuretemplates." + name, true, plugin.getPluginClassLoader());
}
var FRAME = com.intellij.openapi.wm.WindowManager.getInstance().getFrame(P);
/** Opens rel with the caret on the first occurrence of find (a string), or at line/col. */
function openAt(rel, find, line, col) {
  var e = open(rel, line || 0, col || 0);
  if (find) {
    var off = String(e.getDocument().getText()).indexOf(find);
    e.getCaretModel().moveToOffset(off);
    e.getScrollingModel().scrollToCaret(com.intellij.openapi.editor.ScrollType.CENTER);
  }
  return e;
}
/** "x y w h" of a component, in points from the IDE frame's top-left. */
function box(c) {
  var p = javax.swing.SwingUtilities.convertPoint(c, 0, 0, FRAME);
  return p.x + " " + p.y + " " + c.getWidth() + " " + c.getHeight();
}
/** "x y w h" of N editor lines from the caret line minus "above", gutter included, "width" points wide. */
function editorBox(above, lines, width, pad) {
  var e = editor();
  var line = Math.max(0, e.getCaretModel().getLogicalPosition().line - above);
  var y = e.logicalPositionToXY(new com.intellij.openapi.editor.LogicalPosition(line, 0)).y;
  var p = javax.swing.SwingUtilities.convertPoint(e.getContentComponent(), 0, y, FRAME);
  var g = javax.swing.SwingUtilities.convertPoint(e.getComponent(), 0, 0, FRAME);
  var top = Math.max(g.y, p.y - (pad || 0));
  return g.x + " " + top + " " + width + " " + (p.y - top + lines * e.getLineHeight());
}
function toolWindow(id) { return com.intellij.openapi.wm.ToolWindowManager.getInstance(P).getToolWindow(id || "Azure Templates"); }
/** The tool window, "width" points wide. */
function sideWindow(id, width) {
  var tw = toolWindow(id);
  tw.show(null);
  if (width) tw.stretchWidth(width - tw.getComponent().getWidth());
  return tw;
}
/** The first component under root whose class is called name. */
function findComponent(root, name) {
  if (root.getClass().getSimpleName() == name) return root;
  var cs = root.getComponents ? root.getComponents() : [];
  for (var i = 0; i < cs.length; i++) { var f = findComponent(cs[i], name); if (f) return f; }
  return null;
}
/** Fits the graph to its panel (its toolbar's Fit to Window). */
function fitGraph() {
  var g = findComponent(toolWindow().getComponent(), "GraphPanel");
  // JsonObject from the plugin's class loader, not this script's.
  var post = g.getClass().getDeclaredMethods().filter(function (m) { return m.getName() == "post"; })[0];
  post.setAccessible(true);
  var msg = post.getParameterTypes()[0].getConstructor().newInstance();
  msg.addProperty("type", "fit");
  post.invoke(g, msg);
}
function decorator(tw) { return tw.getDecorator ? tw.getDecorator() : tw.getComponent(); }
function tab(panel, width) {
  var tw = sideWindow(null, width);
  var tabs = tw.getContentManager().getContent(0).getComponent();
  tabs.select(cls("ui." + panel));
}
`;

async function post(endpoint, script, runInEdt) {
  const res = await fetch(ROBOT + endpoint, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ script: PRELUDE + script, runInEdt }),
  });
  const json = await res.json();
  if (json.status !== 'SUCCESS') throw new Error(`robot: ${json.message || ''} ${json.exceptionMessage || ''}\n${script}`);
  return json;
}
/** Runs a script inside the IDE (on the EDT unless edt is false). */
const ide = (script, edt = true) => post('/js/execute', script, edt);
/** Runs an expression inside the IDE and returns it as a string. */
async function ideValue(expr) {
  const { bytes } = await post('/js/retrieveAny', `new java.lang.String("" + (${expr}))`, true);
  const b = Buffer.from(bytes.map(x => x & 255)); // Java serialization of a String: AC ED 00 05 74 <u16 len> <utf8>
  return b.slice(7, 7 + b.readUInt16BE(5)).toString('utf8');
}

// ── Capture ─────────────────────────────────────────────────────────────────
let pid;
function windows() {
  return execFileSync(windowid, ['--all', String(pid)]).toString().trim().split('\n').map(l => {
    const [id, x, y, w, h, layer] = l.split(' ').map(Number);
    return { id, x, y, w, h, layer };
  }).filter(w => w.w > 40 && w.h > 40);
}

/**
 * Captures the IDE frame with every window of the IDE on top of it (popups, dialogs) composited in place,
 * then crops to `crop` ({x, y, w, h} in points from the frame's top-left; default: the whole frame, or
 * the union of the frame and its dialogs when one sticks out) and reduces it to a palette PNG.
 */
const parseBox = s => { const [x, y, w, h] = s.split(' ').map(Number); return { x, y, w, h }; };
/** A component's box ("x y w h" from the prelude's box/editorBox) as a crop. */
const boxOf = async expr => parseBox(await ideValue(expr));
/** The smallest box around all of these, plus `pad` points. */
function union(boxes, pad = 0) {
  const l = Math.min(...boxes.map(b => b.x)) - pad, t = Math.min(...boxes.map(b => b.y)) - pad;
  const r = Math.max(...boxes.map(b => b.x + b.w)) + pad, btm = Math.max(...boxes.map(b => b.y + b.h)) + pad;
  return { x: l, y: t, w: r - l, h: btm - t };
}

// Tip of the Day draws a tip's image in a column about 450pt wide: wider ones are cut off.
const MAX_WIDTH = 900;

async function capture(name, crop, settle = 800) {
  await sleep(settle);
  const all = windows();
  const frame = all.filter(w => w.layer === 0).sort((a, b) => b.w * b.h - a.w * a.h)[0];
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'atn-shot-'));
  const shot = id => { const f = path.join(tmp, `${id}.png`); execFileSync('screencapture', ['-x', '-o', '-l' + id, f]); return f; };
  const base = shot(frame.id);
  const scale = Number(execFileSync('magick', ['identify', '-format', '%w', base]).toString()) / frame.w;
  // CGWindowList is front to back: draw the others back to front, at their offset from the frame.
  const others = all.filter(w => w.id !== frame.id).reverse();
  let left = 0, top = 0, right = frame.w, bottom = frame.h;
  for (const w of others) {
    left = Math.min(left, w.x - frame.x); top = Math.min(top, w.y - frame.y);
    right = Math.max(right, w.x - frame.x + w.w); bottom = Math.max(bottom, w.y - frame.y + w.h);
  }
  const args = ['-size', `${Math.round((right - left) * scale)}x${Math.round((bottom - top) * scale)}`, 'xc:#1e1f22',
    base, '-geometry', `+${Math.round(-left * scale)}+${Math.round(-top * scale)}`, '-composite'];
  for (const w of others) {
    args.push(shot(w.id), '-geometry', `+${Math.round((w.x - frame.x - left) * scale)}+${Math.round((w.y - frame.y - top) * scale)}`, '-composite');
  }
  // crop may be a function of the other windows' boxes (popups), relative to the frame.
  if (typeof crop === 'function') crop = crop(others.map(w => ({ x: w.x - frame.x, y: w.y - frame.y, w: w.w, h: w.h })));
  const c = Object.assign({ x: left, y: top, w: right - left, h: bottom - top }, crop);
  c.x = Math.max(c.x, left); c.y = Math.max(c.y, top);
  c.w = Math.min(c.w, right - c.x); c.h = Math.min(c.h, bottom - c.y);
  const geo = `${Math.round(c.w * scale)}x${Math.round(c.h * scale)}+${Math.round((c.x - left) * scale)}+${Math.round((c.y - top) * scale)}`;
  const file = path.isAbsolute(name) ? name : path.join(out, `${name}.png`);
  execFileSync('magick', [...args, '-crop', geo, '+repage', '-resize', `${MAX_WIDTH}x>`, '-strip', '-colors', '256', file]);
  fs.rmSync(tmp, { recursive: true, force: true });
  if (!path.isAbsolute(name)) console.log('shot', name);
}

/** Polls a boolean expression inside the IDE until it is true. */
async function until(expr, ms = 20000) {
  const end = Date.now() + ms;
  while ((await ideValue(expr)) !== 'true') {
    if (Date.now() > end) throw new Error('timed out waiting for ' + expr);
    await sleep(300);
  }
}
/** True once the server's error highlighting is on the caret's line. */
const ERROR_AT_CARET = `(function () {
  var e = editor(), doc = e.getDocument(), line = e.getCaretModel().getLogicalPosition().line;
  var hs = com.intellij.codeInsight.daemon.impl.DaemonCodeAnalyzerImpl.getHighlights(doc, com.intellij.lang.annotation.HighlightSeverity.ERROR, P);
  for (var i = 0; i < hs.size(); i++) if (doc.getLineNumber(hs.get(i).getStartOffset()) == line) return true;
  return false;
})()`;

/** <name>.gif from the frames, ending on <name>.png (held longer). */
function gif(name, frames) {
  const png = path.join(out, `${name}.png`);
  execFileSync('magick', ['-delay', '90', ...frames, '-delay', '300', png, '-loop', '0', '-resize', '75%', '-fuzz', '3%', '-layers', 'Optimize', path.join(out, `${name}.gif`)]);
  for (const f of frames) fs.rmSync(f);
}

/** Closes popups and dialogs left by the previous shot and returns to one editor. */
async function reset() {
  await ide(`
    var windows = java.awt.Window.getWindows();
    for (var i = 0; i < windows.length; i++) {
      var w = windows[i];
      if (w instanceof javax.swing.RootPaneContainer) {
        var rp = w.getRootPane();
        if (rp) com.intellij.openapi.ui.popup.JBPopupFactory.getInstance().getChildPopups(rp).forEach(function (p) { p.cancel(); });
      }
      if (w instanceof javax.swing.JDialog && w.isShowing()) w.dispose();
    }
    com.intellij.openapi.fileEditor.FileEditorManager.getInstance(P).getOpenFiles().forEach(function (f) { com.intellij.openapi.fileEditor.FileEditorManager.getInstance(P).closeFile(f); });
    // The robot's own write actions log "write-unsafe context" errors: drop the red IDE-error balloon.
    com.intellij.diagnostic.MessagePool.getInstance().clearErrors();
    var nm = com.intellij.notification.NotificationsManager.getNotificationsManager();
    var notes = nm.getNotificationsOfType(com.intellij.notification.Notification, P);
    for (var n = 0; n < notes.length; n++) notes[n].expire();
    var twm = com.intellij.openapi.wm.ToolWindowManager.getInstance(P);
    twm.getToolWindowIds().forEach(function (id) { var t = twm.getToolWindow(id); if (t && t.isVisible()) t.hide(null); });
  `);
  await sleep(400);
}

// ── Shots ───────────────────────────────────────────────────────────────────
const SHOTS = {
  // The plugin's own Tips dialog, on the tip with an image.
  tipsDialog: async () => {
    await ide(`
      var d = cls("ui.TipsDialog").getConstructors()[0].newInstance(P);
      d.show();
      var show = d.getClass().getDeclaredMethod("show", java.lang.Integer.TYPE);
      show.setAccessible(true);
      show.invoke(d, new java.lang.Integer(${Number(process.env.ATN_TIP || 5)})); // ATN_TIP=<index> to check another tip; 5 is Call Hierarchy
    `);
    await sleep(500);
    await capture('tipsDialog');
  },
  // The verdict above a pipeline's first line (Code Vision).
  pipelineVerdict: async () => {
    await ide(`open("azure-pipelines.yml", 0, 0);`);
    await sleep(3000);
    await ide(`editor().getScrollingModel().scrollVertically(0);`); // the verdict sits above line 1
    await capture('pipelineVerdict', await boxOf(`editorBox(0, 9, 440, 40)`));
  },
  // Every pipeline's verdict, failing first.
  pipelinesTab: async () => {
    await ide(`open("azure-pipelines.yml", 0, 0); tab("PipelinesPanel", 440);`);
    await sleep(2500);
    const b = await boxOf(`box(decorator(toolWindow()))`);
    await capture('pipelinesTab', { ...b, h: 360 });
  },
  // My changes: a template parameter renamed (unsaved), so the pipelines calling it get new
  // errors on top of the ones they already had.
  myChanges: async () => {
    const rel = 'templates/run-tests.yml';
    const original = fs.readFileSync(path.join(samples, rel), 'utf8');
    const mode = '"azureTemplatesNavigator.pipelines.onlyMyChanges"';
    try {
      await ide(`
        com.intellij.ide.util.PropertiesComponent.getInstance(P).setValue(${mode}, true, false);
        var e = openAt(${JSON.stringify(rel)}, "- name: testProject");
        var doc = e.getDocument();
        var at = String(doc.getText()).indexOf("- name: testProject") + "- name: testProject".length;
        com.intellij.openapi.command.WriteCommandAction.runWriteCommandAction(P, new java.lang.Runnable({ run: function () { doc.insertString(at, "Path"); } }));
        tab("PipelinesPanel", 440);
      `);
      await sleep(5000);
      const b = await boxOf(`box(decorator(toolWindow()))`);
      await capture('myChanges', { ...b, h: 420 });
    } finally {
      await ide(`
        var e = open(${JSON.stringify(rel)}, 0, 0);
        com.intellij.openapi.command.WriteCommandAction.runWriteCommandAction(P, new java.lang.Runnable({ run: function () { e.getDocument().setText(${JSON.stringify(original)}); } }));
        com.intellij.openapi.fileEditor.FileDocumentManager.getInstance().saveDocument(e.getDocument());
        com.intellij.ide.util.PropertiesComponent.getInstance(P).setValue(${mode}, false, false);
      `);
    }
  },
  // Quick Documentation on a template call: its parameters.
  hoverParameters: async () => {
    await ide(`openAt("azure-pipelines.yml", "templates/build-dotnet.yml");`);
    await sleep(1500);
    const line = await boxOf(`editorBox(1, 3, 440, 0)`);
    await ide(`action("QuickJavaDoc");`);
    await capture('hoverParameters', popups => union([line, ...popups], 4), 2500);
  },
  // Completing a template path inserts its required parameters.
  completeRequired: async () => {
    const rel = 'azure-pipelines.yml';
    const original = fs.readFileSync(path.join(samples, rel), 'utf8');
    const frames = [];
    const frame = async (ms, crop) => { const f = path.join(os.tmpdir(), `atn-ij-frame-${frames.length}.png`); await capture(f, crop, ms); frames.push(f); };
    try {
      await ide(`
        var e = openAt(${JSON.stringify(rel)}, "- template: templates/run-tests.yml");
        var doc = e.getDocument();
        var line = doc.getLineNumber(e.getCaretModel().getOffset());
        var start = doc.getLineStartOffset(line);
        var indent = String(doc.getText()).substring(start, e.getCaretModel().getOffset());
        com.intellij.openapi.command.WriteCommandAction.runWriteCommandAction(P, new java.lang.Runnable({ run: function () {
          doc.insertString(start, indent + "- template: templates/noti\\n");
        } }));
        e.getCaretModel().moveToOffset(start + indent.length + "- template: templates/noti".length);
      `);
      const crop = await boxOf(`editorBox(2, 9, 440, 0)`);
      await frame(800, crop);
      // Completion closes its popup when the editor loses focus, so this one shot brings the IDE to the front.
      await ide(`java.awt.Desktop.getDesktop().requestForeground(true); FRAME.toFront();
        com.intellij.openapi.wm.IdeFocusManager.getInstance(P).requestFocus(editor().getContentComponent(), true);`);
      await sleep(800);
      await ide(`new com.intellij.codeInsight.completion.CodeCompletionHandlerBase(com.intellij.codeInsight.completion.CompletionType.BASIC).invokeCompletion(P, editor());`);
      await until(`com.intellij.codeInsight.lookup.LookupManager.getActiveLookup(editor()) != null`);
      await frame(1500, crop);
      await ide(`
        var lookup = com.intellij.codeInsight.lookup.LookupManager.getActiveLookup(editor());
        if (lookup) lookup.finishLookup(com.intellij.codeInsight.lookup.Lookup.NORMAL_SELECT_CHAR);
      `);
      await capture('completeRequired', crop, 1500);
      gif('completeRequired', frames);
    } finally {
      await ide(`
        var e = open(${JSON.stringify(rel)}, 0, 0);
        var state = com.intellij.codeInsight.template.impl.TemplateManagerImpl.getTemplateState(e);
        if (state) state.gotoEnd(true);
        com.intellij.openapi.command.WriteCommandAction.runWriteCommandAction(P, new java.lang.Runnable({ run: function () { e.getDocument().setText(${JSON.stringify(original)}); } }));
        com.intellij.openapi.fileEditor.FileDocumentManager.getInstance().saveDocument(e.getDocument());
      `);
    }
  },
  // A quick fix: "did you mean".
  quickFixes: async () => {
    await ide(`openAt("extends/teams/search/azure-pipelines.yml", "dependson:");`);
    await until(ERROR_AT_CARET);
    await sleep(1000);
    const line = await boxOf(`editorBox(3, 6, 440, 0)`);
    await ide(`action("ShowIntentionActions");`);
    await capture('quickFixes', popups => union([line, ...popups]), 2500);
  },
  // Find Usages on a template.
  findUsages: async () => {
    await ide(`openAt("extends/platform-templates/pipelines/secure-base.yml", "parameters:");`);
    await sleep(2000);
    const line = await boxOf(`editorBox(1, 2, 440, 30)`);
    await ide(`action("ShowUsages");`);
    await capture('findUsages', popups => union([line, ...popups]), 2500);
  },
  // Call Hierarchy: every pipeline that reaches a template.
  callHierarchy: async () => {
    await ide(`openAt("extends/platform-templates/pipelines/secure-base.yml", "parameters:");`);
    await sleep(1500);
    await ide(`action("CallHierarchy");`);
    await sleep(2500);
    await ide(`sideWindow("Hierarchy", 440);`);
    const b = await boxOf(`box(decorator(toolWindow("Hierarchy")))`);
    await capture('callHierarchy', { ...b, h: 300 }, 1500);
  },
  // The Structure popup: stages, jobs, steps and template calls.
  structure: async () => {
    await ide(`openAt("extends/teams/search/azure-pipelines.yml", "trigger:");`);
    await sleep(1500);
    await ide(`action("FileStructurePopup");`);
    await capture('structure', popups => union(popups.map(b => ({ ...b, w: Math.min(b.w, 440), h: Math.min(b.h, 440) }))), 2000);
  },
  // The tool window's tabs.
  dependencies: async () => {
    await ide(`open("azure-pipelines.yml", 0, 0); tab("DependenciesPanel", 440);`);
    await capture('dependencies', { ...(await boxOf(`box(decorator(toolWindow()))`)), h: 360 }, 2500);
  },
  templateGraph: async () => {
    await ide(`open("extends/platform-templates/pipelines/secure-base.yml", 0, 0); tab("GraphPanel", 820);`);
    // A wide, short tool window for this one: the graph lays out across, and fits a tip-sized image.
    await ide(`FRAME.setBounds(60, 60, 1280, 560);`);
    try {
      await sleep(3500);
      await ide(`fitGraph();`);
      await capture('templateGraph', await boxOf(`box(decorator(toolWindow()))`), 1500);
    } finally {
      await ide(`FRAME.setBounds(60, 60, 1280, 800);`);
    }
  },
  diagnostics: async () => {
    await ide(`open("azure-pipelines.yml", 0, 0); tab("DiagnosticsPanel", 440);`);
    await capture('diagnostics', { ...(await boxOf(`box(decorator(toolWindow()))`)), h: 360 }, 2500);
  },
  otherRepositories: async () => {
    await ide(`open("cross-repo-pipeline.yml", 0, 0); tab("RepositoriesPanel", 440);`);
    await capture('otherRepositories', { ...(await boxOf(`box(decorator(toolWindow()))`)), h: 300 }, 2500);
  },
  stats: async () => {
    await ide(`open("azure-pipelines.yml", 0, 0); tab("StatsPanel", 440);`);
    await capture('stats', { ...(await boxOf(`box(decorator(toolWindow()))`)), h: 440 }, 2500);
  },
  // The help button in the tool window header: Get Started, What's New, report a bug...
  helpMenu: async () => {
    await ide(`open("azure-pipelines.yml", 0, 0); tab("DependenciesPanel", 440); action("AzureTemplatesNavigator.ReportIssue", toolWindow().getComponent());`);
    await capture('helpMenu', popups => union(popups, 6), 1500);
  },
  // IntelliJ's Tip of the Day, showing only this plugin's tips (from this script only: the API is internal).
  tipOfTheDay: async () => {
    await ide(`
      var beans = new java.util.ArrayList();
      com.intellij.ide.util.TipAndTrickBean.EP_NAME.getExtensionList().forEach(function (b) {
        if (b.getPluginDescriptor() && b.getPluginDescriptor().getPluginId().getIdString() == ${JSON.stringify(PLUGIN_ID)}) beans.add(b);
      });
      // Start on a tip with a screenshot.
      beans.sort(function (a, b) { return (b.fileName.indexOf("templateGraph") >= 0 ? 1 : 0) - (a.fileName.indexOf("templateGraph") >= 0 ? 1 : 0); });
      var ctor = java.lang.Class.forName("com.intellij.ide.util.TipDialog").getDeclaredConstructors()[0];
      ctor.setAccessible(true);
      var dialog = ctor.newInstance(P, com.intellij.ide.util.TipsSortingResult.create(beans));
      dialog.show();
    `);
    await capture('tipOfTheDay');
  },
};

(async () => {
  if (process.platform !== 'darwin') throw new Error('Screenshots need macOS (screencapture).');
  if (!fs.existsSync(windowid)) execFileSync('swiftc', ['-O', path.join(__dirname, 'windowid.swift'), '-o', windowid]);
  fs.mkdirSync(out, { recursive: true });
  pid = Number(await ideValue('java.lang.ProcessHandle.current().pid()'));
  // A fixed frame size, so shots and crops are the same on every machine.
  await ide(`com.intellij.openapi.wm.WindowManager.getInstance().getFrame(P).setBounds(60, 60, 1280, 800);`);
  const only = process.argv.slice(2);
  for (const [name, take] of Object.entries(SHOTS)) {
    if (only.length && !only.includes(name)) continue;
    await reset();
    await take();
  }
  await reset();
})().catch(err => { console.error(err.message); process.exit(1); });
