# CLAUDE.md

Monorepo for Azure Templates Navigator: one Go engine (`server/`, binary `atn`) and two thin IDE clients.
See README.md for the architecture diagram and the user-facing behaviour.

## Golden rule

**Features go in the server.** Hover, completion, diagnostics, quick fixes, resolution, tree and graph data all live in `server/internal/engine`. The clients only:

- start the binary
- send settings
- render custom views (trees, graph webview)
- implement client-side commands (open file, quick pick)

If you find yourself writing YAML parsing or template logic in JS or Kotlin, stop and add it to the engine (plus a custom `atn/*` request if a view needs it).

## Layout

- **`server/internal/pipeline`**: pure line-based YAML helpers. No I/O, and every function is unit-tested. Columns are byte offsets.
- **`server/internal/repos`**: clone URL inference (`urls.go`) and the fetch/cache manager (`manager.go`). It shells out to `git` with prompts disabled.
- **`server/internal/engine`**: the index (file cache, overlay of open docs, forward/reverse edges, alias registry), `resolve.go`, `diagnostics.go`, `actions.go`, `features.go` (hover/definition/completion) and `views.go` (tree/graph/lists). One mutex: public methods lock it, lower-case helpers assume it is held.
- **`server/internal/lsp`**: JSON-RPC framing and the LSP methods. It converts between UTF-16 positions and byte columns.
  - Standard methods beyond hover/definition/completion/codeAction: `documentSymbol` (outline: `pipeline/outline.go`), `references`, `codeLens` ("N usages" above a template), `prepareCallHierarchy` + `callHierarchy/incomingCalls|outgoingCalls`. Logic is in `engine/navigation.go`; `lsp/navigation.go` only converts.
  - Custom requests: `atn/dependencyTree`, `atn/graph`, `atn/workspaceDiagnostics`, `atn/templates`, `atn/parameters`, `atn/repositories`, `atn/references` (usages with labels, for client-drawn popups), `atn/info`.
  - Notification: `atn/didChange`.
  - Commands: `atn.fetchRepository`, `atn.fetchAllRepositories`, `atn.refresh`.
- **`server/cmd/atn`**: the CLI (`lsp`, `check`, `fetch`, `repos`, `version`).
- **`shared/webview`**: the graph page.
  - `graph.html` has `{{HEAD}}` and `{{SCRIPTS}}` placeholders, filled by each host.
  - `graph.js` talks to `window.atnHost || acquireVsCodeApi()`.
  - Message types: ready, setFileScope, setDepth, openFile, openFileBeside, copyPath, expand, contextMenu → graphData, searchData, fit, setFullPath.
  - `window.atnHost.nativeChrome = true` (IntelliJ) hides the page's own search bar and toolbar, and sends `contextMenu` instead of drawing a menu. The host provides those natively.
- **`clients/vscode`**: plain JS with `vscode-languageclient`.
  - `extension.js`: client and commands. `src/views.js`: tree providers. `src/graphView.js`: the webview.
  - `scripts/copy-assets.js` copies `shared/webview` → `media/` and `dist/bin` → `bin/`. Both are generated, so don't edit them.
- **`clients/intellij`**: Kotlin with LSP4IJ.
  - `lsp/AtnLsp.kt`: factory, connection, client and binary lookup. `AtnService.kt`: requests and the change topic. `settings/`, `ui/` (tool window tabs, JCEF graph, actions).
  - `ui/Navigation.kt`: the native pieces (usages popup, `ShowUsagesAction`, gutter icons on `template:`, Structure view routing).
  - The look should be IntelliJ's, not a reskin of VS Code. Prefer an IntelliJ surface fed by an LSP feature (Find Usages, Call Hierarchy, Structure, Code Vision) over a custom panel. LSP4IJ registers some providers only for TEXT/TextMate, so `plugin.xml` registers them for `yaml`.
  - Code lens commands are `<commandPrefix>showUsages`. LSP4IJ runs the IntelliJ action with that id, `AzureTemplatesNavigator.showUsages`, and passes the arguments in `CommandExecutor.LSP_COMMAND`. VS Code registers `azure-templates-navigator.showUsages`.
  - Gradle copies `shared/webview` into resources and `dist/bin` into the sandbox and zip.

## Commands

```bash
scripts/build.sh dev        # build atn for this machine (dist/bin/<host>) + copy assets to VS Code client
scripts/build.sh test       # go vet+test, VS Code E2E (real VS Code), IntelliJ unit tests
cd server && go test ./...  # fast loop
dist/bin/darwin-arm64/atn check --root samples   # end-to-end without an IDE
cd clients/intellij && ./gradlew runIde --args="$(cd ../../samples && pwd)"   # logs: build/idea-sandbox/*/log/idea.log, prefix [atn]
```

`scripts/build.sh dev` wipes `dist/bin` and builds only the host target. Run `scripts/build.sh server` before packaging.

## Conventions

- **Go:** keep it stdlib-only and Go 1.20 compatible (no `slices`, `maps`, `min` or `max`). Run `gofmt`.
- **Settings:** add a setting in three places, then run both parity tests (`engine/parity_test.go`, `SettingsParityTest.kt`):
  1. `engine.Settings` / `repos.Config`, with camelCase JSON tags
  2. `clients/vscode/package.json`
  3. `AtnSettings.Data` + `toServerJson()` + the configurable UI (`requiredParameterColor` has no IntelliJ UI row, because IntelliJ hovers are plain markdown)
- **Diagnostic codes** are part of the public surface (CLI JSON, quick fixes). Don't rename them.
- **Hover command links** are only emitted when the client sends `client.commandLinks: true` (VS Code). They are prefixed with `client.commandPrefix`. IntelliJ gets plain markdown, so anything a link does must also exist as a code action or IDE action there.
- **`client.plainMarkdown: true`** (IntelliJ) strips emoji and inline HTML from hovers and completion docs (`engine.Plain`), because they don't render well in IntelliJ's documentation popup.
- **`extends:` call sites:** `template:` and `parameters:` are sibling keys in the same mapping, in either order. Look up a call's parameters through `pipeline.MappingSiblings`, never by "lines indented deeper than the template line".
- **Block scalars are text, not structure.** A `template:` line inside `script: |` is shell. Structural scanners skip those lines through `pipeline.BlockScalarMask`, but `${{ parameters.x }}` scanners must keep reading them. In the engine, look up a reference with `f.refAt(line)`, never `pipeline.TemplateRefOnLine` on a raw line.
- **Call-site parameters come in three forms:** block, flow (`parameters: { a: 1 }`, possibly multi-line) and forwarded (`parameters: ${{ parameters }}`, so `CallSite.Unknowable()`). Use `PassedParam` positions (`ValueLine`/`ValueCol`/`ValueEnd`, `DelStart`/`DelEnd` for flow), never "to the end of the line".
- **Index freshness:** document and file events call `markDirty`, and `ensureLinked` re-links only those files. Set `e.linked = false` (full rebuild) only for changes that affect every file: settings, roots, repository state, alias declarations. `engine/incremental_test.go` checks that incremental results equal a full rebuild; extend it when adding a dependency between files.
- **Samples:** `samples/extends/` models platform teams publishing templates and app teams extending them. Its README lists the exact expected diagnostics; keep that list in sync when adding checks.
- **Versioning:** a single `VERSION` file. `build.sh` syncs it into `package.json` and `gradle.properties`.
- **Tests for repository fetching** use `file://` URL overrides and local bare repos. No network.

## Releasing

Releases are published by `.github/workflows/release.yml` when a `vX.Y.Z` tag is pushed. Nothing is published by hand.

1. Set `VERSION` to the new version.
2. In `CHANGELOG.md`, change the heading to `## [X.Y.Z] — YYYY-MM-DD`. The workflow refuses a tag whose section is missing or still says `unreleased`, and uses that section as the GitHub release notes.
3. Commit, then `git tag vX.Y.Z && git push origin main vX.Y.Z`.

The workflow runs `scripts/build.sh test`, builds the six platform VSIXs and the IntelliJ zip, then publishes to the VS Code Marketplace, to Open VSX (only if `OVSX_PAT` is set), to a GitHub release (VSIXs, plugin zip, `atn-*` CLI binaries) and to the JetBrains Marketplace. JetBrains reviews each upload by hand, so it shows up there a day or two later. If a step fails, fix it and re-run the job: every step before JetBrains skips work already done.

- **Never change the extension IDs.** `publisher` + `name` in `clients/vscode/package.json` and `<id>` in `plugin.xml` tie the release to the existing Marketplace listings (installs, ratings, reviews). Changing them creates a new, empty listing.
- **Secrets** (repo Settings → Secrets and variables → Actions): `VSCE_PAT`, `JETBRAINS_MARKETPLACE_TOKEN`, optional `OVSX_PAT`.
