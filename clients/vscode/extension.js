'use strict';

// Azure Templates Navigator — VS Code client.
//
// This extension is a thin shell around the `atn` language server (server/
// in the monorepo). Every piece of logic — parsing, resolution, diagnostics,
// hover, completion, quick fixes, repository download, tree/graph data —
// lives in the server so the VS Code and IntelliJ plugins behave identically.
// This file only wires the server to VS Code UI.

const fs = require('fs');
const os = require('os');
const path = require('path');
const vscode = require('vscode');
const { LanguageClient, State } = require('vscode-languageclient/node');
const { DependencyTreeProvider, DiagnosticsTreeProvider, PipelinesTreeProvider, RepositoriesTreeProvider, StatsTreeProvider, FeedbackTreeProvider } = require('./src/views');
const { GraphViewProvider } = require('./src/graphView');
const { issueUrl, CHANGELOG_URL, REVIEW_URL } = require('./src/issue');

const SECTION = 'azure-templates-navigator';

/** @type {LanguageClient | undefined} */
let client;
/** @type {vscode.OutputChannel} */
let output;

function serverPath(context) {
  const custom = vscode.workspace.getConfiguration(SECTION).get('server.path');
  if (custom) return custom;
  const exe = process.platform === 'win32' ? 'atn.exe' : 'atn';
  const target = `${process.platform}-${process.arch}`;
  const candidates = [
    path.join(context.extensionPath, 'bin', target, exe),
    path.join(context.extensionPath, 'bin', exe),
  ];
  return candidates.find(p => fs.existsSync(p)) || candidates[0];
}

/** Settings sent to the server — same shape the IntelliJ plugin sends. */
function serverSettings() {
  const c = vscode.workspace.getConfiguration(SECTION);
  return {
    requiredParameterColor: c.get('requiredParameterColor'),
    diagnostics: {
      enabled: c.get('diagnostics.enabled'),
      unusedParameters: c.get('diagnostics.unusedParameters'),
    },
    graph: { rootPath: c.get('graph.rootPath') },
    changes: {
      enabled: c.get('changes.enabled'),
      dimExistingProblems: c.get('changes.dimExistingProblems'),
    },
    repositories: {
      autoFetch: c.get('repositories.autoFetch'),
      cacheDir: c.get('repositories.cacheDir'),
      refreshIntervalHours: c.get('repositories.refreshIntervalHours'),
      preferLocalClones: c.get('repositories.preferLocalClones'),
      urls: c.get('repositories.urls'),
      localPaths: c.get('repositories.localPaths'),
      azureDevOpsOrganization: c.get('repositories.azureDevOpsOrganization'),
      azureDevOpsProject: c.get('repositories.azureDevOpsProject'),
      githubEnterpriseHost: c.get('repositories.githubEnterpriseHost'),
      gitPath: c.get('repositories.gitPath'),
    },
  };
}

async function startClient(context, onServerChange) {
  const command = serverPath(context);
  if (!fs.existsSync(command)) {
    vscode.window.showErrorMessage(
      `Azure Templates Navigator: language server not found for ${process.platform}-${process.arch} (${command}).`);
    return;
  }
  try { fs.chmodSync(command, 0o755); } catch { /* read-only install */ }

  client = new LanguageClient(
    'azureTemplatesNavigator',
    'Azure Templates Navigator',
    { command, args: ['lsp'] },
    {
      documentSelector: [{ language: 'yaml' }, { pattern: '**/*.{yml,yaml}' }],
      outputChannel: output,
      markdown: { isTrusted: true, supportHtml: true },
      middleware: {
        // vscode-languageclient never sets supportThemeIcons, so `$(icon)` in
        // server hovers would render as literal text without this.
        async provideHover(document, position, token, next) {
          const hover = await next(document, position, token);
          for (const c of (hover && hover.contents) || []) {
            if (c instanceof vscode.MarkdownString) c.supportThemeIcons = true;
          }
          return hover;
        },
      },
      initializationOptions: {
        settings: serverSettings(),
        client: { commandLinks: true, commandPrefix: `${SECTION}.` },
      },
      synchronize: {
        fileEvents: vscode.workspace.createFileSystemWatcher('**/{*.yml,*.yaml,.azure-templates-navigator.json}'),
      },
    },
  );
  client.onNotification('atn/didChange', () => onServerChange());
  await client.start();
}

/**
 * Once per version: after an update, offer What's New; when no version was
 * recorded yet (a new install, or an update from 2.6 or older), offer the
 * walkthrough. Never awaited: the promise only settles when the
 * notification is dismissed.
 * @param {vscode.ExtensionContext} context
 */
function announceVersion(context) {
  const version = context.extension.packageJSON.version;
  const seen = context.globalState.get('lastVersion');
  if (!version || seen === version) return;
  context.globalState.update('lastVersion', version);
  if (context.extensionMode === vscode.ExtensionMode.Test) return;
  const actions = seen ? ["What's New", 'Get Started'] : ['Get Started'];
  const message = seen
    ? `Azure Templates Navigator was updated to ${version}.`
    : `Azure Templates Navigator ${version}: Get Started shows what it can do, one feature at a time.`;
  vscode.window.showInformationMessage(message, ...actions).then(pick => {
    if (pick === 'Get Started') vscode.commands.executeCommand(`${SECTION}.getStarted`);
    else if (pick === "What's New") vscode.commands.executeCommand(`${SECTION}.whatsNew`);
  });
}

/**
 * Called once when the extension is first activated.
 * @param {vscode.ExtensionContext} context
 */
async function activate(context) {
  output = vscode.window.createOutputChannel('Azure Templates Navigator');
  context.subscriptions.push(output);

  const request = async (method, params) => {
    if (!client || client.state !== State.Running) return undefined;
    try {
      return await client.sendRequest(method, params || {});
    } catch (err) {
      output.appendLine(`[${method}] ${err && err.message}`);
      return undefined;
    }
  };
  const execute = (command, args) => request('workspace/executeCommand', { command, arguments: args || [] });

  const tree = new DependencyTreeProvider(request);
  // The Pipelines and Diagnostics views share one mode: 'all' (everything)
  // or 'changes' (what your work changed), remembered per workspace.
  const mode = context.workspaceState.get('pipelinesMode', 'all');
  const diagnostics = new DiagnosticsTreeProvider(request, mode);
  const pipelines = new PipelinesTreeProvider(request, mode);
  const setPipelinesMode = async mode => {
    pipelines.setMode(mode);
    diagnostics.setMode(mode);
    await context.workspaceState.update('pipelinesMode', mode);
    await vscode.commands.executeCommand('setContext', `${SECTION}.pipelinesMode`, mode);
  };
  vscode.commands.executeCommand('setContext', `${SECTION}.pipelinesMode`, pipelines.mode);
  const repositories = new RepositoriesTreeProvider(request);
  const stats = new StatsTreeProvider(request);
  const graph = new GraphViewProvider(context, request);

  let pending;
  const refreshViews = () => {
    clearTimeout(pending);
    pending = setTimeout(() => {
      tree.refresh();
      diagnostics.refresh();
      pipelines.refresh();
      repositories.refresh();
      stats.refresh();
      graph.refresh();
    }, 250);
  };

  const treeView = vscode.window.createTreeView(`${SECTION}.templateTree`, { treeDataProvider: tree, showCollapseAll: true });
  const diagView = vscode.window.createTreeView(`${SECTION}.diagnosticsPanel`, { treeDataProvider: diagnostics });
  const pipelinesView = vscode.window.createTreeView(`${SECTION}.pipelinesView`, { treeDataProvider: pipelines, showCollapseAll: true });
  const repoView = vscode.window.createTreeView(`${SECTION}.repositoriesView`, { treeDataProvider: repositories });
  const statsView = vscode.window.createTreeView(`${SECTION}.statsView`, { treeDataProvider: stats, showCollapseAll: true });
  diagnostics.onCounts = ({ mode, label, errors, warnings }) => {
    const n = errors + warnings;
    diagView.description = mode !== 'changes' ? '' : label ? `since ${label} · ` + (n ? `${n} new` : 'no new problems') : 'my changes';
    diagView.badge = n > 0
      ? { value: n, tooltip: `${errors} ${mode === 'changes' ? 'new ' : ''}error(s), ${warnings} ${mode === 'changes' ? 'new ' : ''}warning(s)` }
      : undefined;
  };
  // Next to the view title. All: "2 of 7 will fail", the badge counts
  // failing pipelines. My changes: "since main · 2 new errors", the badge
  // counts new errors.
  pipelines.onSummary = ({ mode, reports, changes }) => {
    if (mode === 'changes') {
      const c = changes && changes.available ? changes : null;
      const n = c ? c.newErrors + c.newWarnings : 0;
      pipelinesView.description = !c ? 'my changes' : `since ${c.label} · ` + (n ? `${n} new problem${n === 1 ? '' : 's'}` : 'no new problems');
      pipelinesView.badge = c && c.newErrors ? { value: c.newErrors, tooltip: `${c.newErrors} new error(s) since ${c.label}` } : undefined;
      return;
    }
    const failing = (reports || []).filter(r => r.verdict === 'fail').length;
    const warned = (reports || []).filter(r => r.verdict === 'warn').length;
    const fresh = (reports || []).reduce((n, r) => n + (r.change ? r.change.new : 0), 0);
    pipelinesView.description = (!reports ? '' : failing ? `${failing} of ${reports.length} will fail`
      : warned ? `${warned} of ${reports.length} may fail` : reports.length ? `all ${reports.length} OK` : '')
      + (fresh ? ` · +${fresh} new` : '');
    pipelinesView.badge = failing ? { value: failing, tooltip: `${failing} pipeline(s) will fail when queued` } : undefined;
  };
  tree.onTitle = title => { treeView.description = title; };
  context.subscriptions.push(vscode.window.registerTreeDataProvider(`${SECTION}.feedbackView`, new FeedbackTreeProvider()));
  context.subscriptions.push(treeView, diagView, pipelinesView, repoView, statsView,
    vscode.window.registerWebviewViewProvider(`${SECTION}.graphView`, graph, { webviewOptions: { retainContextWhenHidden: true } }));

  const onEditor = editor => {
    const doc = editor && editor.document;
    if (doc && doc.uri.scheme === 'file' && /\.ya?ml$/i.test(doc.fileName)) {
      tree.setFile(doc.uri);
      graph.setFile(doc.uri);
    }
  };
  context.subscriptions.push(vscode.window.onDidChangeActiveTextEditor(onEditor));

  // ── Commands ────────────────────────────────────────────────────────────
  const openFile = async (filePath, { beside = false, line } = {}) => {
    if (!filePath) return;
    const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(filePath));
    const editor = await vscode.window.showTextDocument(doc, beside ? vscode.ViewColumn.Beside : vscode.ViewColumn.Active);
    if (typeof line === 'number' && line > 0) {
      const pos = new vscode.Position(line, 0);
      editor.selection = new vscode.Selection(pos, pos);
      editor.revealRange(new vscode.Range(pos, pos), vscode.TextEditorRevealType.InCenter);
    }
  };
  const nodeArg = a => (a && (a.node || a.row)) || a || {};

  const commands = {
    openTemplate: (a = {}) => openFile(a.filePath, { beside: !!a.beside, line: a.line }),
    openTemplateBeside: a => openFile(nodeArg(a).filePath, { beside: true }),
    goToReference: a => { const n = nodeArg(a); return openFile(n.refFile, { line: n.refLine }); },
    copyTemplatePath: async a => {
      const p = nodeArg(a).filePath;
      if (p) { await vscode.env.clipboard.writeText(p); vscode.window.setStatusBarMessage(`Copied ${p}`, 2000); }
    },
    searchTemplates: async () => {
      const items = (await request('atn/templates')) || [];
      const pick = await vscode.window.showQuickPick(items.map(t => ({
        label: `$(${t.kind === 'pipeline' ? 'play' : 'file'}) ${t.label}`,
        description: t.relativePath,
        detail: `${t.paramCount} parameter(s), ${t.requiredCount} required · used by ${t.callerCount} file(s)`,
        filePath: t.filePath,
      })), { placeHolder: 'Search templates and pipelines…', matchOnDescription: true });
      if (pick) await openFile(pick.filePath);
    },
    searchParameters: async (a = {}) => {
      // From the hover link: {filePath} of the template. From the palette:
      // the template referenced on the cursor line, or the active file itself.
      const ed = vscode.window.activeTextEditor;
      const params = a.filePath
        ? { uri: vscode.Uri.file(a.filePath).toString() }
        : ed ? { uri: ed.document.uri.toString(), line: ed.selection.active.line } : null;
      if (!params) return;
      const res = await request('atn/parameters', params);
      if (!res || !res.parameters || !res.parameters.length) {
        vscode.window.showInformationMessage('No template parameters found here.');
        return;
      }
      const pick = await vscode.window.showQuickPick(res.parameters.map(p => ({
        label: `$(${p.required ? 'circle-filled' : 'circle-outline'}) ${p.name}`,
        description: p.type + (p.default ? ` = ${p.default}` : ''),
        detail: (p.required ? 'required' : 'optional') + (p.values && p.values.length ? ` · one of ${p.values.join(', ')}` : ''),
        line: p.line,
      })), { placeHolder: 'Search template parameters…', matchOnDescription: true, matchOnDetail: true });
      if (pick) await openFile(res.filePath, { line: pick.line });
    },
    // Code lens on a pipeline ("✗ Will fail when queued: …") or the palette:
    // every problem in the pipeline and the templates it uses.
    showPipelineProblems: async (a = {}) => {
      const ed = vscode.window.activeTextEditor;
      const uri = a.uri || (ed && ed.document.uri.toString());
      if (!uri) return;
      const rep = await request('atn/pipelineCheck', { uri });
      if (!rep) {
        vscode.window.showInformationMessage('This file is a template, not a pipeline. Open the pipeline that uses it to check it.');
        return;
      }
      if (!rep.problems.length) {
        vscode.window.showInformationMessage(`${rep.relativePath}: ${rep.title.replace(/^\S+ /, '')} (${rep.templates} template(s) checked).`);
        return;
      }
      const pick = await vscode.window.showQuickPick(rep.problems.map(p => ({
        label: `$(${p.severity === 1 ? 'error' : 'warning'}) ${p.relativePath}:${p.range.start.line + 1}`,
        description: p.code,
        detail: p.message,
        problem: p,
      })), { placeHolder: `${rep.relativePath} — ${rep.title}`, matchOnDescription: true, matchOnDetail: true });
      if (!pick) return;
      const r = pick.problem.range;
      await vscode.window.showTextDocument(vscode.Uri.parse(pick.problem.uri), {
        selection: new vscode.Range(r.start.line, r.start.character, r.end.line, r.end.character),
      });
    },
    // Pipelines view title: switch between every pipeline and your changes.
    showMyChanges: () => setPipelinesMode('changes'),
    showAllPipelines: () => setPipelinesMode('all'),
    // What "my changes" are compared with: automatic, uncommitted only, a
    // branch, or any ref typed in. Stored per branch (git config).
    chooseChangesBase: async () => {
      const choices = await request('atn/changeBases', {});
      if (!choices || !choices.length) {
        vscode.window.showInformationMessage('Changes are compared with a git commit: open a folder that is a git repository.');
        return;
      }
      const other = { label: '$(edit) Another branch, tag or commit…', ref: undefined };
      const pick = await vscode.window.showQuickPick([
        ...choices.map(c => ({
          label: `${c.picked ? '$(check) ' : ''}${c.ref === '' ? '$(sparkle) ' : c.ref === 'HEAD' ? '$(git-commit) ' : '$(git-branch) '}${c.label}`,
          detail: c.detail, ref: c.ref,
        })),
        other,
      ], { placeHolder: 'Compare my changes with…', matchOnDetail: true });
      if (!pick) return;
      let ref = pick.ref;
      if (pick === other) {
        ref = await vscode.window.showInputBox({ prompt: 'A branch, tag or commit to compare with', placeHolder: 'origin/main, feature/story, 3f2a1c4…' });
        if (!ref) return;
      }
      try {
        await client.sendRequest('atn/setChangesBase', { ref });
      } catch (err) {
        vscode.window.showErrorMessage(`Can't compare with ${ref}: ${err && err.message}`);
        return;
      }
      await setPipelinesMode('changes');
      refreshViews();
    },
    // Code lens "N usages" above a template: peek its call sites.
    showUsages: async (a = {}) => {
      if (!a.uri) return;
      await vscode.commands.executeCommand('editor.action.findReferences',
        vscode.Uri.parse(a.uri), new vscode.Position(a.line || 0, a.character || 0));
    },
    refresh: async () => { await execute('atn.refresh'); refreshViews(); },
    expandTemplateGraph: () => graph.openPanel(),
    // From a Stats row (or a tree node): scope the graph to a file, or filter
    // the workspace graph by repository or to problems.
    showInGraph: async a => {
      const row = nodeArg(a);
      if (row.focus === 'repo') graph.showFiltered({ repos: [row.repo] });
      else if (row.focus === 'problems') graph.showFiltered({ problemsOnly: true });
      else if (row.focus === 'kind') graph.showFiltered({ kinds: row.kinds || [] });
      else if (row.filePath) graph.focusFile(row.filePath);
      else return;
      await vscode.commands.executeCommand(`${SECTION}.graphView.focus`);
    },
    fetchRepository: async a => {
      const arg = nodeArg(a);
      const alias = arg.alias || (arg.repo && arg.repo.alias);
      const file = arg.file || arg.refFile || (arg.repo && arg.repo.declaredInPath);
      if (!alias) return;
      await execute('atn.fetchRepository', [{ alias, file }]);
      vscode.window.setStatusBarMessage(`$(sync~spin) Fetching @${alias}…`, 4000);
      refreshViews();
    },
    fetchAllRepositories: async () => {
      const r = await execute('atn.fetchAllRepositories');
      vscode.window.setStatusBarMessage(`$(sync~spin) Updating ${r ? r.started : 0} repositories…`, 4000);
      refreshViews();
    },
    openRepositoryFolder: a => {
      const repo = nodeArg(a).repo;
      if (repo && repo.location && repo.location.dir) {
        vscode.commands.executeCommand('revealFileInOS', vscode.Uri.file(repo.location.dir));
      }
    },
    openCacheFolder: async () => {
      const info = await request('atn/info');
      if (!info) return;
      fs.mkdirSync(info.cacheDir, { recursive: true });
      vscode.env.openExternal(vscode.Uri.file(info.cacheDir));
    },
    setRequiredParameterColor: async () => {
      const config = vscode.workspace.getConfiguration(SECTION);
      const input = await vscode.window.showInputBox({
        title: 'Required Parameter Color',
        prompt: 'HEX color, e.g. #c92d35',
        value: config.get('requiredParameterColor'),
        validateInput: v => /^#([0-9A-Fa-f]{3}){1,2}$/.test(v) ? undefined : 'Enter a HEX color like #ff0000',
      });
      if (input) await config.update('requiredParameterColor', input, vscode.ConfigurationTarget.Global);
    },
    restartServer: async () => {
      if (client) await client.restart();
      refreshViews();
    },
    showOutput: () => output.show(),
    // Opens a pre-filled GitHub issue. {kind: 'bug'|'feature'} skips the
    // picker; {open: false} only returns the URL (used by the E2E test).
    reportIssue: async (a = {}) => {
      let kind = a.kind;
      if (!kind) {
        const pick = await vscode.window.showQuickPick([
          { label: '$(bug) Report a bug', description: 'Something doesn\'t work as expected', kind: 'bug' },
          { label: '$(lightbulb) Suggest a feature', kind: 'feature' },
          { label: '$(star-empty) Give feedback', description: 'Rate and review on the Visual Studio Marketplace', kind: 'review' },
        ], { placeHolder: 'Report a bug or suggest a feature on GitHub, or leave a review' });
        if (!pick) return undefined;
        if (pick.kind === 'review') return vscode.commands.executeCommand(`${SECTION}.giveFeedback`);
        kind = pick.kind;
      }
      // Works without a running server: that is often when people report.
      const info = await Promise.race([request('atn/info'), new Promise(r => setTimeout(r, 2000))]);
      const ext = vscode.extensions.getExtension(`bogdanbujor.${SECTION}`);
      const url = issueUrl(kind, {
        environment: [
          `Azure Templates Navigator ${ext ? ext.packageJSON.version : 'unknown'}`,
          `atn ${(info && info.version) || 'unknown (server not running)'}`,
          `${vscode.env.appName} ${vscode.version}`,
          `${os.type()} ${os.release()} ${os.arch()}`,
        ],
      });
      // A string, not Uri.parse(url): Uri re-encodes the query and would
      // turn the body's own '&' and '=' into parameter separators.
      if (a.open !== false) await vscode.env.openExternal(url);
      return url;
    },
    // The walkthrough built from shared/tips/tips.json (see scripts/copy-assets.js).
    getStarted: () => vscode.commands.executeCommand('workbench.action.openWalkthrough', `bogdanbujor.${SECTION}#getStarted`, false),
    whatsNew: async (a = {}) => {
      if (a.open !== false) await vscode.env.openExternal(CHANGELOG_URL);
      return CHANGELOG_URL;
    },
    // The Marketplace listing's review section. A string, for the same reason as above.
    giveFeedback: async (a = {}) => {
      if (a.open !== false) await vscode.env.openExternal(REVIEW_URL);
      return REVIEW_URL;
    },
  };
  for (const [name, fn] of Object.entries(commands)) {
    context.subscriptions.push(vscode.commands.registerCommand(`${SECTION}.${name}`, fn));
  }

  context.subscriptions.push(vscode.workspace.onDidChangeConfiguration(e => {
    if (!e.affectsConfiguration(SECTION) || !client) return;
    if (e.affectsConfiguration(`${SECTION}.server.path`)) {
      vscode.commands.executeCommand(`${SECTION}.restartServer`);
      return;
    }
    client.sendNotification('workspace/didChangeConfiguration', { settings: serverSettings() });
  }));

  announceVersion(context);
  await startClient(context, refreshViews);
  onEditor(vscode.window.activeTextEditor);
  refreshViews();
  // For the E2E test.
  return { request, graph, stats, pipelines, diagnostics, repositories };
}

async function deactivate() {
  if (client) await client.stop();
}

module.exports = { activate, deactivate, serverSettings };
