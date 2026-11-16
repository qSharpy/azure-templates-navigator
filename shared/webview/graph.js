/* eslint-env browser */
/* global d3, acquireVsCodeApi */
'use strict';

// ---------------------------------------------------------------------------
// Host bridge — this file is shared by every IDE client.
//   VS Code : acquireVsCodeApi() provides postMessage.
//   IntelliJ: the host defines window.atnHost = { postMessage(msg) } before
//             this script runs and delivers replies via window.postMessage.
// Host → webview messages: graphData | searchData | noWorkspace | error |
//                          fit | setFullPath
// Webview → host messages: ready | setFileScope | setDepth | setDirection |
//                          setFilter | focusFile | openFile | openFileBeside |
//                          copyPath | expand | contextMenu
// graphData carries the node/edge totals, the filter facets and the current
// filter ({problemsOnly, kinds, repos}) and direction, which the host owns.
// ---------------------------------------------------------------------------
const vscode = window.atnHost
  || (typeof acquireVsCodeApi === 'function' ? acquireVsCodeApi() : { postMessage() {} });

// A host with native chrome (IntelliJ) draws its own toolbar, search and
// context menu around the page; the page then only renders the graph.
//   Host → page:  fit | setFullPath {enabled}
//   Page → host:  contextMenu {filePath, kind}
const nativeChrome = !!(window.atnHost && window.atnHost.nativeChrome);
if (nativeChrome) document.body.classList.add('native-chrome');

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------
let allNodes = [];
let allEdges = [];
let simulation = null;
let fileScopeEnabled = true; // mirrors _fileScopeEnabled on the extension side
let scopedFilePath = null;   // the focal file when in file-scope mode
let showFullPath = false;    // whether to show workspace-relative paths as node labels
let graphDepth = 1;          // current BFS depth for file-scope mode (1–10)
let graphDirection = 'both'; // file scope: both | callers | callees
/** Filter state, owned by the host and echoed back in graphData. */
let filter = { problemsOnly: false, kinds: [], repos: [] };
let facets = { kinds: {}, problems: 0, repos: [] };

/** @type {Array<{filePath:string,filename:string,relativePath:string,directory:string}>} */
let searchIndex = [];        // all workspace YAML files for the search bar

// ---------------------------------------------------------------------------
// Colour palette (matches legend in HTML)
// ---------------------------------------------------------------------------
const KIND_COLOR = {
  pipeline: '#4e9de0',
  local:    '#3dba8a',
  external: '#9b6fd4',
  missing:  '#e05c5c',
  unknown:  '#e09a3d',
};

// Node size follows the server's `weight` (callers + callees, counted over
// the whole workspace), so a node keeps its size whatever the scope, depth or
// filters. Area grows with weight (radius with its square root), steep enough
// that hubs stand out: weight 1 → 13, 15 → 29, 40 → 43, 100 → 63, 140+ → 73.
const MIN_RADIUS = 8;
const MAX_RADIUS = 73;
function radius(d) {
  return Math.min(MAX_RADIUS, MIN_RADIUS + 5.5 * Math.sqrt(d.weight || 0));
}

const KIND_STROKE = {
  pipeline: '#2a7abf',
  local:    '#1f8a5e',
  external: '#6a3fa8',
  missing:  '#b03030',
  unknown:  '#b06a10',
};

// ---------------------------------------------------------------------------
// DOM refs
// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// Search bar
// ---------------------------------------------------------------------------
const searchInput   = document.getElementById('search-input');
const searchResults = document.getElementById('search-results');
const searchEmpty   = document.getElementById('search-empty');

let searchDebounce = null;

searchInput.addEventListener('input', () => {
  clearTimeout(searchDebounce);
  searchDebounce = setTimeout(() => runSearch(searchInput.value), 120);
});

searchInput.addEventListener('keydown', (e) => {
  if (e.key === 'ArrowDown') {
    e.preventDefault();
    const first = searchResults.querySelector('.sr-item');
    if (first) first.focus();
  } else if (e.key === 'Escape') {
    closeSearch();
  }
});

searchResults.addEventListener('keydown', (e) => {
  const items = [...searchResults.querySelectorAll('.sr-item')];
  const idx = items.indexOf(document.activeElement);
  if (e.key === 'ArrowDown') {
    e.preventDefault();
    if (idx < items.length - 1) items[idx + 1].focus();
  } else if (e.key === 'ArrowUp') {
    e.preventDefault();
    if (idx > 0) items[idx - 1].focus();
    else searchInput.focus();
  } else if (e.key === 'Enter') {
    e.preventDefault();
    if (document.activeElement && document.activeElement.dataset.filepath) {
      openSearchResult(document.activeElement.dataset.filepath);
    }
  } else if (e.key === 'Escape') {
    closeSearch();
  }
});

// Close dropdown when clicking outside the search bar
document.addEventListener('click', (e) => {
  if (!e.target.closest('#search-bar')) closeSearch();
});

function runSearch(query) {
  const q = query.trim().toLowerCase();
  if (!q) { closeSearch(); return; }

  const hits = searchIndex.filter(item =>
    item.relativePath.toLowerCase().includes(q) ||
    item.filename.toLowerCase().includes(q)
  ).slice(0, 30);

  renderSearchResults(hits);
}

function renderSearchResults(hits) {
  // Remove old items (keep the #search-empty sentinel)
  searchResults.querySelectorAll('.sr-item').forEach(el => el.remove());

  if (hits.length === 0) {
    searchEmpty.style.display = 'block';
    searchResults.classList.add('open');
    return;
  }

  searchEmpty.style.display = 'none';
  searchResults.classList.add('open');

  for (const item of hits) {
    const el = document.createElement('div');
    el.className = 'sr-item';
    el.tabIndex = 0;
    el.dataset.filepath = item.filePath;
    el.title = item.relativePath;

    const name = document.createElement('div');
    name.className = 'sr-name';
    name.textContent = item.filename;

    const dir = document.createElement('div');
    dir.className = 'sr-path';
    dir.textContent = item.directory || item.relativePath;

    el.appendChild(name);
    el.appendChild(dir);

    el.addEventListener('mousedown', (e) => {
      // Use mousedown so it fires before the blur that would close the dropdown
      e.preventDefault();
      openSearchResult(item.filePath);
    });

    searchResults.appendChild(el);
  }
}

function openSearchResult(filePath) {
  closeSearch();
  vscode.postMessage({ type: 'openFile', filePath });
}

function closeSearch() {
  searchResults.classList.remove('open');
  searchInput.value = '';
}

// ---------------------------------------------------------------------------
// DOM refs (graph)
// ---------------------------------------------------------------------------
const svgEl          = document.getElementById('svg');
const zoomLayer      = document.getElementById('zoom-layer');
const edgesLayer     = document.getElementById('edges-layer');
const edgeLabels     = document.getElementById('edge-labels-layer');
const nodesLayer     = document.getElementById('nodes-layer');
const ctxMenu        = document.getElementById('ctx-menu');
const emptyState     = document.getElementById('empty-state');
const btnFileScope   = document.getElementById('btn-file-scope');
const btnFullPath    = document.getElementById('btn-full-path');
const depthControls  = document.getElementById('depth-controls');
const depthValue     = document.getElementById('depth-value');
const btnDepthDec    = document.getElementById('btn-depth-dec');
const btnDepthInc    = document.getElementById('btn-depth-inc');
const directionSel   = document.getElementById('direction');
const countsEl       = document.getElementById('counts');
const btnProblems    = document.getElementById('flt-problems');
const btnRepos       = document.getElementById('flt-repos');
const repoMenu       = document.getElementById('repo-menu');
const kindButtons    = [...document.querySelectorAll('.flt-kind')];
const ALL_KINDS      = kindButtons.map(b => b.dataset.kind);

const svg = d3.select(svgEl);

// ---------------------------------------------------------------------------
// Zoom behaviour
// ---------------------------------------------------------------------------
const zoom = d3.zoom()
  .scaleExtent([0.05, 4])
  .on('zoom', (event) => {
    d3.select(zoomLayer).attr('transform', event.transform);
  });

svg.call(zoom);

// ---------------------------------------------------------------------------
// Legend toggle
// ---------------------------------------------------------------------------
document.getElementById('legend-toggle').addEventListener('click', () => {
  document.getElementById('legend').classList.toggle('open');
});

// ---------------------------------------------------------------------------
// Toolbar buttons
// ---------------------------------------------------------------------------
document.getElementById('btn-fit').addEventListener('click', fitView);

// ---------------------------------------------------------------------------
// File-scope toggle button
// ---------------------------------------------------------------------------

/**
 * Updates the visual state of the file-scope button to match `fileScopeEnabled`.
 */
function updateFileScopeButton() {
  if (fileScopeEnabled) {
    btnFileScope.classList.add('active');
    btnFileScope.textContent = 'File';
    btnFileScope.title = 'Showing current file only — click to show full workspace graph';
  } else {
    btnFileScope.classList.remove('active');
    btnFileScope.textContent = 'Workspace';
    btnFileScope.title = 'Scope graph to the currently open file (shows parent + direct children only)';
  }
}

btnFileScope.addEventListener('click', () => {
  fileScopeEnabled = !fileScopeEnabled;
  updateFileScopeButton();
  updateDepthControls();
  vscode.postMessage({ type: 'setFileScope', enabled: fileScopeEnabled });
});

// ---------------------------------------------------------------------------
// Depth controls (file-scope mode only)
// ---------------------------------------------------------------------------

/**
 * Updates the depth controls UI to reflect the current `graphDepth` value
 * and shows/hides the controls based on whether file-scope mode is active.
 */
function updateDepthControls() {
  if (fileScopeEnabled) {
    depthControls.classList.add('visible');
  } else {
    depthControls.classList.remove('visible');
  }
  depthValue.textContent = String(graphDepth);
  directionSel.value = graphDirection;
  btnDepthDec.disabled = graphDepth <= 1;
  btnDepthInc.disabled = graphDepth >= 10;
}

btnDepthDec.addEventListener('click', () => {
  if (graphDepth <= 1) return;
  graphDepth--;
  updateDepthControls();
  vscode.postMessage({ type: 'setDepth', depth: graphDepth });
});

directionSel.addEventListener('change', () => {
  graphDirection = directionSel.value;
  vscode.postMessage({ type: 'setDirection', direction: graphDirection });
});

btnDepthInc.addEventListener('click', () => {
  if (graphDepth >= 10) return;
  graphDepth++;
  updateDepthControls();
  vscode.postMessage({ type: 'setDepth', depth: graphDepth });
});

// ---------------------------------------------------------------------------
// Filters (in-page controls; IntelliJ draws native ones and hides these)
// ---------------------------------------------------------------------------

function sendFilter() {
  vscode.postMessage({ type: 'setFilter', filter });
}

/** Kinds currently shown; an empty filter list means "all". */
function shownKinds() {
  return filter.kinds && filter.kinds.length ? filter.kinds : ALL_KINDS;
}

btnProblems.addEventListener('click', () => {
  filter = Object.assign({}, filter, { problemsOnly: !filter.problemsOnly });
  updateFilterBar();
  sendFilter();
});

for (const b of kindButtons) {
  b.addEventListener('click', () => {
    const shown = new Set(shownKinds());
    if (shown.has(b.dataset.kind)) {
      if (shown.size === 1) return; // keep at least one kind
      shown.delete(b.dataset.kind);
    } else {
      shown.add(b.dataset.kind);
    }
    const kinds = ALL_KINDS.filter(k => shown.has(k));
    filter = Object.assign({}, filter, { kinds: kinds.length === ALL_KINDS.length ? [] : kinds });
    updateFilterBar();
    sendFilter();
  });
}

btnRepos.addEventListener('click', (e) => {
  e.stopPropagation();
  repoMenu.classList.toggle('open');
});
repoMenu.addEventListener('click', (e) => e.stopPropagation());
document.addEventListener('click', () => repoMenu.classList.remove('open'));

function renderRepoMenu() {
  repoMenu.textContent = '';
  const all = (facets.repos || []).map(r => r.key);
  const selected = new Set(filter.repos && filter.repos.length ? filter.repos : all);
  for (const r of facets.repos || []) {
    const label = document.createElement('label');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = selected.has(r.key);
    cb.addEventListener('change', () => {
      if (cb.checked) selected.add(r.key); else selected.delete(r.key);
      if (selected.size === 0) { selected.add(r.key); cb.checked = true; return; }
      const repos = all.filter(k => selected.has(k));
      filter = Object.assign({}, filter, { repos: repos.length === all.length ? [] : repos });
      updateFilterBar();
      sendFilter();
    });
    const name = document.createElement('span');
    name.textContent = r.label;
    const count = document.createElement('span');
    count.className = 'count';
    count.textContent = String(r.count);
    label.append(cb, name, count);
    repoMenu.appendChild(label);
  }
}

function updateFilterBar() {
  btnProblems.classList.toggle('active', !!filter.problemsOnly);
  btnProblems.textContent = `⚠ Problems ${facets.problems || 0}`;
  const shown = new Set(shownKinds());
  for (const b of kindButtons) {
    b.classList.toggle('off', !shown.has(b.dataset.kind));
    const n = (facets.kinds || {})[b.dataset.kind] || 0;
    b.lastChild.textContent = `${b.dataset.label || (b.dataset.label = b.lastChild.textContent)} ${n}`;
  }
  const repos = facets.repos || [];
  btnRepos.style.display = repos.length > 1 ? '' : 'none';
  const picked = filter.repos && filter.repos.length;
  btnRepos.classList.toggle('active', !!picked);
  btnRepos.textContent = picked ? `Repositories: ${picked} of ${repos.length} ▾` : 'Repositories ▾';
  renderRepoMenu();
}

function updateCounts(shownNodes, shownEdges, totalNodes, totalEdges) {
  const n = shownNodes === totalNodes ? `${totalNodes} nodes` : `${shownNodes} of ${totalNodes} nodes`;
  const e = shownEdges === totalEdges ? `${totalEdges} edges` : `${shownEdges} of ${totalEdges} edges`;
  countsEl.textContent = `${n} · ${e}`;
}

// ---------------------------------------------------------------------------
// Full-path toggle button
// ---------------------------------------------------------------------------

/**
 * Updates the visual state of the full-path button and re-renders node labels.
 */
function updateFullPathButton() {
  if (showFullPath) {
    btnFullPath.classList.add('active');
    btnFullPath.title = 'Showing full workspace-relative paths — click to show filenames only';
  } else {
    btnFullPath.classList.remove('active');
    btnFullPath.title = 'Toggle between filename and full workspace-relative path labels';
  }
}

btnFullPath.addEventListener('click', () => {
  showFullPath = !showFullPath;
  updateFullPathButton();
  // Re-render node labels in place without rebuilding the whole graph
  updateNodeLabels();
});

/**
 * Returns the display label for a node based on the current showFullPath setting.
 * @param {object} d  node datum
 * @returns {string}
 */
function nodeLabel(d) {
  if (showFullPath && d.relativePath) {
    return d.relativePath;
  }
  return d.label;
}

/**
 * Updates only the text labels of existing nodes (no simulation restart needed).
 */
function updateNodeLabels() {
  d3.select(nodesLayer).selectAll('g.node').select('text.label')
    .text(d => truncate(nodeLabel(d), showFullPath ? 40 : 24));
}

// ---------------------------------------------------------------------------
// Context menu — dismiss on outside click or Escape
// ---------------------------------------------------------------------------
document.addEventListener('click', () => hideCtxMenu());
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hideCtxMenu(); });

function showCtxMenu(event, d) {
  event.preventDefault();
  event.stopPropagation();
  if (nativeChrome) {
    vscode.postMessage({ type: 'contextMenu', filePath: d.filePath || '', kind: d.kind || '' });
    return;
  }

  // Build menu items
  const items = ctxMenu.querySelectorAll('.ctx-item');
  items.forEach(el => el.remove());

  if (d.filePath && d.kind !== 'missing' && d.kind !== 'unknown') {
    addCtxItem('Focus graph here', () => {
      vscode.postMessage({ type: 'focusFile', filePath: d.filePath });
    });
    addCtxItem('Open file', () => {
      vscode.postMessage({ type: 'openFile', filePath: d.filePath });
    });
    addCtxItem('Open to the side', () => {
      vscode.postMessage({ type: 'openFileBeside', filePath: d.filePath });
    });
  }
  if (d.filePath) {
    addCtxItem('Copy path', () => {
      vscode.postMessage({ type: 'copyPath', text: d.filePath });
    });
  }

  if (!ctxMenu.querySelector('.ctx-item')) return; // nothing to show

  const container = document.getElementById('graph-container');
  const rect = container.getBoundingClientRect();
  let x = event.clientX - rect.left;
  let y = event.clientY - rect.top;

  ctxMenu.style.display = 'block';
  // Clamp so menu stays inside container
  const mw = ctxMenu.offsetWidth  || 160;
  const mh = ctxMenu.offsetHeight || 80;
  if (x + mw > rect.width  - 4) x = rect.width  - mw - 4;
  if (y + mh > rect.height - 4) y = rect.height - mh - 4;
  ctxMenu.style.left = `${x}px`;
  ctxMenu.style.top  = `${y}px`;
}

function addCtxItem(label, onClick) {
  const el = document.createElement('div');
  el.className = 'ctx-item';
  el.textContent = label;
  el.addEventListener('click', (e) => {
    e.stopPropagation();
    hideCtxMenu();
    onClick();
  });
  ctxMenu.appendChild(el);
}

function hideCtxMenu() {
  ctxMenu.style.display = 'none';
}

// ---------------------------------------------------------------------------
// Message handler (from extension host)
// ---------------------------------------------------------------------------
window.addEventListener('message', (event) => {
  const msg = event.data;
  switch (msg.type) {
    case 'searchData':
      // Receive the full file list for client-side search filtering
      searchIndex = msg.items || [];
      break;

    case 'graphData':
      // Sync file-scope state from the extension (e.g. on first load)
      if (typeof msg.fileScopeEnabled === 'boolean' && msg.fileScopeEnabled !== fileScopeEnabled) {
        fileScopeEnabled = msg.fileScopeEnabled;
        updateFileScopeButton();
      }

      // Sync depth from the extension (e.g. on first load or panel open)
      if (typeof msg.graphDepth === 'number' && msg.graphDepth !== graphDepth) {
        graphDepth = msg.graphDepth;
      }
      if (typeof msg.direction === 'string') graphDirection = msg.direction;
      updateDepthControls();

      if (msg.filter) filter = Object.assign({ problemsOnly: false, kinds: [], repos: [] }, msg.filter);
      facets = msg.facets || { kinds: {}, problems: 0, repos: [] };
      updateFilterBar();

      // Track the focal file for upstream/downstream colouring
      scopedFilePath = msg.scopedFile || null;

      renderGraph(msg.nodes, msg.edges, msg.scopedFile);
      updateCounts((msg.nodes || []).length, (msg.edges || []).length,
        typeof msg.totalNodes === 'number' ? msg.totalNodes : (msg.nodes || []).length,
        typeof msg.totalEdges === 'number' ? msg.totalEdges : (msg.edges || []).length);
      break;
    case 'fit':
      fitView();
      break;
    case 'setFullPath':
      showFullPath = !!msg.enabled;
      updateFullPathButton();
      updateNodeLabels();
      break;
    case 'noWorkspace':
      showEmpty('Open a workspace folder to explore template dependencies.');
      break;
    case 'error':
      showEmpty(`Error: ${msg.message}`);
      break;
    default:
      break;
  }
});

// Signal to the extension that we're ready
vscode.postMessage({ type: 'ready' });

// ---------------------------------------------------------------------------
// Hierarchical layout helpers
// ---------------------------------------------------------------------------

/**
 * Assigns a BFS depth layer to every node, starting from pipeline roots.
 * Nodes not reachable from any root get layer = max_layer + 1.
 *
 * @param {object[]} nodes
 * @param {object[]} edges  – raw edge objects (source/target are ids at this point)
 * @returns {Map<string, number>}  nodeId → layer
 */
function computeLayers(nodes, edges) {
  // Build adjacency list (directed: source → target)
  /** @type {Map<string, string[]>} */
  const adj = new Map(nodes.map(n => [n.id, []]));
  for (const e of edges) {
    const sid = typeof e.source === 'object' ? e.source.id : e.source;
    const tid = typeof e.target === 'object' ? e.target.id : e.target;
    if (adj.has(sid)) adj.get(sid).push(tid);
  }

  const layer = new Map();
  const queue = [];

  // Seed BFS from pipeline roots
  for (const n of nodes) {
    if (n.kind === 'pipeline') {
      layer.set(n.id, 0);
      queue.push(n.id);
    }
  }

  // BFS
  let head = 0;
  while (head < queue.length) {
    const cur = queue[head++];
    const curLayer = layer.get(cur);
    for (const nxt of (adj.get(cur) || [])) {
      if (!layer.has(nxt) || layer.get(nxt) < curLayer + 1) {
        layer.set(nxt, curLayer + 1);
        queue.push(nxt);
      }
    }
  }

  // Assign orphan nodes to a layer after the deepest known layer
  const maxLayer = layer.size > 0 ? Math.max(...layer.values()) : 0;
  for (const n of nodes) {
    if (!layer.has(n.id)) layer.set(n.id, maxLayer + 1);
  }

  return layer;
}

/**
 * Computes initial (x, y) positions using a top-down hierarchical layout.
 * Nodes in the same layer are spread horizontally with even spacing.
 *
 * @param {object[]} nodes
 * @param {Map<string, number>} layerMap
 * @param {number} width   – canvas width
 * @param {number} height  – canvas height
 */
function applyHierarchicalPositions(nodes, layerMap, width, height) {
  // Group nodes by layer
  /** @type {Map<number, object[]>} */
  const byLayer = new Map();
  for (const n of nodes) {
    const l = layerMap.get(n.id) ?? 0;
    if (!byLayer.has(l)) byLayer.set(l, []);
    byLayer.get(l).push(n);
  }

  const layers = [...byLayer.keys()].sort((a, b) => a - b);
  const numLayers = layers.length;
  // Use a minimum of 140px between layers so nodes don't crowd vertically
  const minLayerGap = 140;
  const layerGap = numLayers > 1
    ? Math.max(minLayerGap, (height * 0.85) / (numLayers - 1))
    : 0;
  const topPad = height * 0.075;

  for (const l of layers) {
    const group = byLayer.get(l);
    const count = group.length;
    // Ensure at least 160px between nodes in the same layer
    const minXGap = 160;
    const naturalXGap = count > 1 ? (width * 0.85) / (count - 1) : 0;
    const xGap   = count > 1 ? Math.max(minXGap, naturalXGap) : 0;
    const xStart = count > 1 ? width / 2 - (xGap * (count - 1)) / 2 : width / 2;

    group.forEach((n, i) => {
      n.x = xStart + i * xGap;
      n.y = topPad + l * layerGap;
      // No jitter — let the simulation handle fine-tuning from clean positions
    });
  }
}

// ---------------------------------------------------------------------------
// Main render function
// ---------------------------------------------------------------------------

/**
 * Returns the stroke colour for an edge based on its direction property.
 * @param {object} e  edge datum
 * @returns {string}
 */
function edgeColor(e) {
  if (e.direction === 'upstream')   return '#e09a3d';  // amber — caller → focal
  if (e.direction === 'downstream') return '#4e9de0';  // blue  — focal → callee
  return '#666';                                        // grey  — workspace-wide
}

/**
 * @param {import('../graphWebViewProvider').GraphNode[]} nodes
 * @param {import('../graphWebViewProvider').GraphEdge[]} edges
 */
function renderGraph(nodes, edges, scopedFile) {
  hideNodeTip();
  if (!nodes || nodes.length === 0) {
    allNodes = [];
    allEdges = [];
    if (simulation) simulation.stop();
    d3.select(edgesLayer).selectAll('*').remove();
    d3.select(edgeLabels).selectAll('*').remove();
    d3.select(nodesLayer).selectAll('*').remove();
    if (isFiltered()) {
      showEmpty('No nodes match the filters.');
    } else if (fileScopeEnabled) {
      showEmpty('Open a YAML pipeline file to scope the graph to it.');
    } else {
      showEmpty();
    }
    return;
  }

  hideEmpty();

  allNodes = nodes.map(n => Object.assign({}, n));
  allEdges = edges.map(e => Object.assign({}, e));

  // Clear previous render
  d3.select(edgesLayer).selectAll('*').remove();
  d3.select(edgeLabels).selectAll('*').remove();
  d3.select(nodesLayer).selectAll('*').remove();

  const width  = svgEl.clientWidth  || 600;
  const height = svgEl.clientHeight || 500;

  // ── Compute hierarchical seed positions ─────────────────────────────────
  const layerMap = computeLayers(allNodes, allEdges);
  applyHierarchicalPositions(allNodes, layerMap, width, height);

  // ── Force simulation ─────────────────────────────────────────────────────
  if (simulation) simulation.stop();

  // Layer-based y-positioning force: match the same gap used in seed positions
  const numLayers = Math.max(...layerMap.values()) + 1;
  const minLayerGap = 140;
  const layerGap  = numLayers > 1
    ? Math.max(minLayerGap, (height * 0.85) / (numLayers - 1))
    : 0;
  const topPad    = height * 0.075;

  simulation = d3.forceSimulation(allNodes)
    .force('link', d3.forceLink(allEdges)
      .id(d => d.id)
      // Longer link distances keep connected nodes well separated
      .distance(d => {
        const target = typeof d.target === 'object' ? d.target : allNodes.find(n => n.id === d.target);
        return target && target.kind === 'external' ? 220 : 160;
      })
      // Weaker link strength so layer/collision forces can dominate
      .strength(0.25)
    )
    .force('charge', d3.forceManyBody()
      // Stronger repulsion pushes nodes apart more aggressively
      .strength(-800)
      .distanceMax(600)
    )
    .force('collide', d3.forceCollide()
      // Account for node circle + label text width (~70px) below the circle
      .radius(d => radius(d) + 55)
      .strength(1.0)
      .iterations(4)
    )
    // Weak horizontal centering — just enough to keep the graph from drifting
    .force('x', d3.forceX(width / 2).strength(0.02))
    // Strong vertical layer attraction — keeps nodes in their assigned row
    .force('y', d3.forceY(d => {
      const l = layerMap.get(d.id) ?? 0;
      return topPad + l * layerGap;
    }).strength(0.6))
    // Faster cooling — nodes settle in ~1 s instead of ~5 s
    .alphaDecay(0.04)
    .velocityDecay(0.35)
    .on('tick', ticked);

  // ── Edges ────────────────────────────────────────────────────────────────
  const edgeSel = d3.select(edgesLayer)
    .selectAll('line')
    .data(allEdges)
    .enter()
    .append('line')
    .attr('stroke', d => edgeColor(d))
    .attr('stroke-width', d => (d.direction === 'upstream' || d.direction === 'downstream') ? 2 : 1.5)
    .attr('stroke-opacity', d => (d.direction === 'upstream' || d.direction === 'downstream') ? 0.75 : 0.6)
    .attr('stroke-dasharray', d => d.direction === 'upstream' ? '5,3' : null)
    .attr('marker-end', d => d.direction === 'upstream' ? 'url(#arrow-upstream)' : 'url(#arrow)');

  // Edge labels (only for cross-repo edges that have a label)
  const edgeLabelSel = d3.select(edgeLabels)
    .selectAll('text')
    .data(allEdges.filter(e => e.label))
    .enter()
    .append('text')
    .attr('font-size', 9)
    .attr('fill', d => edgeColor(d))
    .attr('text-anchor', 'middle')
    .attr('dy', -3)
    .text(d => d.label);

  // ── Nodes ────────────────────────────────────────────────────────────────
  const nodeGroup = d3.select(nodesLayer)
    .selectAll('g.node')
    .data(allNodes)
    .enter()
    .append('g')
    .attr('class', 'node')
    .style('cursor', d => (d.filePath && d.kind !== 'missing') ? 'pointer' : 'default')
    .call(
      d3.drag()
        .on('start', dragStarted)
        .on('drag',  dragged)
        .on('end',   dragEnded)
    );

  // Focal-node outer ring (only in file-scope mode for the scoped file)
  nodeGroup.filter(d => scopedFile && d.filePath === scopedFile)
    .append('circle')
    .attr('r', d => radius(d) + 5)
    .attr('fill', 'none')
    .attr('stroke', 'var(--vscode-focusBorder, #007fd4)')
    .attr('stroke-width', 2)
    .attr('stroke-dasharray', '4,2')
    .attr('opacity', 0.8)
    .attr('pointer-events', 'none');

  // Context nodes (callers kept only to show who uses a filtered node) are faded.
  nodeGroup.style('opacity', d => d.context ? 0.45 : 1);

  // Circle
  nodeGroup.append('circle')
    .attr('r', d => radius(d))
    .attr('fill', d => KIND_COLOR[d.kind] || '#888')
    .attr('stroke', d => KIND_STROKE[d.kind] || '#555')
    .attr('stroke-width', 2)
    .attr('filter', d => d.kind === 'pipeline' ? 'url(#shadow)' : null);

  // Icon text inside circle
  nodeGroup.append('text')
    .attr('text-anchor', 'middle')
    .attr('dominant-baseline', 'central')
    .attr('font-size', d => Math.round(Math.max(9, radius(d) * 0.7)))
    .attr('fill', '#fff')
    .attr('pointer-events', 'none')
    .text(d => kindIcon(d.kind));

  // Problem badge: red for errors, amber for warnings
  nodeGroup.filter(d => !d.broken && (d.errors > 0 || d.warnings > 0))
    .append('circle')
    .attr('class', 'badge')
    .attr('cx', d => radius(d) * 0.72)
    .attr('cy', d => -radius(d) * 0.72)
    .attr('r', 4.5)
    .attr('fill', d => d.errors > 0 ? '#e05c5c' : '#e0b03d')
    .attr('stroke', 'var(--vscode-editor-background, #1e1e1e)')
    .attr('stroke-width', 1.5)
    .attr('pointer-events', 'none');

  // Label below circle
  nodeGroup.append('text')
    .attr('class', 'label')
    .attr('text-anchor', 'middle')
    .attr('dy', d => radius(d) + 13)
    .attr('font-size', 10)
    .attr('fill', 'var(--vscode-editor-foreground)')
    .attr('pointer-events', 'none')
    .text(d => truncate(nodeLabel(d), showFullPath ? 40 : 24));

  // ── Interactions ──────────────────────────────────────────────────────────
  nodeGroup
    .on('click', (event, d) => {
      event.stopPropagation();
      hideCtxMenu();
      if (d.filePath && d.kind !== 'missing' && d.kind !== 'unknown') {
        vscode.postMessage({ type: 'openFile', filePath: d.filePath });
      }
    })
    .on('contextmenu', (event, d) => {
      showCtxMenu(event, d);
    })
    .on('dblclick', (event, node) => {
      event.stopPropagation();
      node.fx = null;
      node.fy = null;
      if (simulation) simulation.alpha(0.3).restart();
    })
    .on('mouseover', (event, d) => {
      highlightNeighbours(d, edgeSel, nodeGroup);
      showNodeTip(event, d);
    })
    .on('mousemove', (event) => moveNodeTip(event))
    .on('mouseout', () => {
      resetHighlight(edgeSel, nodeGroup);
      hideNodeTip();
    });

  // Click on background → dismiss context menu
  svg.on('click', () => {
    resetHighlight(edgeSel, nodeGroup);
    hideCtxMenu();
  });

  // ── Tick function ──────────────────────────────────────────────────────────
  // Edges run between circle rims so arrowheads aren't hidden under big nodes.
  function rim(d, end) {
    const dx = d.target.x - d.source.x;
    const dy = d.target.y - d.source.y;
    const len = Math.hypot(dx, dy) || 1;
    if (end) {
      const r = radius(d.target) + 2;
      return [d.target.x - (dx / len) * r, d.target.y - (dy / len) * r];
    }
    const r = radius(d.source);
    return [d.source.x + (dx / len) * r, d.source.y + (dy / len) * r];
  }

  function ticked() {
    edgeSel
      .attr('x1', d => rim(d, false)[0])
      .attr('y1', d => rim(d, false)[1])
      .attr('x2', d => rim(d, true)[0])
      .attr('y2', d => rim(d, true)[1]);

    edgeLabelSel
      .attr('x', d => (d.source.x + d.target.x) / 2)
      .attr('y', d => (d.source.y + d.target.y) / 2);

    nodeGroup.attr('transform', d => `translate(${d.x},${d.y})`);
  }

  // Fit early (nodes are roughly in place after ~800 ms) and again when fully settled
  setTimeout(() => fitView(), 800);
  simulation.on('end', () => fitView());
}

// ---------------------------------------------------------------------------
// Node hover card (text written by the server)
// ---------------------------------------------------------------------------
const nodeTip = document.getElementById('node-tip');

function showNodeTip(event, d) {
  const lines = (d.tooltip || d.relativePath || d.label || '').split('\n');
  nodeTip.textContent = '';
  const title = document.createElement('div');
  title.className = 'tip-title';
  title.textContent = lines[0];
  nodeTip.appendChild(title);
  if (lines.length > 1) nodeTip.appendChild(document.createTextNode(lines.slice(1).join('\n')));
  nodeTip.style.display = 'block';
  moveNodeTip(event);
}

function moveNodeTip(event) {
  if (nodeTip.style.display !== 'block') return;
  const rect = document.getElementById('graph-container').getBoundingClientRect();
  let x = event.clientX - rect.left + 14;
  let y = event.clientY - rect.top + 14;
  if (x + nodeTip.offsetWidth > rect.width - 4) x = Math.max(4, event.clientX - rect.left - nodeTip.offsetWidth - 10);
  if (y + nodeTip.offsetHeight > rect.height - 4) y = Math.max(4, event.clientY - rect.top - nodeTip.offsetHeight - 10);
  nodeTip.style.left = `${x}px`;
  nodeTip.style.top = `${y}px`;
}

function hideNodeTip() {
  nodeTip.style.display = 'none';
}

// ---------------------------------------------------------------------------
// Drag handlers
// ---------------------------------------------------------------------------
function dragStarted(event, d) {
  if (!event.active && simulation) simulation.alphaTarget(0.3).restart();
  d.fx = d.x;
  d.fy = d.y;
}

function dragged(event, d) {
  d.fx = event.x;
  d.fy = event.y;
}

function dragEnded(event) {
  if (!event.active && simulation) simulation.alphaTarget(0);
  // Keep node pinned after drag
}

// ---------------------------------------------------------------------------
// Highlight / dim
// ---------------------------------------------------------------------------
function highlightNeighbours(d, edgeSel, nodeGroup) {
  const connectedIds = new Set([d.id]);
  allEdges.forEach(e => {
    const sid = typeof e.source === 'object' ? e.source.id : e.source;
    const tid = typeof e.target === 'object' ? e.target.id : e.target;
    if (sid === d.id) connectedIds.add(tid);
    if (tid === d.id) connectedIds.add(sid);
  });

  edgeSel
    .attr('stroke-opacity', e => {
      const sid = typeof e.source === 'object' ? e.source.id : e.source;
      const tid = typeof e.target === 'object' ? e.target.id : e.target;
      return (sid === d.id || tid === d.id) ? 1 : 0.05;
    })
    .attr('stroke-width', e => {
      const sid = typeof e.source === 'object' ? e.source.id : e.source;
      const tid = typeof e.target === 'object' ? e.target.id : e.target;
      return (sid === d.id || tid === d.id) ? 2.5 : 1.5;
    });

  nodeGroup.style('opacity', n => connectedIds.has(n.id) ? 1 : 0.12);
}

function resetHighlight(edgeSel, nodeGroup) {
  edgeSel
    .attr('stroke-opacity', e => (e.direction === 'upstream' || e.direction === 'downstream') ? 0.75 : 0.6)
    .attr('stroke-width',   e => (e.direction === 'upstream' || e.direction === 'downstream') ? 2 : 1.5);
  nodeGroup.style('opacity', n => n.context ? 0.45 : 1);
}

// ---------------------------------------------------------------------------
// Fit view
// ---------------------------------------------------------------------------
function fitView() {
  if (allNodes.length === 0) return;

  const padding = 50;
  const w = svgEl.clientWidth  || 600;
  const h = svgEl.clientHeight || 500;

  // Bounds include each node's circle and the label below it.
  const placed = allNodes.filter(n => n.x != null && isFinite(n.x) && n.y != null && isFinite(n.y));
  if (placed.length === 0) return;
  let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
  for (const n of placed) {
    const r = radius(n);
    minX = Math.min(minX, n.x - r);
    maxX = Math.max(maxX, n.x + r);
    minY = Math.min(minY, n.y - r);
    maxY = Math.max(maxY, n.y + r + 16);
  }
  minX -= padding; maxX += padding; minY -= padding; maxY += padding;

  const scaleX = w / (maxX - minX);
  const scaleY = h / (maxY - minY);
  const scale  = Math.min(scaleX, scaleY, 2);

  const tx = (w - scale * (minX + maxX)) / 2;
  const ty = (h - scale * (minY + maxY)) / 2;

  svg.transition().duration(500).call(
    zoom.transform,
    d3.zoomIdentity.translate(tx, ty).scale(scale)
  );
}

// ---------------------------------------------------------------------------
// Empty state
// ---------------------------------------------------------------------------
function showEmpty(msg) {
  emptyState.style.display = 'flex';
  const lines = emptyState.querySelectorAll('div:not(.icon)');
  if (!emptyState.dataset.defaults) {
    emptyState.dataset.defaults = JSON.stringify([...lines].map(l => l.textContent));
  }
  const defaults = JSON.parse(emptyState.dataset.defaults);
  lines.forEach((l, i) => {
    l.textContent = msg ? (i === 0 ? msg : '') : defaults[i];
    l.style.display = l.textContent ? '' : 'none';
  });
}

function hideEmpty() {
  emptyState.style.display = 'none';
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
function kindIcon(kind) {
  switch (kind) {
    case 'pipeline': return '▶';
    case 'external': return '⬡';
    case 'missing':  return '✕';
    case 'unknown':  return '?';
    default:         return '◆';
  }
}

function isFiltered() {
  return !!(filter.problemsOnly || (filter.kinds && filter.kinds.length) || (filter.repos && filter.repos.length));
}

function truncate(str, max) {
  return str.length > max ? str.slice(0, max - 1) + '…' : str;
}
