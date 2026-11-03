# Changelog

One changelog for the `atn` server, the VS Code extension and the IntelliJ plugin. They share a version.
For history before 2.0.0, see the CHANGELOGs of the former `azure-templates-vscode-extension` (1.x) and `azure-templates-intellij` (1.x) repositories.

## [2.0.0] — unreleased

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
