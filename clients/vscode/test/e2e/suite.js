'use strict';
const assert = require('assert');
const path = require('path');
const vscode = require('vscode');

const sleep = ms => new Promise(r => setTimeout(r, ms));
async function waitFor(fn, ms = 20000) {
  const end = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() > end) throw new Error('timeout');
    await sleep(200);
  }
}

async function run() {
  const root = vscode.workspace.workspaceFolders[0].uri.fsPath;
  const ext = vscode.extensions.getExtension('bogdanbujor.azure-templates-navigator');
  await ext.activate();
  const uri = vscode.Uri.file(path.join(root, 'azure-pipelines.yml'));
  const doc = await vscode.workspace.openTextDocument(uri);
  await vscode.window.showTextDocument(doc);

  // 1. diagnostics published by the server
  const diags = await waitFor(() => {
    const d = vscode.languages.getDiagnostics(uri);
    return d.length ? d : null;
  });
  const codes = diags.map(d => String(d.code));
  console.log('diagnostics:', codes.join(', '));
  assert.ok(codes.includes('missing-required-param'));

  // 2. hover on a template line
  const tplLine = doc.getText().split('\n').findIndex(l => l.includes('template: templates/build-dotnet.yml'));
  const hovers = await vscode.commands.executeCommand('vscode.executeHoverProvider', uri, new vscode.Position(tplLine, 16));
  const md = hovers.map(h => h.contents.map(c => c.value || c).join('')).join('');
  console.log('hover:', md.slice(0, 120).replace(/\n/g, ' '));
  assert.ok(md.includes('Parameters'));
  assert.ok(md.includes('azure-templates-navigator.searchParameters'), 'hover has parameter search link');

  // 3. completion inside a parameters block
  const comp = await vscode.commands.executeCommand('vscode.executeCompletionItemProvider', uri, new vscode.Position(tplLine + 2, 14));
  console.log('completions:', comp.items.slice(0, 5).map(i => typeof i.label === 'string' ? i.label : i.label.label).join(', '));
  assert.ok(comp.items.length > 0);

  // 4. quick fixes for a diagnostic
  const missing = diags.find(d => d.code === 'missing-required-param');
  const actions = await vscode.commands.executeCommand('vscode.executeCodeActionProvider', uri, missing.range);
  console.log('actions:', actions.map(a => a.title).join(' | '));
  assert.ok(actions.some(a => a.title.startsWith('Add missing parameter')));

  // 5. go to definition
  const defs = await vscode.commands.executeCommand('vscode.executeDefinitionProvider', uri, new vscode.Position(tplLine, 16));
  assert.ok(defs.length === 1);

  // 6. commands registered
  const all = await vscode.commands.getCommands(true);
  for (const c of ['searchTemplates', 'searchParameters', 'fetchRepository', 'fetchAllRepositories', 'refresh', 'expandTemplateGraph', 'showUsages']) {
    assert.ok(all.includes(`azure-templates-navigator.${c}`), c);
  }
  // 7. navigation on an extends: template — usages lens, references, outline
  const base = vscode.Uri.file(path.join(root, 'extends/platform-templates/pipelines/secure-base.yml'));
  await vscode.workspace.openTextDocument(base);
  const lenses = await waitFor(async () => {
    const l = await vscode.commands.executeCommand('vscode.executeCodeLensProvider', base);
    return l && l.length ? l : null;
  });
  console.log('code lens:', lenses[0].command.title);
  assert.ok(/^\d+ usages/.test(lenses[0].command.title), 'usage lens');
  assert.strictEqual(lenses[0].command.command, 'azure-templates-navigator.showUsages');
  const refs = await vscode.commands.executeCommand('vscode.executeReferenceProvider', base, new vscode.Position(0, 0));
  console.log('references:', refs.length);
  assert.ok(refs.some(r => r.uri.fsPath.endsWith(path.join('teams', 'search', 'azure-pipelines.yml'))), 'search team extends secure-base');
  const syms = await vscode.commands.executeCommand('vscode.executeDocumentSymbolProvider', base);
  console.log('symbols:', syms.map(s => s.name).join(', '));
  assert.ok(syms.some(s => s.name === 'parameters' && s.children.some(c => c.name === 'stages')), 'outline has parameters');

  await vscode.commands.executeCommand('workbench.view.extension.azure-templates-navigator');
  await sleep(1500);
  console.log('E2E OK');
}

module.exports = { run };
