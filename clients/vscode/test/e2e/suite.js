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

  // 3b. accepting a template path completion also adds the required parameters,
  //     lined up under `template:` (VS Code must not re-indent them). Edited in memory, then reverted.
  {
    const editor = vscode.window.activeTextEditor;
    const at = doc.getText().split('\n').findIndex(l => l.includes('- template: templates/notify-teams.yml'));
    const indent = doc.lineAt(at).text.match(/^\s*/)[0];
    const typed = `${indent}- template: templates/notify-te`;
    await editor.edit(e => e.insert(new vscode.Position(at, 0), typed + '\n'));
    editor.selection = new vscode.Selection(at, typed.length, at, typed.length);
    await vscode.commands.executeCommand('editor.action.triggerSuggest');
    await sleep(1500);
    await vscode.commands.executeCommand('acceptSelectedSuggestion');
    await sleep(500);
    const got = [0, 1, 2].map(i => doc.lineAt(at + i).text);
    console.log('accepted completion:', JSON.stringify(got));
    const key = ' '.repeat(indent.length + 2);
    assert.deepStrictEqual(got, [`${indent}- template: templates/notify-teams.yml`, `${key}parameters:`, `${key}  message: ''`]);
    await vscode.commands.executeCommand('workbench.action.files.revert');
  }

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
  for (const c of ['searchTemplates', 'searchParameters', 'fetchRepository', 'fetchAllRepositories', 'refresh', 'expandTemplateGraph', 'showUsages', 'reportIssue', 'giveFeedback', 'showPipelineProblems']) {
    assert.ok(all.includes(`azure-templates-navigator.${c}`), c);
  }
  // 6b. Report Issue: a pre-filled GitHub URL with versions and nothing local
  const issue = await vscode.commands.executeCommand('azure-templates-navigator.reportIssue', { kind: 'bug', open: false });
  const q = new URL(issue).searchParams;
  console.log('issue url:', issue.length, 'chars;', q.get('body').split('\n').filter(l => l.startsWith('- ')).join(' | '));
  console.log('issue url survives Uri.parse:', vscode.Uri.parse(issue).toString() === issue);
  assert.ok(issue.startsWith('https://github.com/qSharpy/azure-templates-navigator/issues/new?'));
  assert.strictEqual(q.get('labels'), 'bug');
  assert.ok(q.get('body').includes('### What happened?'));
  assert.ok(/- atn \d+\.\d+\.\d+/.test(q.get('body')), 'atn version from the running server');
  assert.ok(!q.get('body').includes(root), 'no workspace paths in the issue');
  const feature = new URL(await vscode.commands.executeCommand('azure-templates-navigator.reportIssue', { kind: 'feature', open: false }));
  assert.strictEqual(feature.searchParams.get('labels'), 'enhancement');
  const review = await vscode.commands.executeCommand('azure-templates-navigator.giveFeedback', { open: false });
  assert.ok(review.startsWith('https://marketplace.visualstudio.com/items?itemName=bogdanbujor.azure-templates-navigator'));
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

  // 8. Stats view (atn/stats) and Show in Graph (atn/graph filters)
  const api = ext.exports;
  for (const c of ['showInGraph']) assert.ok(all.includes(`azure-templates-navigator.${c}`), c);
  const sections = await waitFor(() => api.request('atn/stats'));
  console.log('stats:', sections.map(s => `${s.id}=${s.rows.length}`).join(', '));
  const byId = Object.fromEntries(sections.map(s => [s.id, s]));
  for (const id of ['overview', 'problemsByType', 'mostUsed', 'misCalled', 'fanOut', 'unused', 'repositories']) assert.ok(byId[id], id);
  assert.ok(byId.mostUsed.rows.length > 0 && byId.mostUsed.rows[0].focus === 'file');
  assert.ok(byId.unused.rows.some(r => r.filePath.endsWith('stale-template.yml')), 'stale-template.yml is unused');
  const roots = await api.stats.getChildren();
  assert.strictEqual(roots.length, sections.length);
  const usedRows = await api.stats.getChildren(roots.find(r => r.section.id === 'mostUsed'));
  const usedItem = api.stats.getTreeItem(usedRows[0]);
  assert.strictEqual(usedItem.command.command, 'azure-templates-navigator.openTemplate');
  assert.strictEqual(usedItem.contextValue, 'stat-file');

  // Show in Graph on a template: the graph is scoped to it, nodes carry size and tooltip.
  const top = usedRows[0].row;
  await vscode.commands.executeCommand('azure-templates-navigator.showInGraph', usedRows[0]);
  const fg = await waitFor(() => {
    const l = api.graph.lastGraph;
    return l && l.scopedFile === top.filePath ? l : null;
  });
  const focal = fg.nodes.find(n => n.filePath === top.filePath);
  console.log('graph focus:', top.label, `weight ${focal.weight},`, `${fg.nodes.length}/${fg.totalNodes} nodes`);
  assert.ok(focal.weight > 0 && focal.callers > 0 && focal.tooltip.includes('Used by'), 'node size and tooltip');

  // By repository: the workspace graph filtered to that repository (callers faded).
  const repoRow = byId.repositories.rows[0];
  await vscode.commands.executeCommand('azure-templates-navigator.showInGraph', { row: repoRow });
  const rg = await waitFor(() => {
    const l = api.graph.lastGraph;
    return l && !l.scopedFile && l.filter.repos[0] === repoRow.repo ? l : null;
  });
  console.log('graph repo filter:', repoRow.repo, `${rg.nodes.length}/${rg.totalNodes} nodes`);
  assert.ok(rg.nodes.length > 0 && rg.nodes.length < rg.totalNodes);
  assert.ok(rg.nodes.every(n => n.context || n.repoName === repoRow.repo));
  assert.ok(rg.facets.repos.length > 1);

  // Errors: problems only.
  const errRow = byId.overview.rows.find(r => r.kind === 'error');
  await vscode.commands.executeCommand('azure-templates-navigator.showInGraph', { row: errRow });
  const pg = await waitFor(() => {
    const l = api.graph.lastGraph;
    return l && l.filter.problemsOnly ? l : null;
  });
  console.log('graph problems:', `${pg.nodes.length}/${pg.totalNodes} nodes, facet ${pg.facets.problems}`);
  assert.ok(pg.nodes.filter(n => !n.context).every(n => n.broken || n.errors || n.warnings));
  assert.ok(pg.nodes.some(n => n.broken), 'broken references are kept');

  // 9. pipeline verdict lens on a root pipeline
  const verdict = await waitFor(async () => {
    const l = await vscode.commands.executeCommand('vscode.executeCodeLensProvider', uri);
    return l && l.length ? l : null;
  });
  console.log('verdict lens:', verdict[0].command.title);
  assert.ok(verdict[0].command.title.startsWith('✗ Will fail when queued'), 'verdict lens');
  assert.strictEqual(verdict[0].command.command, 'azure-templates-navigator.showPipelineProblems');

  await vscode.commands.executeCommand('workbench.view.extension.azure-templates-navigator');
  await sleep(1500);
  console.log('E2E OK');
}

module.exports = { run };
