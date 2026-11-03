# Azure Templates Navigator

IDE support for Azure Pipelines YAML templates, built as **one engine with thin IDE shells**:

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
   - With `autoFetch` off, or after a failure, the hover and the quick fix show **Fetch repository**. The Repositories view lists every repo with its state, commit and age.

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
| Pipeline ⇄ template chain | Call Hierarchy (Ctrl+Alt+H); the Azure Templates tool window | Show Call Hierarchy; the Azure Templates views |
| Stages / jobs / steps / template calls | Structure view (Cmd/Ctrl+F12) | Outline view, breadcrumbs |
| Jump to the referenced template | Ctrl/Cmd+click, or the gutter icon on `template:` | Ctrl/Cmd+click |
| Problems and fixes | Inspections + Alt+Enter; Diagnostics tab | Problems view + quick fixes |
| Template graph | Graph tab (IntelliJ toolbar and menus) | Graph panel |

## Settings (identical in both IDEs)

| VS Code key (`azure-templates-navigator.*`) | IntelliJ: Settings → Tools → Azure Templates Navigator | Default |
|---|---|---|
| `requiredParameterColor` | — (VS Code hovers only) | `#c92d35` |
| `diagnostics.enabled` | Validate template call sites | `true` |
| `diagnostics.unusedParameters` | Warn about unused template parameters | `true` |
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
| `server.path` | Language server binary | bundled |

`server/internal/engine/parity_test.go` (Go) and `SettingsParityTest` (Kotlin) fail the build if the two clients' settings drift from the server's schema.

## CLI

The same binary validates pipelines in a terminal or in CI:

```bash
atn check [--root DIR] [--format text|json|azure] [--strict] [--no-fetch] [--config FILE]
atn fetch [--root DIR]          # download every referenced repository
atn repos [--root DIR]          # show how each alias resolves
atn version
```

`check` exits with code 1 on errors, or on warnings too with `--strict`. To get annotations in an Azure Pipelines run, add these steps:

```yaml
- script: |
    curl -sSL -o atn https://github.com/qSharpy/azure-templates-navigator/releases/latest/download/atn-linux-x64
    chmod +x atn && ./atn check --format azure
  displayName: Validate pipeline templates
```

The download URL assumes the binaries from `dist/release/` are attached to a GitHub release of the repo you publish this from.
In CI the checkout already has credentials for Azure Repos via `System.AccessToken`. For other private repos, set `repositories.urls` with a token-bearing URL, or run `git config --global url."https://x:$(TOKEN)@github.com/".insteadOf https://github.com/` first.

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

1. Bump `VERSION`. `build.sh` copies it into `package.json` and `gradle.properties`.
2. Run `scripts/build.sh vscode-targets`, which cross-compiles the server first. Then run `npx vsce publish --packagePath dist/*-$(cat VERSION).vsix`.
3. Run `scripts/build.sh intellij`, then upload `dist/azure-templates-intellij-<v>.zip` to JetBrains Marketplace.
4. Attach `dist/release/atn-*` to a GitHub release for CLI and CI users.

Platforms: darwin-arm64, darwin-x64, linux-x64, linux-arm64, win32-x64, win32-arm64.
