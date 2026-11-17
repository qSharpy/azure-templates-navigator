# Azure Templates Navigator

Hover, validate, complete and navigate **Azure Pipelines YAML templates**. This includes templates that live in other repositories, which are downloaded for you.

![The template graph around a template, with its callers and callees](https://raw.githubusercontent.com/qSharpy/azure-templates-navigator/main/docs/images/vscode/templateGraph.png)

Also available for JetBrains IDEs, with the same engine and the same settings, and as an `atn` CLI for CI.

**[Feature guide](https://github.com/qSharpy/azure-templates-navigator/blob/main/docs/README.md)**: every feature with screenshots and shortcuts. In VS Code, *Azure Templates: Get Started* walks through them one at a time.

## Features

### Will this pipeline run?

![The verdict above a pipeline](https://raw.githubusercontent.com/qSharpy/azure-templates-navigator/main/docs/images/vscode/pipelineVerdict.png)

Every pipeline shows a verdict above its first line. It covers the pipeline and every template the pipeline reaches. Click it to list the problems, each at the line that leads to it. The **Pipelines** view lists every pipeline in the workspace, failing first.

### Hover any `template:` line

![Hover on a template call](https://raw.githubusercontent.com/qSharpy/azure-templates-navigator/main/docs/images/vscode/hoverParameters.png)

- **Parameters.** Every parameter with its type, default and allowed values. Required ones are highlighted. A **Search** link filters long parameter lists and jumps to a declaration.
- **Repository.** For `x.yml@alias`, the hover shows which repository, ref and copy (local clone or cache) was used, and how old it is.
- **Fetch.** If the repository isn't available yet, the hover shows a **Fetch repository** link.

Hover also works on a passed parameter (`  env: prod`), on `${{ parameters.x }}` inside a template, and on `$(variable)` / `variables.x`.

### Validation and quick fixes

![A quick fix renames a misspelled parameter](https://raw.githubusercontent.com/qSharpy/azure-templates-navigator/main/docs/images/vscode/quickFixes.gif)

| Check | Quick fix (<kbd>Ctrl+.</kbd> / <kbd>⌘.</kbd>) |
|---|---|
| Missing required parameter | Add it (or add all missing ones at once) |
| Unknown parameter, with "did you mean …" | Rename it, or remove it |
| Wrong type (e.g. `'yes'` for a `boolean`) | Replace with the right literal |
| Value not in `values:` | Pick an allowed value |
| Parameter declared but never used | Remove the declaration |
| Template file not found / unknown `@alias` | — |
| Repository not downloaded | Fetch repository |

### Completion

![Completing a template path adds its required parameters](https://raw.githubusercontent.com/qSharpy/azure-templates-navigator/main/docs/images/vscode/completeRequired.gif)

- **Parameter names** under `parameters:`, with required ones first.
- **Allowed values** and booleans.
- **Template paths** after `template:`. Picking one also adds a `parameters:` block with the template's required parameters (Tab moves between the values). Turn it off with `completion.requiredParameters`.

### Who uses this template?

![Show Call Hierarchy on a template](https://raw.githubusercontent.com/qSharpy/azure-templates-navigator/main/docs/images/vscode/callHierarchy.png)

- **"N usages · M pipelines"** above a template lists its call sites.
- **Find All References** (<kbd>Shift+Alt+F12</kbd>) works on a template and on each of its parameters.
- **Show Call Hierarchy** (<kbd>Shift+Alt+H</kbd>) shows the whole chain, from the template up to every pipeline that reaches it, or from a pipeline down to everything it calls.
- The **Outline** view and breadcrumbs show a pipeline's stages, jobs, steps and template calls.

### Dependency tree, graph, stats and repositories

![The Stats view](https://raw.githubusercontent.com/qSharpy/azure-templates-navigator/main/docs/images/vscode/stats.png)

- **Dependencies** shows what the current file calls and who calls it.
- **Graph** is an interactive view of the current file or the whole workspace.
  - Nodes are sized by how many files call a template plus how many it calls, across the whole workspace. Hover a node for its usages, the pipelines that reach it, and its problems.
  - Click a node to light up everything that reaches it and everything it calls.
  - The filter bar shows only problems, some kinds of nodes, or some repositories. Around the current file you can draw callers, callees or both.
  - The corner shows how many nodes and edges are drawn.
- **Stats** is for teams that publish templates: problems by type, most used, most mis-called, calls the most templates, not referenced in this workspace, and per-repository counts. *Show in Graph* centres or filters the graph on a row.
- **Diagnostics** lists the problems across the workspace.
- **Repositories** lists every `resources.repositories` entry with its state, commit and age, the templates you use from it, and fetch and open actions.

### Shortcuts

| To | macOS | Windows, Linux |
|---|---|---|
| See a template's parameters | hover, or <kbd>⌘K</kbd> <kbd>⌘I</kbd> | hover, or <kbd>Ctrl+K</kbd> <kbd>Ctrl+I</kbd> |
| Open the template | <kbd>⌘</kbd>-click or <kbd>F12</kbd> | <kbd>Ctrl</kbd>-click or <kbd>F12</kbd> |
| Complete | <kbd>⌃Space</kbd> | <kbd>Ctrl+Space</kbd> |
| Quick fix | <kbd>⌘.</kbd> | <kbd>Ctrl+.</kbd> |
| Find who uses a template or parameter | <kbd>⇧⌥F12</kbd> | <kbd>Shift+Alt+F12</kbd> |
| Show Call Hierarchy | <kbd>⇧⌥H</kbd> | <kbd>Shift+Alt+H</kbd> |
| Go to a stage, job or step | <kbd>⇧⌘O</kbd> | <kbd>Ctrl+Shift+O</kbd> |

## Templates from other repositories

```yaml
resources:
  repositories:
    - repository: templates
      type: github                       # github · git (Azure Repos) · githubenterprise · bitbucket
      name: my-org/pipeline-templates
steps:
  - template: build.yml@templates        # ← hover works, no manual clone needed
```

The repository is resolved from, in order:

1. `repositories.localPaths`
2. a clone next to your repo, `../pipeline-templates`
3. the shared cache, `~/.azure-templates-navigator/repos/…`

The cache is a shallow, read-only git fetch of the exact `ref`. It uses your existing git credentials (SSH, Git Credential Manager, `gh auth setup-git`) and never prompts.

- **Azure Repos.** Organization and project are read from your repo's remote. If they can't be inferred, set `repositories.azureDevOpsOrganization`.
- **Team-wide settings.** Commit a `.azure-templates-navigator.json` next to your pipelines:

```json
{ "repositories": { "urls": { "templates": "git@github.com:my-org/pipeline-templates.git" } } }
```

## Settings

| Setting | Default | |
|---|---|---|
| `azure-templates-navigator.repositories.autoFetch` | `true` | Download referenced repositories automatically |
| `azure-templates-navigator.repositories.cacheDir` | `~/.azure-templates-navigator/repos` | Shared with the JetBrains plugin and the CLI |
| `azure-templates-navigator.repositories.refreshIntervalHours` | `12` | Re-fetch cached copies in the background (0 = never) |
| `azure-templates-navigator.repositories.preferLocalClones` | `true` | Use `../<repo>` when it exists |
| `azure-templates-navigator.repositories.urls` / `localPaths` | `{}` | Per alias, `org/repo` or repo name |
| `azure-templates-navigator.repositories.azureDevOpsOrganization` / `azureDevOpsProject` | | When it can't be inferred |
| `azure-templates-navigator.repositories.githubEnterpriseHost` | | For `type: githubenterprise` |
| `azure-templates-navigator.diagnostics.enabled` | `true` | |
| `azure-templates-navigator.diagnostics.unusedParameters` | `true` | |
| `azure-templates-navigator.completion.requiredParameters` | `true` | Add required parameters when completing a template path |
| `azure-templates-navigator.requiredParameterColor` | `#c92d35` | Hover highlight |
| `azure-templates-navigator.graph.rootPath` | | Sub-folder for the workspace graph |

## Commands

- *Azure Templates: Search Templates…*
- *Search Template Parameters…*
- *Fetch All Repositories*
- *Open Cache Folder*
- *Refresh*
- *Restart Language Server*
- *Show Output*
- *Report Issue…*
- *Give Feedback (Rate and Review)*
- *Get Started*, *What's New*

## Help and feedback

The **Help and Feedback** view at the bottom of the Azure Templates sidebar has everything in one place:

- **Report a Bug**: opens a new GitHub issue with the extension, `atn`, VS Code and OS versions filled in. Nothing is sent until you submit it.
- **Get Started** (a walkthrough of the features) and **What's New**.
- **Documentation**: the [feature guide](https://github.com/qSharpy/azure-templates-navigator/blob/main/docs/README.md), with screenshots and shortcuts. **Show Log** is worth attaching to a bug report.
- **Suggest a Feature**: a GitHub issue for ideas and missing checks.
- **Give Feedback**: rate and review the extension here on the Marketplace. It helps other people find it.

The bug button on the Dependencies and Diagnostics views does the same.

## Use it in CI

```bash
atn check --format azure   # emits ##vso[task.logissue] annotations, exit code 1 on errors
```

See the [project README](https://github.com/qSharpy/azure-templates-navigator#cli) for details.
