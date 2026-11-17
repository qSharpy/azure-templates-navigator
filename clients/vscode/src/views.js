'use strict';

// Tree views. They render view models produced by the language server
// (atn/dependencyTree, atn/workspaceDiagnostics, atn/pipelineCheck, atn/changes, atn/repositories, atn/stats), so labels,
// descriptions and tooltips are identical to the IntelliJ plugin.

const vscode = require('vscode');
const { DOCS_URL } = require('./issue');

const KIND_ICON = {
  pipeline: 'rocket',
  template: 'file-code',
  external: 'repo',
  missing: 'error',
  dynamic: 'symbol-variable',
  unknownAlias: 'question',
  repoUnavailable: 'cloud-download',
  cycle: 'sync',
};

function icon(kind, severity) {
  const id = KIND_ICON[kind] || 'file';
  if (severity === 1 || kind === 'missing' || kind === 'unknownAlias') return new vscode.ThemeIcon(id, new vscode.ThemeColor('list.errorForeground'));
  if (severity === 2 || kind === 'repoUnavailable') return new vscode.ThemeIcon(id, new vscode.ThemeColor('list.warningForeground'));
  return new vscode.ThemeIcon(id);
}

class Base {
  constructor(request) {
    this.request = request;
    this._emitter = new vscode.EventEmitter();
    this.onDidChangeTreeData = this._emitter.event;
  }
  refresh() { this._emitter.fire(); }
}

// ── Dependencies ──────────────────────────────────────────────────────────
class DependencyTreeProvider extends Base {
  constructor(request) {
    super(request);
    this.uri = undefined;
    this.onTitle = () => {};
  }

  setFile(uri) {
    if (this.uri && this.uri.toString() === uri.toString()) return;
    this.uri = uri;
    this.refresh();
  }

  async getChildren(element) {
    if (element) return element.children || [];
    if (!this.uri) return [];
    const t = await this.request('atn/dependencyTree', { uri: this.uri.toString() });
    if (!t) return [];
    this.onTitle(t.relativePath);
    const isCalling = t.isCalling || [], calledBy = t.calledBy || [];
    return [
      { section: true, label: `Is calling (${isCalling.length})`, children: isCalling.map(n => ({ node: n })), icon: 'arrow-down' },
      { section: true, label: `Called by (${t.callerCount})`, children: calledBy.map(n => ({ node: n })), icon: 'arrow-up' },
    ];
  }

  getTreeItem(el) {
    if (el.section) {
      const item = new vscode.TreeItem(el.label, el.children.length ? vscode.TreeItemCollapsibleState.Expanded : vscode.TreeItemCollapsibleState.None);
      item.iconPath = new vscode.ThemeIcon(el.icon);
      item.contextValue = 'section';
      return item;
    }
    const n = el.node;
    el.children = (n.children || []).map(c => ({ node: c }));
    const item = new vscode.TreeItem(n.label, el.children.length ? vscode.TreeItemCollapsibleState.Collapsed : vscode.TreeItemCollapsibleState.None);
    item.id = n.id;
    item.description = [n.description, n.paramCount ? `${n.paramCount}p` : ''].filter(Boolean).join(' · ');
    item.tooltip = n.tooltip;
    item.iconPath = icon(n.kind, n.severity);
    const isFile = n.filePath && n.kind !== 'missing';
    item.contextValue = (isFile ? 'file' : n.kind) + (n.refFile ? '-ref' : '');
    if (isFile) {
      item.command = { command: 'azure-templates-navigator.openTemplate', title: 'Open', arguments: [{ filePath: n.filePath, line: n.line }] };
    } else if (n.refFile) {
      item.command = { command: 'azure-templates-navigator.openTemplate', title: 'Go to reference', arguments: [{ filePath: n.refFile, line: n.refLine }] };
    }
    return item;
  }
}

// ── Diagnostics ───────────────────────────────────────────────────────────
class DiagnosticsTreeProvider extends Base {
  // mode: 'all' (every problem) or 'changes' (only the ones your work added),
  // shared with the Pipelines view.
  constructor(request, mode = 'all') {
    super(request);
    this.mode = mode;
    this.onCounts = () => {};
  }

  setMode(mode) {
    this.mode = mode;
    this.refresh();
  }

  async getChildren(element) {
    if (element) return element.children;
    if (this.mode === 'changes') return this.changes();
    const files = (await this.request('atn/workspaceDiagnostics')) || [];
    let errors = 0, warnings = 0;
    const out = files.map(f => {
      errors += f.errors; warnings += f.warnings;
      return { file: f, children: f.diagnostics.map(d => ({ diag: d, file: f })) };
    });
    this.onCounts({ errors, warnings });
    return out;
  }

  async changes() {
    const c = await this.request('atn/workspaceDiagnostics', { onlyNew: true });
    if (!c || !c.available) {
      this.onCounts({ mode: 'changes', errors: 0, warnings: 0 });
      return [{ message: (c && c.reason) || 'The language server is not running', tooltip: 'Changes are compared with a git commit: open a folder that is a git repository.' }];
    }
    this.onCounts({ mode: 'changes', label: c.label, errors: c.newErrors, warnings: c.newWarnings });
    const rows = [{
      base: true,
      label: `Since ${c.label}`,
      description: c.existing ? `${c.existing} already there, hidden` : '',
      tooltip: `Only the problems your changes added since ${c.label}. ${c.existing} problem(s) that were already there are hidden: *Show Everything* in the title shows them.\n\nClick to compare with another branch or commit.`,
    }];
    if (!c.files.length) rows.push({ message: 'No new problems', icon: 'pass' });
    for (const f of c.files) rows.push({ file: f, children: f.diagnostics.map(d => ({ diag: d, file: f })) });
    return rows;
  }

  getTreeItem(el) {
    if (el.message) {
      const item = new vscode.TreeItem(el.message, vscode.TreeItemCollapsibleState.None);
      item.iconPath = new vscode.ThemeIcon(el.icon || 'info', el.icon === 'pass' ? new vscode.ThemeColor('testing.iconPassed') : undefined);
      item.tooltip = el.tooltip;
      return item;
    }
    if (el.base) {
      const item = new vscode.TreeItem(el.label, vscode.TreeItemCollapsibleState.None);
      item.description = el.description;
      item.tooltip = el.tooltip;
      item.iconPath = new vscode.ThemeIcon('git-compare');
      item.command = { command: 'azure-templates-navigator.chooseChangesBase', title: 'Compare with…' };
      return item;
    }
    if (el.diag) {
      const d = el.diag;
      const item = new vscode.TreeItem(d.message, vscode.TreeItemCollapsibleState.None);
      item.description = `Ln ${d.line + 1}`;
      item.tooltip = `${d.message}\n[${d.code}]`;
      item.iconPath = new vscode.ThemeIcon(d.severity === 1 ? 'error' : 'warning',
        new vscode.ThemeColor(d.severity === 1 ? 'list.errorForeground' : 'list.warningForeground'));
      item.command = { command: 'azure-templates-navigator.openTemplate', title: 'Go to issue', arguments: [{ filePath: el.file.filePath, line: d.line }] };
      return item;
    }
    const f = el.file;
    const item = new vscode.TreeItem(f.relativePath, vscode.TreeItemCollapsibleState.Expanded);
    item.description = [f.errors ? `${f.errors} error(s)` : '', f.warnings ? `${f.warnings} warning(s)` : ''].filter(Boolean).join(', ');
    item.resourceUri = vscode.Uri.file(f.filePath);
    item.iconPath = vscode.ThemeIcon.File;
    return item;
  }
}

// ── Repositories ──────────────────────────────────────────────────────────
class RepositoriesTreeProvider extends Base {
  async getChildren(element) {
    if (element) return element.children;
    const repos = (await this.request('atn/repositories')) || [];
    return repos.map(r => {
      const decls = r.declarations || [];
      const children = (r.templates || []).map(t => ({ template: t, alias: r.alias }));
      if (decls.length) children.push({ declGroup: true, count: decls.length, children: decls.map(d => ({ declaration: d })) });
      // declaredInPath: the file fetchRepository resolves the alias in
      return { repo: { ...r, declaredInPath: decls.length ? decls[0].filePath : undefined }, children };
    });
  }

  getTreeItem(el) {
    if (el.template) {
      const t = el.template;
      const item = new vscode.TreeItem(t.label, vscode.TreeItemCollapsibleState.None);
      const uses = `${t.usages} usage${t.usages === 1 ? '' : 's'} · ${t.callers} file${t.callers === 1 ? '' : 's'}`;
      item.description = t.broken ? `${uses} · not found` : uses;
      item.tooltip = t.broken
        ? `${t.label}@${el.alias} can't be found. Click to go to the first call.`
        : `${t.label}@${el.alias}\nUsed ${uses} in this workspace`;
      item.iconPath = t.broken
        ? new vscode.ThemeIcon('error', new vscode.ThemeColor('list.errorForeground'))
        : new vscode.ThemeIcon('file-code');
      item.contextValue = t.broken ? 'repoTemplate-broken' : 'repoTemplate';
      item.command = t.filePath
        ? { command: 'azure-templates-navigator.openTemplate', title: 'Open', arguments: [{ filePath: t.filePath }] }
        : { command: 'azure-templates-navigator.openTemplate', title: 'Go to Call', arguments: [{ filePath: t.refFile, line: t.refLine }] };
      return item;
    }
    if (el.declGroup) {
      const item = new vscode.TreeItem('Declared in', vscode.TreeItemCollapsibleState.Collapsed);
      item.description = `${el.count} file${el.count === 1 ? '' : 's'}`;
      item.tooltip = 'Files whose resources.repositories declares this repository';
      item.iconPath = new vscode.ThemeIcon('references');
      item.contextValue = 'repoDeclarations';
      return item;
    }
    if (el.declaration) {
      const d = el.declaration;
      const item = new vscode.TreeItem(d.relativePath, vscode.TreeItemCollapsibleState.None);
      item.description = `line ${d.line + 1}`;
      item.iconPath = new vscode.ThemeIcon('file');
      item.contextValue = 'repoDeclaration';
      item.command = { command: 'azure-templates-navigator.openTemplate', title: 'Open', arguments: [{ filePath: d.filePath, line: d.line }] };
      return item;
    }
    const r = el.repo;
    const loc = r.location || {};
    const item = new vscode.TreeItem(r.label, el.children.length ? vscode.TreeItemCollapsibleState.Collapsed : vscode.TreeItemCollapsibleState.None);
    const n = (r.templates || []).length;
    item.description = `${r.detail} · ${n} template${n === 1 ? '' : 's'} used`;
    const tip = new vscode.MarkdownString();
    tip.appendMarkdown(`**${r.name}** (@${r.alias})\n\n`);
    if (loc.url) tip.appendMarkdown(`URL: \`${loc.url}\`\n\n`);
    if (loc.dir) tip.appendMarkdown(`Folder: \`${loc.dir}\`\n\n`);
    if (loc.commit) tip.appendMarkdown(`Commit: \`${loc.commit.slice(0, 12)}\`\n\n`);
    if (loc.error) tip.appendMarkdown(`⚠️ ${loc.error}`);
    item.tooltip = tip;
    const iconId = { ready: 'repo', fetching: 'sync~spin', missing: 'cloud-download', failed: 'warning', unconfigured: 'gear' }[loc.state] || 'repo';
    item.iconPath = loc.state === 'failed' || loc.state === 'unconfigured'
      ? new vscode.ThemeIcon(iconId, new vscode.ThemeColor('list.warningForeground'))
      : new vscode.ThemeIcon(iconId);
    item.contextValue = `repo-${loc.source || loc.state}-${loc.state}`;
    return item;
  }
}

// ── Pipelines ─────────────────────────────────────────────────────────────
// Two modes, switched from the view title:
//  - all: every root pipeline with its verdict (atn/pipelineCheck without a
//    uri), failing first; problems your work added are marked "new".
//  - changes: what your work changed since where it started (atn/changes):
//    the pipelines it reaches, each with its new and fixed problems, and the
//    files you changed. The first row names the base; click it to change it.
const VERDICT_ICON = {
  fail: ['error', 'list.errorForeground'],
  warn: ['warning', 'list.warningForeground'],
  ok: ['pass', 'testing.iconPassed'],
};
const FILE_ICON = { added: 'diff-added', modified: 'diff-modified', deleted: 'diff-removed' };

function verdictIcon(verdict) {
  const [id, color] = VERDICT_ICON[verdict] || ['circle-slash', 'disabledForeground'];
  return new vscode.ThemeIcon(id, new vscode.ThemeColor(color));
}

function problemItem(p, { fixed = false } = {}) {
  const line = p.range.start.line + 1;
  const item = new vscode.TreeItem(p.message, vscode.TreeItemCollapsibleState.None);
  item.description = `${p.new ? 'new · ' : ''}${p.relativePath}:${line}`;
  item.tooltip = `${p.message}\n${p.relativePath}:${line} [${p.code}]${fixed ? '\nFixed: it was there at the base, it is gone now' : p.new ? '\nNew: your changes added it' : ''}`;
  item.iconPath = fixed
    ? new vscode.ThemeIcon('pass', new vscode.ThemeColor('testing.iconPassed'))
    : new vscode.ThemeIcon(p.severity === 1 ? 'error' : 'warning',
      new vscode.ThemeColor(p.severity === 1 ? 'list.errorForeground' : 'list.warningForeground'));
  item.command = { command: 'azure-templates-navigator.openTemplate', title: 'Go to problem', arguments: [{ filePath: p.filePath, line: p.range.start.line }] };
  return item;
}

class PipelinesTreeProvider extends Base {
  constructor(request, mode = 'all') {
    super(request);
    this.mode = mode;
    this.onSummary = () => {};
  }

  setMode(mode) {
    if (this.mode === mode) return;
    this.mode = mode;
    this.refresh();
  }

  async getChildren(element) {
    if (element) return element.children || [];
    return this.mode === 'changes' ? this.changes() : this.all();
  }

  async all() {
    const reports = await this.request('atn/pipelineCheck', {});
    if (!reports) {
      this.onSummary({ mode: 'all', reports: null });
      return [{ message: 'Diagnostics are turned off', tooltip: 'Turn on azure-templates-navigator.diagnostics.enabled to check pipelines' }];
    }
    this.onSummary({ mode: 'all', reports });
    return reports.map(r => ({ report: r, children: r.problems.map(p => ({ problem: p })) }));
  }

  async changes() {
    const c = await this.request('atn/changes', {});
    this.onSummary({ mode: 'changes', changes: c });
    if (!c) return [{ message: 'The language server is not running' }];
    if (!c.available) {
      return [{ message: c.reason || 'Nothing to compare with', tooltip: 'Changes are compared with a git commit: open a folder that is a git repository.' }];
    }
    const base = c.bases[0] || {};
    const rows = [{
      base: true,
      label: `Since ${c.label}`,
      description: base.commits ? `${base.commits} commit${base.commits === 1 ? '' : 's'} + uncommitted` : base.ref === 'HEAD' ? '' : 'uncommitted',
      tooltip: c.bases.map(b => `${b.repo}: ${b.detail}${b.error ? `\n⚠ ${b.error}` : ''}`).join('\n') + '\n\nClick to compare with another branch or commit.',
      warn: c.bases.some(b => b.error),
    }];
    // what you changed first, then what it did to the pipelines
    if (c.files.length) {
      rows.push({
        section: true, label: `Files changed (${c.files.length})`, icon: 'files', expanded: true,
        children: c.files.map(f => ({ file: f, children: [...f.new.map(x => ({ problem: x })), ...f.fixed.map(x => ({ problem: x, fixed: true }))] })),
      });
    }
    rows.push({
      summary: true,
      label: c.summary,
      newErrors: c.newErrors,
      tooltip: `What your changes did to the pipelines below.\n\nIn total, ${c.allFailing} of ${c.allPipelines} pipelines fail when queued, counting problems that were there before your changes. Click to see every pipeline.`,
      description: `${c.allFailing} of ${c.allPipelines} fail in total`,
    });
    for (const p of c.pipelines) {
      const children = [];
      if (p.new.length) children.push({ section: true, label: `New (${p.new.length})`, icon: 'diff-added', expanded: true, children: p.new.map(x => ({ problem: x })) });
      if (p.fixed.length) children.push({ section: true, label: `Fixed (${p.fixed.length})`, icon: 'pass', children: p.fixed.map(x => ({ problem: x, fixed: true })) });
      if (p.changed.length) children.push({ section: true, label: `Files you changed that it uses (${p.changed.length})`, icon: 'git-compare', children: p.changed.map(rel => ({ rel })) });
      rows.push({ change: p, children });
    }
    return rows;
  }

  getTreeItem(el) {
    const None = vscode.TreeItemCollapsibleState.None;
    if (el.message) {
      const item = new vscode.TreeItem(el.message, None);
      item.iconPath = new vscode.ThemeIcon('info');
      item.tooltip = el.tooltip;
      return item;
    }
    if (el.base) {
      const item = new vscode.TreeItem(el.label, None);
      item.description = el.description;
      item.tooltip = el.tooltip;
      item.iconPath = new vscode.ThemeIcon('git-compare', el.warn ? new vscode.ThemeColor('list.warningForeground') : undefined);
      item.command = { command: 'azure-templates-navigator.chooseChangesBase', title: 'Compare with…' };
      item.contextValue = 'changesBase';
      return item;
    }
    if (el.summary) {
      const item = new vscode.TreeItem(el.label, None);
      item.description = el.description;
      item.tooltip = el.tooltip;
      item.iconPath = el.newErrors ? new vscode.ThemeIcon('error', new vscode.ThemeColor('list.errorForeground'))
        : new vscode.ThemeIcon('pass', new vscode.ThemeColor('testing.iconPassed'));
      item.command = { command: 'azure-templates-navigator.showAllPipelines', title: 'Show all pipelines' };
      return item;
    }
    if (el.section) {
      const item = new vscode.TreeItem(el.label, el.expanded ? vscode.TreeItemCollapsibleState.Expanded : vscode.TreeItemCollapsibleState.Collapsed);
      item.iconPath = new vscode.ThemeIcon(el.icon);
      return item;
    }
    if (el.rel) {
      const item = new vscode.TreeItem(el.rel, None);
      item.iconPath = new vscode.ThemeIcon('file');
      return item;
    }
    if (el.problem) return problemItem(el.problem, { fixed: el.fixed });
    if (el.file) {
      const f = el.file;
      const item = new vscode.TreeItem(f.relativePath, el.children.length ? vscode.TreeItemCollapsibleState.Collapsed : None);
      const parts = [f.status];
      if (f.pipelines) parts.push(`${f.pipelines} pipeline${f.pipelines === 1 ? '' : 's'} use it`);
      if (f.new.length) parts.push(`+${f.new.length} new`);
      if (f.fixed.length) parts.push(`${f.fixed.length} fixed`);
      item.description = parts.join(' · ');
      item.iconPath = new vscode.ThemeIcon(FILE_ICON[f.status] || 'file');
      if (f.status !== 'deleted') {
        item.resourceUri = vscode.Uri.file(f.filePath);
        item.command = { command: 'azure-templates-navigator.openTemplate', title: 'Open', arguments: [{ filePath: f.filePath }] };
      }
      return item;
    }
    if (el.change) {
      const p = el.change;
      const item = new vscode.TreeItem(p.relativePath, el.children.length ? vscode.TreeItemCollapsibleState.Collapsed : None);
      item.id = `change:${p.filePath}`;
      item.description = p.title;
      const before = p.before.exists ? `${p.before.errors} error(s), ${p.before.warnings} warning(s)` : 'did not exist';
      const now = p.now.exists ? `${p.now.errors} error(s), ${p.now.warnings} warning(s)` : 'removed';
      item.tooltip = `${p.relativePath}\nBefore: ${before}\nNow: ${now}`;
      item.iconPath = verdictIcon(p.now.verdict);
      if (p.now.exists) {
        item.resourceUri = vscode.Uri.file(p.filePath);
        item.contextValue = 'pipeline';
        item.command = { command: 'azure-templates-navigator.openTemplate', title: 'Open', arguments: [{ filePath: p.filePath }] };
      }
      return item;
    }
    const r = el.report;
    const item = new vscode.TreeItem(r.relativePath, r.problems.length
      ? vscode.TreeItemCollapsibleState.Collapsed : vscode.TreeItemCollapsibleState.None);
    item.id = `pipeline:${r.filePath}`;
    item.description = r.title.replace(/^\S+ /, '') + (r.change && r.change.new ? ` · +${r.change.new} new` : '');
    item.tooltip = `${r.relativePath}\n${r.title}\n${r.templates} template(s) checked` + (r.change ? `\nYour changes: ${r.change.title}` : '');
    item.iconPath = verdictIcon(r.verdict);
    item.resourceUri = vscode.Uri.file(r.filePath);
    item.contextValue = 'pipeline';
    item.command = { command: 'azure-templates-navigator.openTemplate', title: 'Open', arguments: [{ filePath: r.filePath }] };
    return item;
  }
}

// ── Stats ─────────────────────────────────────────────────────────────────
const STAT_ICON = {
  pipeline: 'rocket', template: 'file-code', external: 'repo', missing: 'error',
  repo: 'repo', error: 'error', warning: 'warning', info: 'info',
};

class StatsTreeProvider extends Base {
  async getChildren(element) {
    if (element) return (element.section ? element.rows : element.row.children || []).map(row => ({ row }));
    this.sections = (await this.request('atn/stats')) || [];
    return this.sections.map((s, i) => ({ section: s, rows: s.rows || [], index: i }));
  }

  getTreeItem(el) {
    if (el.section) {
      const s = el.section;
      const state = !el.rows.length ? vscode.TreeItemCollapsibleState.None
        : el.index < 3 ? vscode.TreeItemCollapsibleState.Expanded : vscode.TreeItemCollapsibleState.Collapsed;
      const item = new vscode.TreeItem(s.label, state);
      item.id = `stats:${s.id}`;
      item.description = s.description;
      item.tooltip = s.tooltip;
      item.contextValue = 'stat-section';
      return item;
    }
    const r = el.row;
    const hasChildren = !!(r.children && r.children.length);
    const item = new vscode.TreeItem(r.label, hasChildren
      ? vscode.TreeItemCollapsibleState.Collapsed : vscode.TreeItemCollapsibleState.None);
    item.description = r.description;
    item.tooltip = r.tooltip;
    const id = STAT_ICON[r.kind] || 'circle-small';
    const color = r.kind === 'error' || r.kind === 'missing' || r.severity === 1 ? 'list.errorForeground'
      : r.kind === 'warning' || r.severity === 2 ? 'list.warningForeground' : undefined;
    item.iconPath = color ? new vscode.ThemeIcon(id, new vscode.ThemeColor(color)) : new vscode.ThemeIcon(id);
    item.contextValue = r.focus ? `stat-${r.focus}` : 'stat';
    // A click goes to the row's file; a row with children only expands (its
    // graph stays in the context menu); else the view or graph it points at.
    if (r.filePath) {
      item.command = { command: 'azure-templates-navigator.openTemplate', title: 'Open', arguments: [{ filePath: r.filePath, line: r.line }] };
    } else if (hasChildren) {
      // expand only
    } else if (r.view) {
      item.command = { command: `azure-templates-navigator.${r.view}View.focus`, title: 'Open View' };
    } else if (r.focus) {
      item.command = { command: 'azure-templates-navigator.showInGraph', title: 'Show in Graph', arguments: [el] };
    }
    return item;
  }
}

/** "Help and Feedback": always-visible links to report a bug, read the docs, open the log, suggest a feature or leave a review. */
class FeedbackTreeProvider {
  getChildren() {
    const ns = 'azure-templates-navigator';
    return [
      { label: 'Get Started', description: 'tips, one feature at a time', icon: 'rocket', command: `${ns}.getStarted`, tooltip: 'Opens the Get Started walkthrough' },
      { label: "What's New", icon: 'sparkle', command: `${ns}.whatsNew`, tooltip: 'Changelog on GitHub' },
      { label: 'Report a Bug', icon: 'bug', command: `${ns}.reportIssue`, args: [{ kind: 'bug' }], tooltip: 'Opens a new GitHub issue with your versions filled in' },
      { label: 'Documentation', description: 'every feature, with shortcuts', icon: 'book', command: 'vscode.open', args: [vscode.Uri.parse(DOCS_URL)], tooltip: 'The feature guide on GitHub: screenshots and shortcuts for everything the extension does' },
      { label: 'Show Log', icon: 'output', command: `${ns}.showOutput`, tooltip: 'Useful to attach to a bug report' },
      { label: 'Suggest a Feature', icon: 'lightbulb', command: `${ns}.reportIssue`, args: [{ kind: 'feature' }], tooltip: 'Opens a new GitHub issue' },
      { label: 'Give Feedback', description: 'rate and review', icon: 'star-empty', command: `${ns}.giveFeedback`, tooltip: 'Rate and review the extension on the Visual Studio Marketplace' },
    ];
  }
  getTreeItem(el) {
    const item = new vscode.TreeItem(el.label, vscode.TreeItemCollapsibleState.None);
    item.iconPath = new vscode.ThemeIcon(el.icon);
    item.description = el.description;
    item.tooltip = el.tooltip;
    item.command = { command: el.command, title: el.label, arguments: el.args || [] };
    return item;
  }
}

module.exports = { DependencyTreeProvider, DiagnosticsTreeProvider, PipelinesTreeProvider, RepositoriesTreeProvider, StatsTreeProvider, FeedbackTreeProvider };
