# Changelog

One changelog for the `atn` server, the VS Code extension and the IntelliJ plugin. They share a version.
For history before 2.0.0, see the CHANGELOGs of the former `azure-templates-vscode-extension` (1.x) and `azure-templates-intellij` (1.x) repositories.

## [Unreleased]

### Added

- **Graph nodes are sized by how connected they are.** Size follows callers + callees, counted over the whole workspace, so a widely used template stays big when the graph shows only the current file. The scale is a square root, so hubs don't swamp the view.
  - Hovering a node shows how many files and call sites use it, how many pipelines reach it, what it calls, its parameters, and its errors and warnings.
  - Files with errors or warnings get a red or amber badge.
- **Node and edge counts** in the corner of the graph: "23 of 87 nodes · 31 of 140 edges" when a filter is on.
- **Graph filters.**
  - *Problems*: only files with errors or warnings, and missing templates, unknown aliases or unavailable repositories.
  - *Kinds*: pipelines, templates, external templates, broken references.
  - *Repositories*: this workspace and each repository templates come from.
  - With *Problems* or *Repositories*, the direct callers of what matches stay visible, faded, so you can see who uses it.
  - Around the current file: callers only, callees only, or both, next to the depth.
  - VS Code: a filter bar under the graph toolbar, and *Focus graph here* on a node's menu. IntelliJ: *Only Problems*, a *Filter* popup and a direction combo in the Graph tab's toolbar.
- **Stats view** (VS Code: under Repositories; IntelliJ: a Stats tab), for teams that publish templates:
  - Overview: pipelines, templates, external templates, repositories, errors, warnings, broken references.
  - Most used: call sites, files and pipelines reached. These are the templates a parameter change affects most.
  - Most mis-called: templates whose call sites have the most parameter problems, often a sign of a confusing interface.
  - Calls the most templates.
  - Not referenced in this workspace: candidates for clean-up. Templates published for other repositories show up here too.
  - By repository: templates, call sites and files using each repository.
  - Click a row to open the file. *Show in Graph* centres the graph on it, filters it to the repository, or shows only problems.
- `atn/stats` request and new `atn/graph` parameters (`direction`, `problemsOnly`, `kinds`, `repos`). The response adds per-node usage numbers, `totalNodes`, `totalEdges` and `facets`.

## [2.2.0] — 2026-11-16

### Added

- **Report a bug or an idea from the IDE.** It opens a new GitHub issue with the extension, `atn`, IDE and OS versions filled in. Nothing is sent until you submit it.
  - VS Code: a *Help and Feedback* view in the Azure Templates sidebar, a bug button on the Dependencies and Diagnostics views, and *Azure Templates: Report Issue…*. The Marketplace page and *Help → Report Issue* link to the issue tracker too.
  - IntelliJ: a bug button in the Azure Templates tool window header, and *Help → Report Azure Templates Navigator Issue…*. When the plugin throws, IntelliJ's error dialog offers *Report to Azure Templates Navigator*, with the stack trace in the issue.
  - GitHub issue forms for bug reports and feature requests.
  - Both also link to the documentation and the log, and *Give Feedback* opens the extension's reviews on the Visual Studio Marketplace or JetBrains Marketplace.

## [2.1.0] — 2026-11-03

### Fixed

- **`template:` text inside a script is no longer treated as a template call.** Lines in a block scalar (`script: |`, `bash: >-`, `- |`, ...) are shell, not YAML structure.
  - Before, a script that printed `template: x.yml` got a "template not found" error, and its line showed up in usages, the call hierarchy and the graph.
  - The outline and variables lookup skip script bodies too.
- **Parameters passed as a flow mapping are recognised:** `parameters: { env: prod, count: 3 }`, on one line or over several.
  - Before, every required parameter was reported missing, and typos and wrong values were never flagged.
  - Quick fixes edit inside the braces: add missing parameters, remove or rename an unknown one, change a value.
  - Hover, go-to-definition and find usages work on the keys.
- **`parameters: ${{ parameters }}`** (forwarding everything to another template) no longer reports every required parameter as missing.
- **Type-mismatch and value-not-allowed ranges** stop at the value. Their quick fixes no longer delete a trailing comment.

### Performance

- **Edits re-check only what they affect.**
  - Changing a file re-links that file. It re-checks the file, its callers, and any file whose reference it makes appear or disappear.
  - Before, every keystroke rebuilt the call graph and re-checked the whole workspace. On a 2,200-file workspace that took about 33 ms per keystroke, against about 0.4 ms now.
  - A change to `resources.repositories` still rebuilds everything, because aliases resolve references in other files.
- **Only changed diagnostics are republished.**
- **The tree views and the graph reload only when something changed.** Both IDEs refresh them on the server's `atn/didChange` notification, which was sent after every keystroke.
- **Requests no longer queue behind each other.** The server reads document changes and `$/cancelRequest` while a request is running. Requests run on worker goroutines, and a request cancelled before it starts is answered with `RequestCancelled`.
- **YAML that isn't Azure Pipelines is left out of the index:** Kubernetes manifests, GitHub Actions workflows, docker-compose files, Helm values and the like. They get no diagnostics, graph nodes or template-path completions.

## [2.0.0] — 2026-11-03

### Architecture

- **One engine for both IDEs.** All logic moved into `atn`, a Go language server and CLI bundled with each plugin.
  - The VS Code extension and the IntelliJ plugin are now thin LSP clients, so features and fixes land in both at once.
  - Settings have the same names and meaning everywhere, enforced by parity tests.
- **One graph webview** is shared by both IDEs.

### Added

- **Automatic download of templates from other repositories** (`template: x.yml@alias`).
  - Supported types: GitHub, Azure Repos, GitHub Enterprise and Bitbucket.
  - Repositories are resolved via `localPaths`, then a sibling clone, then a shared cache (`~/.azure-templates-navigator/repos`).
  - The cache holds shallow fetches of the exact `ref` and refreshes in the background.
  - Downloads use the user's existing git credentials and never prompt.
  - **Fetch repository** is available as a hover link and as a quick fix. A new **Repositories** view shows each repository's state, commit and age.
  - Azure DevOps organization and project are inferred from the workspace remote.
  - Templates that use an alias they don't declare resolve it through the pipelines that do.
- **`.azure-templates-navigator.json`**: a committable, team-wide repository configuration.
- **New diagnostics:**
  - value not in `values:`
  - template file not found
  - unknown repository alias
  - repository unavailable or still downloading
  - "did you mean …" for misspelled parameter names
- **New quick fixes:**
  - rename a misspelled parameter
  - add all missing required parameters
  - pick an allowed value
  - create the `parameters:` block when it's missing
- **Completion:** template paths after `template:`, plus allowed values and booleans.
- **Hover:** works on passed parameters and on `${{ parameters.x }}` inside templates, and shows where an external repository was resolved from.
- **`atn` CLI:** `check` (text, JSON or Azure Pipelines annotations), `fetch`, `repos`.
- **IntelliJ:** *Search Template Parameters…* in the editor context menu, matching the VS Code hover link.

- **`extends:` templates are validated like `template:` calls.**
  - Before, parameters written next to `template:` were ignored, so every required parameter was reported missing and typos were never flagged.
  - Hover, completion and go-to-definition now also work inside `extends: parameters:`.
  - `parameters:` may come before or after `template:`.
- **New diagnostic `undeclared-param`:** `${{ parameters.x }}` where `x` is not declared. Azure evaluates it to an empty string instead of failing.
  - Quick fixes: change to the closest declared name, or declare the parameter (with `default: ''`, so callers keep today's behaviour).
- **Find usages** of a template, or of one of its parameters: the declaration, the uses in the template body, and every call site that passes it. In IntelliJ: Find Usages. In VS Code: Find All References.
- **Call hierarchy:** which pipelines and templates reference a template, and which templates it references. In IntelliJ: Ctrl+Alt+H. In VS Code: Show Call Hierarchy.
- **"N usages · M pipelines"** above every template (Code Vision in IntelliJ, a code lens in VS Code). Clicking it lists the call sites.
- **Outline of pipelines and templates:** parameters, variables, repositories, stages → jobs → steps, template calls and `${{ if }}` blocks. In IntelliJ: the Structure view. In VS Code: Outline and breadcrumbs.
- **`samples/extends/`:** an example organisation with platform and security template repos and four consumer teams, including the expected diagnostics.

### Changed

- **IntelliJ looks like IntelliJ:**
  - Find Usages, Call Hierarchy, Code Vision and the Structure view replace VS Code-style affordances.
  - Gutter icons on `template:` references.
  - The graph has an IntelliJ toolbar (follow current file, depth, full paths, fit, search) and IntelliJ context menus on nodes.
  - Documentation popups have no emoji or inline HTML.
  - New tool window icon.
- **Graph:** the context menu no longer shows raw icon codes such as `$(go-to-file)`.

- **IntelliJ:** requires 2024.2+ and the LSP4IJ plugin, which the Marketplace installs automatically. All views are now tabs of one **Azure Templates** tool window.
  - Settings are stored in a new file, so 1.x settings start from the 2.0 defaults. Set them again under *Settings → Tools → Azure Templates Navigator*.
- **VS Code:**
  - The `azure-templates-navigator.diagnostics.debounceMs` setting was removed, because the server now debounces changes itself. If you had set it, VS Code greys it out as an unknown setting in `settings.json`, and you can delete it.
  - Tree view filter toggles were simplified.
- **Unicode parameter names** are recognised everywhere.
