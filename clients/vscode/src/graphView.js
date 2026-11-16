'use strict';

// Hosts the shared graph webview (shared/webview/graph.{html,js}, copied into
// media/ at build time). Graph data comes from the server (atn/graph).

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const vscode = require('vscode');

class GraphViewProvider {
  constructor(context, request) {
    this.context = context;
    this.request = request;
    this.webviews = new Set();
    this.fileScope = true;
    this.depth = 1;
    this.direction = 'both';
    this.filter = { problemsOnly: false, kinds: [], repos: [] };
    this.activeUri = undefined;
    this.focusUri = undefined; // "Focus graph here": overrides the active editor until it changes
  }

  /**
   * Scope the graph to a file (from the node menu or the Stats view). Filters
   * set by an earlier "Show in Graph" are cleared, so the file's whole
   * neighbourhood shows, as its usage counts promise.
   */
  focusFile(filePath) {
    this.fileScope = true;
    this.filter = { problemsOnly: false, kinds: [], repos: [] };
    this.focusUri = vscode.Uri.file(filePath);
    this.refresh();
  }

  /** Workspace graph with a filter (from the Stats view). */
  showFiltered(filter) {
    this.fileScope = false;
    this.filter = Object.assign({ problemsOnly: false, kinds: [], repos: [] }, filter);
    this.refresh();
  }

  /** The last graphData sent, for tests. */
  get lastGraph() {
    return this._last;
  }

  setFile(uri) {
    const changed = !this.activeUri || this.activeUri.toString() !== uri.toString();
    this.activeUri = uri;
    if (changed) this.focusUri = undefined;
    if (!changed) return;
    if (this.fileScope) this.refresh();
    else for (const w of this.webviews) w.postMessage({ type: 'select', filePath: uri.fsPath });
  }

  refresh() {
    for (const w of this.webviews) this.send(w);
  }

  resolveWebviewView(view) {
    this.attach(view.webview);
    view.onDidDispose(() => this.webviews.delete(view.webview));
  }

  openPanel() {
    const panel = vscode.window.createWebviewPanel('azure-templates-navigator.graphPanel', 'Template Graph',
      vscode.ViewColumn.One, { enableScripts: true, retainContextWhenHidden: true });
    this.attach(panel.webview);
    panel.onDidDispose(() => this.webviews.delete(panel.webview));
  }

  attach(webview) {
    const media = vscode.Uri.joinPath(this.context.extensionUri, 'media');
    webview.options = { enableScripts: true, localResourceRoots: [media] };
    const nonce = crypto.randomBytes(16).toString('hex');
    const csp = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}'; style-src 'unsafe-inline'; img-src ${webview.cspSource} data:;">`;
    const src = f => webview.asWebviewUri(vscode.Uri.joinPath(media, f));
    const template = fs.readFileSync(path.join(this.context.extensionPath, 'media', 'graph.html'), 'utf8');
    webview.html = template
      // replaceAll: the header comment in graph.html also mentions both placeholders
      .replaceAll('{{HEAD}}', csp)
      .replaceAll('{{SCRIPTS}}', `<script nonce="${nonce}" src="${src('d3.min.js')}"></script>\n  <script nonce="${nonce}" src="${src('graph.js')}"></script>`);
    webview.onDidReceiveMessage(msg => this.onMessage(webview, msg), undefined, this.context.subscriptions);
    this.webviews.add(webview);
  }

  async onMessage(webview, msg) {
    switch (msg.type) {
      case 'ready':
        await this.send(webview);
        await this.sendSearch(webview);
        break;
      case 'setFileScope':
        this.fileScope = !!msg.enabled;
        this.refresh();
        break;
      case 'setDepth':
        this.depth = Math.max(1, Math.min(10, Number(msg.depth) || 1));
        this.refresh();
        break;
      case 'setDirection':
        this.direction = ['callers', 'callees'].includes(msg.direction) ? msg.direction : 'both';
        this.refresh();
        break;
      case 'setFilter':
        this.filter = Object.assign({ problemsOnly: false, kinds: [], repos: [] }, msg.filter);
        this.refresh();
        break;
      case 'focusFile':
        if (msg.filePath) this.focusFile(msg.filePath);
        break;
      case 'openFile':
      case 'openFileBeside':
        vscode.commands.executeCommand('azure-templates-navigator.openTemplate', { filePath: msg.filePath, beside: msg.type === 'openFileBeside' });
        break;
      case 'copyPath':
        if (msg.text) await vscode.env.clipboard.writeText(msg.text);
        break;
      case 'expand':
        this.openPanel();
        break;
      default:
        break;
    }
  }

  async send(webview) {
    if (!vscode.workspace.workspaceFolders || !vscode.workspace.workspaceFolders.length) {
      webview.postMessage({ type: 'noWorkspace' });
      return;
    }
    const focus = this.fileScope ? (this.focusUri || this.activeUri) : undefined;
    const params = Object.assign({}, this.filter, focus ? { uri: focus.toString(), depth: this.depth, direction: this.direction } : {});
    const data = await this.request('atn/graph', params);
    if (!data) return;
    const msg = {
      type: 'graphData',
      nodes: data.nodes,
      edges: data.edges,
      totalNodes: data.totalNodes,
      totalEdges: data.totalEdges,
      facets: data.facets,
      filter: this.filter,
      direction: this.direction,
      fileScopeEnabled: this.fileScope,
      scopedFile: focus ? focus.fsPath : null,
      activeFile: this.activeUri ? this.activeUri.fsPath : null,
      graphDepth: this.depth,
    };
    this._last = msg;
    webview.postMessage(msg);
  }

  async sendSearch(webview) {
    const items = (await this.request('atn/templates')) || [];
    webview.postMessage({
      type: 'searchData',
      items: items.map(t => ({
        filePath: t.filePath,
        filename: t.label,
        relativePath: t.relativePath,
        directory: path.posix.dirname(t.relativePath) === '.' ? '' : path.posix.dirname(t.relativePath),
      })),
    });
  }
}

module.exports = { GraphViewProvider };
