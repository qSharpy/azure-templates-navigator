# Azure Templates Navigator

IDE support for Azure Pipelines YAML templates, for VS Code and the JetBrains IDEs.

**[Feature guide](docs/README.md)**: every feature, with screenshots from both IDEs and the shortcuts for each.

| VS Code | IntelliJ |
|---|---|
| ![The verdict above a pipeline in VS Code](docs/images/vscode/pipelineVerdict.png) | ![Call Hierarchy in IntelliJ](docs/images/intellij/callHierarchy.png) |

It is built as **one engine with thin IDE shells**:

```
                 ┌─────────────────────────────────────────────┐
                 │  atn  (Go, single static binary, ~2.5 MB)   │
                 │  language server  +  CLI                    │
                 │  parsing · resolution · diagnostics ·       │
                 │  quick fixes · completion · tree · graph ·  │
                 │  repository download/cache                  │
                 └──────────────▲───────────────▲──────────────┘
                     LSP/stdio  │               │  LSP/stdio
          ┌─────────────────────┴──┐        ┌───┴────────────────────────┐
          │ clients/vscode         │        │ clients/intellij           │
          │ vscode-languageclient  │        │ LSP4IJ                     │
          │ tree views, commands   │        │ tool window, actions       │
          └──────────┬─────────────┘        └─────────────┬──────────────┘
                     └──── shared/webview (graph page, D3) ┘
```

All features — hover, go-to-definition, completion, diagnostics, quick fixes, the
dependency tree, the graph, cross-repository resolution — live in `server/`.
A feature is written once and appears in both IDEs and in the CLI. The clients
only start the binary, render the custom views, and map settings.

| Path | What |
|---|---|
| `server/` | Go module: `internal/pipeline` (YAML-aware line parsers), `internal/repos` (clone URLs, fetch, cache), `internal/engine` (index, resolution, all features), `internal/lsp` (JSON-RPC/LSP), `cmd/atn` (CLI) |
| `shared/webview/` | Graph page used by both IDEs (`window.atnHost` bridge in IntelliJ, `acquireVsCodeApi` in VS Code) |
| `clients/vscode/` | VS Code extension (publisher `bogdanbujor`, id `azure-templates-navigator`) |
| `clients/intellij/` | IntelliJ plugin (`com.bogdanbujor.azure-templates-navigator`) |
| `samples/` | Pipelines and templates used for manual testing and the VS Code E2E test |
| `scripts/build.sh` | Builds and tests everything |
| `VERSION` | Single version for server and both clients |

## Will this pipeline run?

Most pipeline failures can be seen in the YAML before anything runs. Each pipeline shows a
verdict above its first line, which covers the pipeline and every template it reaches,
including templates in other repositories:

```
✗ Will fail when queued: 2 errors, 1 warning      ← click to list the problems
trigger:
  branches: …
```

- **Errors** are what makes Azure reject the run when you click Run.
  - Template calls: missing or unknown parameters, values not in `values:`, missing templates and unknown aliases.
  - Aliases a template uses but the pipeline doesn't declare. Azure looks aliases up in the *pipeline's* `resources`.
  - GitHub, GitHub Enterprise and Bitbucket repositories declared without `endpoint:` (the service connection).
  - Names: `dependsOn` naming a stage or job that doesn't exist, invalid or duplicate stage and job names.
  - Steps templates used under `jobs:`, defaults that aren't in `values:`, and tabs in indentation.
- **Warnings** are what goes wrong once the run is under way.
  - Output variables read from a step that doesn't exist, or set without `isOutput=true`. They are always empty.
  - Runtime parameters without a default in a pipeline that a trigger or a schedule starts.
  - `${{ parameters.x }}` that evaluates to an empty string.

You don't have to open each pipeline. The **Pipelines** view (VS Code: in the Azure Templates sidebar;
IntelliJ: a tab of the Azure Templates tool window) lists every pipeline in the workspace with its verdict,
failing pipelines first, and "2 of 7 will fail" at the top. Expand a pipeline for its problems, in it and
in its templates, and click one to jump to it.

The same verdict is available in a terminal or a PR check: `atn check path/to/pipeline.yml`.
These checks read the YAML only. They can't know about things that live in Azure DevOps: service
connections, variable groups, environments, agent pools, permissions, or what your scripts do.

## Templates from other repositories

```yaml
resources:
  repositories:
    - repository: templates
      type: github            # or git (Azure Repos), githubenterprise, bitbucket
      name: my-org/pipeline-templates
      ref: refs/heads/main
steps:
  - template: build.yml@templates
```

Nobody has to clone `pipeline-templates` by hand. For `@templates` the engine looks, in order, at:

1. **`repositories.localPaths`**: an explicit folder for an alias, `org/repo` or repo name.
2. **A clone next to the current repository**, `../pipeline-templates`, when `preferLocalClones` is on (the default). Your working copy, with your unpushed edits, wins over the cache.
3. **The shared cache**, `~/.azure-templates-navigator/repos/<host>/<org>/[<project>/]<repo>/<ref>`. This is a shallow `git fetch --depth 1` of the referenced ref.
   - With `autoFetch` on (the default), the download starts as soon as a file references the repo. It is refreshed in the background every `refreshIntervalHours`.
   - With `autoFetch` off, or after a failure, the hover and the quick fix show **Fetch repository**. The Repositories view lists every repo with its state, commit and age, the templates the workspace uses from it (most used first) and the files that declare it.

The download uses your normal git setup (SSH keys, Git Credential Manager, `gh auth setup-git`, Azure DevOps PAT helpers). It never prompts, so a missing credential shows up as a clear message instead of a hung IDE.

**Clone URL inference.**

| `type:` | URL |
|---|---|
| `github` | `https://github.com/<name>.git`, or `git@github.com:` if your own origin uses SSH |
| `githubenterprise` | `https://<githubEnterpriseHost>/<name>.git` |
| `bitbucket` | `https://bitbucket.org/<name>.git` |
| `git` (Azure Repos) | `https://dev.azure.com/<org>/<project>/_git/<repo>`. Org and project come from your origin remote (dev.azure.com, visualstudio.com or SSH), from `name: Project/Repo`, or from the `azureDevOpsOrganization` / `azureDevOpsProject` settings. |

Anything else can be pinned with `repositories.urls`.

**Team configuration.** Commit `.azure-templates-navigator.json` at the repository root. Every teammate, in either IDE and in CI, then gets the same mapping:

```json
{
  "repositories": {
    "azureDevOpsOrganization": "contoso",
    "urls": { "templates": "git@ssh.dev.azure.com:v3/contoso/Platform/pipeline-templates" },
    "localPaths": { "shared": "../shared-templates" }
  }
}
```

IDE settings override the file. Relative paths are relative to the file.

**Templates that use other aliases.** A template inside the templates repo often says `template: x.yml@templates` without declaring `resources` itself. In that case the alias is looked up among the pipelines in your workspace that declare it.

## Where to find things in each IDE

Each feature is computed once by `atn` and shown through the IDE's own UI.

| Feature | IntelliJ | VS Code |
|---|---|---|
| Template parameters, types, defaults | Quick Documentation (hover) | Hover |
| Who uses this template / parameter | Find Usages (Alt+F7); "N usages" Code Vision above the template | Find All References (Shift+Alt+F12); "N usages" code lens |
| Pipeline ⇄ template chain | Call Hierarchy (Ctrl+Alt+H); the Azure Templates tool window | Show Call Hierarchy (Shift+Alt+H); the Azure Templates views |
| Stages / jobs / steps / template calls | Structure view (Cmd/Ctrl+F12) | Outline view, breadcrumbs |
| Jump to the referenced template | Ctrl/Cmd+click, or the gutter icon on `template:` | Ctrl/Cmd+click |
| Problems and fixes | Inspections + Alt+Enter; Diagnostics tab | Problems view + quick fixes |
| Will this pipeline run? | Pipelines tab (every pipeline's verdict, failing first); verdict Code Vision above a pipeline; *Check Pipeline Before Running* (editor right-click menu) | Pipelines view (every pipeline's verdict, failing first); verdict code lens above a pipeline; *Check Pipeline Before Running* (editor right-click menu, palette) |
| Template graph | Graph tab (IntelliJ toolbar and menus) | Graph panel |
| Graph filters: problems, kinds, repositories, callers/callees | *Only Problems*, *Filter* popup and direction combo in the Graph tab toolbar | Filter bar under the graph toolbar; direction next to *Depth* |
| Problems by type, most used / mis-called / unused templates, per-repository counts | Stats tab | Stats view |
| Tips, one feature at a time | *Help → Azure Templates Navigator Tips*; *Get Started* under the bug button in the tool window header; mixed into *Help → Tip of the Day*. After an update, a balloon offers *What's new* | *Get Started* walkthrough (*Help and Feedback* view, *Azure Templates: Get Started*, *Help → Welcome → Walkthroughs*). After an update, a notification offers *What's New* |
| Report a bug or an idea | Bug button in the Azure Templates tool window header; *Help → Report Azure Templates Navigator Issue…*; *Report to Azure Templates Navigator* in the IDE error dialog | *Help and Feedback* view in the Azure Templates sidebar; bug button on the Dependencies and Diagnostics views; *Azure Templates: Report Issue…* |

## Template graph and stats

**Node size** follows callers + callees, counted over the whole workspace, on a square-root scale. A template used everywhere is
big even when the graph shows only the open file. Hover a node for the numbers: files and call sites that use it, pipelines that
reach it, what it calls, its parameters and its problems. A red or amber badge marks errors or warnings. The corner shows how many
nodes and edges are drawn out of the total, and says *Workspace* when the graph isn't following the editor.

**Labels** are file names, with as many parent folders as it takes to tell same-named files apart (`build/template.yml`,
`deploy/template.yml`). *Full Path* shows the whole path.

**Click a node** to open it and select it: it stays lit with every pipeline and template that leads to it and everything it
calls, and the rest dims. Click the background to clear it. In the workspace graph, the file open in the editor is selected.

**Filters** (both IDEs, computed by the server):

- **Problems**: files with errors or warnings, plus missing templates, unknown aliases and unavailable repositories.
- **Kinds**: pipelines, templates, external templates, broken references.
- **Repositories**: this workspace and each repository templates come from.
- **Around the current file**: depth 1–10, callers only, callees only, or both. *Focus graph here* (VS Code) or *Center Graph on This File* (IntelliJ) on a node moves the focus.

With *Problems* or *Repositories*, the direct callers of what matches stay in the graph, faded, so you can see who uses it.

**Stats** is aimed at teams that publish templates:

| Section | What it tells you |
|---|---|
| Overview | Pipelines, templates, external templates, repositories, errors, warnings, broken references. Expand *Errors* or *Warnings* for the files that have them, *Broken references* for each broken reference |
| Problems by type | Errors and warnings per kind (missing required parameter, unknown parameter, template not found…), most frequent first. Expand one for the files that have it; click a file to open its first one |
| Most used | Call sites, files and pipelines reached: what a parameter change affects most |
| Most mis-called | Call sites with parameter problems: often a confusing interface |
| Calls the most templates | Wide templates that are hard to change and debug |
| Not referenced in this workspace | Clean-up candidates. Templates published for other repositories appear here too, so check before deleting |
| By repository | Templates, call sites and files using each repository |

Click a row to open the file (at the problem or reference, where there is one); *Pipelines* and *Repositories* open those views, *Templates* and *External templates* show that kind in the graph. *Show in Graph* centres the graph on that file, filters it to the repository or kind, or shows only problems.

## Settings (identical in both IDEs)

| VS Code key (`azure-templates-navigator.*`) | IntelliJ: Settings → Tools → Azure Templates Navigator | Default |
|---|---|---|
| `requiredParameterColor` | — (VS Code hovers only) | `#c92d35` |
| `diagnostics.enabled` | Validate template call sites | `true` |
| `diagnostics.unusedParameters` | Warn about unused template parameters | `true` |
| `completion.requiredParameters` | Insert required parameters when completing a template path | `true` |
| `graph.rootPath` | Workspace graph root | `""` |
| `repositories.autoFetch` | Download repositories automatically | `true` |
| `repositories.cacheDir` | Cache folder | `~/.azure-templates-navigator/repos` |
| `repositories.refreshIntervalHours` | Refresh after (hours) | `12` |
| `repositories.preferLocalClones` | Prefer clones next to the project | `true` |
| `repositories.urls` | Clone URL overrides | `{}` |
| `repositories.localPaths` | Local paths | `{}` |
| `repositories.azureDevOpsOrganization` / `azureDevOpsProject` | same | `""` |
| `repositories.githubEnterpriseHost` | GitHub Enterprise host | `""` |
| `repositories.gitPath` | git executable | `git` |
| `azure.enabled` | Offer to check my changes in Azure DevOps | `false` |
| `azure.keepSnapshots` | Keep the refs/atn/preview/… refs after a check | `false` |
| `azure.azPath` | Azure CLI (az) | `""` (`az` on the PATH) |
| `server.path` | Language server binary | bundled |

`server/internal/engine/parity_test.go` (Go) and `SettingsParityTest` (Kotlin) fail the build if the two clients' settings drift from the server's schema.

## CLI

The same binary validates pipelines in a terminal or in CI:

```bash
atn check [--root DIR] [--format text|json|azure] [--strict] [--no-fetch] [--since REF] [--config FILE] [files…]
atn validate [--root DIR]… [--since REF] [--yes] [--format text|json] [--diff] [--keep-snapshots] [files…]
atn fetch [--root DIR]          # download every referenced repository
atn repos [--root DIR]          # show how each alias resolves
atn version
```

`check` exits with code 1 on errors, or on warnings too with `--strict`. In text format it ends with one verdict per pipeline:

```
Pipelines:
  azure-pipelines.yml          ✗ Will fail when queued: 1 error
  pipelines/nightly.yml        ✓ No problems found before running
```

A pipeline passed as a file is checked together with every template it uses, wherever they live (`atn check azure-pipelines.yml`).

`--since REF` reports only the problems that are new since `REF`, and exits with 1 only on those. A repository that already has
problems can then gate its pull requests on not adding more. `REF` is one of:

- a branch: compared with where `HEAD` splits from it;
- a commit;
- `HEAD`: uncommitted changes only;
- `auto`: where this branch started, as in the IDEs.

```
$ atn check --since origin/main
azure-pipelines.yml:42:13: error: Missing required parameter 'testProjectPath' (type: string) for template 'templates/run-tests.yml' [missing-required-param]

Since origin/main @ 3d0053a: 1 file changed · 2 pipelines affected · 4 new errors, 1 new warning
All pipelines: 8 of 11 fail when queued

Pipelines your changes reach:
  ✗ azure-pipelines.yml                  +4 new · 5 already there
  ✗ pipelines/microservice-pipeline.yml  +4 new · 2 already there
```

It needs the history back to `REF`. Azure Pipelines checks out shallow by default, so set `fetchDepth: 0` on the checkout.
When `REF` can't be found, or shares no commit with `HEAD`, `check` exits with 2 instead of comparing with the wrong commit.

`validate` asks Azure DevOps to compile every pipeline your changes reach, at the base and with your changes, without running
it ([Check in Azure DevOps](docs/README.md#check-in-azure-devops)). It pushes your changed files to a
`refs/atn/preview/…` ref first, so it lists what it will send and asks; `--yes` skips the question (required without a terminal).
It exits with 1 when your changes make Azure reject a pipeline, and 2 when it couldn't check one. `--diff` prints how each
pipeline's expanded YAML changed. Pass `--root` once per repository when your changes span several (the templates repository
and the one that uses them). It signs in with `az` (`az login` once), or a token in `ATN_AZURE_TOKEN` (or `SYSTEM_ACCESSTOKEN`, not tried in a pipeline yet).

```
$ atn validate --since origin/main
  azure-pipelines.yml  ✗ rejected (1 problem, 1 new) · atn found no error
azure-pipelines.yml:15:7: error: Unexpected value 'scriptt' [azure]

Azure DevOps: 0 of 1 compile · 1 rejected because of your changes · 1 disagrees with atn
```

To get annotations in an Azure Pipelines run, add these steps:

```yaml
- script: |
    curl -sSL -o atn https://github.com/qSharpy/azure-templates-navigator/releases/latest/download/atn-linux-x64
    chmod +x atn && ./atn check --format azure
  displayName: Validate pipeline templates
```

On pull requests, to fail only on what the pull request adds:

```yaml
- checkout: self
  fetchDepth: 0
- script: |
    target='$(System.PullRequest.TargetBranch)'   # refs/heads/main
    ./atn check --format azure --since "origin/${target#refs/heads/}"
  displayName: No new pipeline problems
```

The download URL assumes the binaries from `dist/release/` are attached to a GitHub release of the repo you publish this from.
In CI the checkout already has credentials for Azure Repos via `System.AccessToken`. For other private repos, set `repositories.urls` with a token-bearing URL, or run `git config --global url."https://x:$(TOKEN)@github.com/".insteadOf https://github.com/` first.

## Reporting problems and feedback

Bugs, ideas and feedback go to [GitHub Issues](https://github.com/qSharpy/azure-templates-navigator/issues/new/choose).
The quickest way is from the IDE, which opens a new issue with the extension, `atn`, IDE and OS versions filled in:

- **VS Code:** the *Help and Feedback* view in the Azure Templates sidebar, the bug button on the Dependencies and
  Diagnostics views, or *Azure Templates: Report Issue…* in the command palette.
- **IntelliJ:** the bug button in the Azure Templates tool window header, or *Help → Report Azure Templates Navigator Issue…*. When the plugin throws an exception, IntelliJ's
  error dialog offers *Report to Azure Templates Navigator*, which adds the stack trace to the issue.

Nothing is sent until you submit the issue on GitHub, so check it for anything private first. A bug report is
most useful with the smallest YAML that shows the problem and the log: *Azure Templates: Show Log* in VS Code,
lines starting with `[atn]` in IntelliJ's *Help → Show Log*, or the output of `atn check`.

Both places also offer the documentation, the log, and **Give Feedback**, which opens the extension's reviews on the
[Visual Studio Marketplace](https://marketplace.visualstudio.com/items?itemName=bogdanbujor.azure-templates-navigator&ssr=false#review-details)
or [JetBrains Marketplace](https://plugins.jetbrains.com/plugin/30272-azure-templates-navigator/reviews). Ratings and reviews there help other people find it.

## Building

Requirements: Go ≥ 1.20, Node ≥ 18 (VS Code), JDK 17+ (IntelliJ; Gradle wrapper included).

```bash
scripts/build.sh dev             # atn for this machine only + copy assets into the VS Code client
scripts/build.sh test            # Go tests, VS Code E2E (downloads VS Code once), IntelliJ tests
scripts/build.sh server          # cross-compile atn → dist/bin/<platform>-<arch>/ and dist/release/atn-<platform>-<arch>
scripts/build.sh vscode          # dist/azure-templates-navigator-<v>.vsix, all binaries in one package
scripts/build.sh vscode-targets  # dist/azure-templates-navigator-<platform>-<v>.vsix, one per platform (smaller; recommended for the Marketplace)
scripts/build.sh intellij        # dist/azure-templates-intellij-<v>.zip (all binaries)
scripts/build.sh all
```

**Debugging.**

- **VS Code:** open `clients/vscode` and press F5. It uses the binary in `clients/vscode/bin/`, or set `azure-templates-navigator.server.path` to a `go build` output.
- **IntelliJ:** `cd clients/intellij && ./gradlew runIde`. Server logs appear in `idea.log` with the `[atn]` prefix.
- **Server only:** `go test ./...` in `server/`. `atn check --root samples` exercises nearly everything without an IDE.

## Releasing

Releases are published only by `.github/workflows/release.yml`, which runs when a `vX.Y.Z` tag is pushed. Nothing is published by hand.

1. Set `VERSION` to the new version. `build.sh` copies it into `package.json` and `gradle.properties`.
2. In `CHANGELOG.md`, give the release its heading, `## [X.Y.Z] — YYYY-MM-DD`. The workflow refuses a tag whose section is missing or still says `unreleased`, and uses the section as the GitHub release notes.
3. Commit, then `git tag vX.Y.Z && git push origin main vX.Y.Z`.

The workflow runs `scripts/build.sh test`, builds the six platform VSIXs and the IntelliJ zip, and publishes them to:

- the VS Code Marketplace
- Open VSX, only if the `OVSX_PAT` secret is set
- a GitHub release, with the VSIXs, the plugin zip and the `atn-*` CLI binaries
- the JetBrains Marketplace, which reviews each upload by hand, so it appears a day or two later

If a step fails, fix the problem and re-run the job. Every step before JetBrains skips work that is already done. The secrets it needs are listed at the top of the workflow file.

`scripts/build.sh vscode-targets` and `scripts/build.sh intellij` build the same packages locally, into `dist/`, if you want to try them before tagging.

Platforms: darwin-arm64, darwin-x64, linux-x64, linux-arm64, win32-x64, win32-arm64.
