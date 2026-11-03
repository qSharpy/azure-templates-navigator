'use strict';

// Tree views. They render view models produced by the language server
// (atn/dependencyTree, atn/workspaceDiagnostics, atn/repositories), so labels,
// descriptions and tooltips are identical to the IntelliJ plugin.

const vscode = require('vscode');

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
  constructor(request) {
    super(request);
    this.onCounts = () => {};
  }

  async getChildren(element) {
    if (element) return element.children;
    const files = (await this.request('atn/workspaceDiagnostics')) || [];
    let errors = 0, warnings = 0;
    const out = files.map(f => {
      errors += f.errors; warnings += f.warnings;
      return { file: f, children: f.diagnostics.map(d => ({ diag: d, file: f })) };
    });
    this.onCounts(errors, warnings);
    return out;
  }

  getTreeItem(el) {
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
    return repos.map(r => ({
      repo: { ...r, declaredInPath: undefined },
      children: (r.declaredIn || []).map(p => ({ declaredIn: p })),
    }));
  }

  getTreeItem(el) {
    if (el.declaredIn) {
      const item = new vscode.TreeItem(el.declaredIn, vscode.TreeItemCollapsibleState.None);
      item.iconPath = new vscode.ThemeIcon('file');
      item.description = 'declared in';
      return item;
    }
    const r = el.repo;
    const loc = r.location || {};
    const item = new vscode.TreeItem(r.label, vscode.TreeItemCollapsibleState.Collapsed);
    item.description = r.detail;
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

module.exports = { DependencyTreeProvider, DiagnosticsTreeProvider, RepositoriesTreeProvider };
