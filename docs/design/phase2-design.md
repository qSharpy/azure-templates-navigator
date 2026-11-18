# Phase 2 design: Azure DevOps checks your pipelines

Status: **agreed (2026-11-17); 2a implemented (2026-11-17); 2b designed only.** §14 lists what's left. Built on the research hand-off [`phase2-azure-preview.md`](phase2-azure-preview.md) (cited as *H§n*). Section 11 records the decisions.

## 1. What we build

Phase 1 (2.8.0, *My changes*) answers "what did my changes break?" with our own engine, offline. Phase 2 asks **Azure DevOps** the same question through the pipelines preview API (`POST {org}/{project}/_apis/pipelines/{id}/preview`). The preview compiles a pipeline exactly like a queued run would, but **creates no run** (H§3).

- **2a: check the pipelines my changes reach.** For every root pipeline in *My changes*, Azure compiles it twice, at the base and with your changes. We show:
  - Azure's verdict;
  - whether atn agrees;
  - Azure's problems that your change added;
  - a diff of the expanded YAML.
- **2b: test every flow of a template.** For one template, Azure compiles it under every parameter combination worth trying, at the base and with your changes. This uses *synthetic roots* on a host pipeline (H§5.5). We show which flows changed and which broke, each with its diff.

Non-goals: creating pipeline definitions, running pipelines, personal access tokens (H§2).

## 2. Facts the design relies on

All verified with real calls (H§5, plus one more on 2026-11-17, below).

| Fact | Consequence |
|---|---|
| `yamlOverride` replaces only the root file. Templates are read from git at the resource's ref (H§5.1). | Local work must be pushed to Azure's git server first. |
| `self.refName` selects the root and same-repo templates, **and** sets `Build.SourceBranch`. `self.version` must be reachable from `refName` (H§5.2). | Base and change previews must use the same `refName`, or `${{ if }}` on the branch shows up as a false diff. |
| **New:** `self.refName=refs/atn/preview/snap2` + `self.version=<an ancestor of that ref>` → 200, the ancestor's content, `branch=refs/atn/preview/snap2`. | **One ref per repository serves both sides.** The snapshot commit's parent is the base, and both previews name that ref, pinning `version` to the base or the snapshot. `Build.SourceBranch` is identical on both sides. |
| Alias overrides (`resources.repositories.<alias>.refName/version`) win over `ref:` and leave `Build.SourceBranch` alone (H§5.3). | A template repo's change is previewed without moving the consumer off its branch. |
| Custom refs `refs/atn/preview/*` are accepted by push and by preview, and fire no CI trigger (H§5.4). | Snapshots go to custom refs. No branch is created, and nothing shows in the branch list. |
| Errors are HTTP 400 `PipelineValidationException`, one problem per message line. Lines look like `/<path>[@<alias>] (Line: n, Col: m): text`, or have no location (H§5.7). Git problems come back as 500 `InvalidGitVersionSpec`. | §6.3 parser. |
| `finalYaml` is deterministic and has no refs or SHAs in it (H§5.8). | A plain line diff between the base and change expansions is clean. |
| Not checked by Azure: pools, variable groups, service connections, task inputs (H§5.7). `Build.Reason` is always `Manual`, and variables don't reach `${{ }}` (H§5.6). | Stated in the UI and the docs (§10). |
| **Work org (Entra-backed), as a normal member, not an admin** (H§9.1–9.2): `/preview` returns 200 with an az token for the org's tenant, with `X-VSS-ForceMsaPassThrough` sent too; pushing and deleting `refs/atn/preview/test` works in one repository, with no policy or permission refusal. | **Entra is the main case**; MSA (the sandbox org) stays supported. The pass-through header is harmless on Entra, so it is always sent. A refused push is still handled (§6.2), since other repositories may have stricter policies. |
| ≈0.07–0.16 TSTU per preview, against a budget of 200 per 5 minutes, at 0.3–0.8 s each. | Rate limits matter only for 2b's matrices (§8). |

## 3. Architecture

Golden rule: everything lives in the Go server. The clients render results, show the consent dialog and open diffs.

```
server/internal/azure/          new, stdlib only, no engine import
  client.go     REST: base URL, headers, retries, rate-limit headers, JSON errors
  auth.go       TokenSource: az CLI (per tenant), env token; tenant discovery
  org.go        Org/project/repo from a git remote URL (reuses repos.ParseRemote)
  defs.go       definitions + repositories listing, yamlFilename → definition
  preview.go    Preview(defID, body) → (finalYaml | []Problem)
  problems.go   parse PipelineValidationException messages
  snapshot.go   build the snapshot commit with a temporary index; push and delete refs
  diff.go       line diff (Myers), unified output and +/- counts
server/internal/engine/azure.go       plan, run, comparison, result model, last results
server/internal/engine/azureflows.go  2b: matrix and synthetic roots
server/internal/lsp/azure.go          atn/azure* requests and notifications
server/cmd/atn/validate.go            `atn validate`
```

The `azure` package knows nothing about the engine. The engine turns its own model (changed files, root pipelines, aliases) into `azure` calls, and turns Azure's answers back into its model (problems on local files).

**Locks.** The engine gathers its inputs under `chMu` and then `mu` (the changes report, file texts, each root's resources and alias locations, our verdicts), then releases both. Every network call and git push runs without an engine lock. Results go into a separate `azMu` (a leaf lock, like `lastMu`).

## 4. Authentication (no PATs)

`azure.TokenSource` is an interface: `Token(ctx, org) (string, error)`. Sources, tried in order:

1. **Env token**: `ATN_AZURE_TOKEN`, else `SYSTEM_ACCESSTOKEN` (an Azure Pipelines job's bearer token). For `atn validate` in CI. Cheap, and no PAT.
2. **az CLI**: `az account get-access-token --resource 499b84ac-1321-427f-aa17-267ca6975798 [--tenant <tid>] -o json`. The token is cached per tenant until `expiresOn` minus 5 minutes. The executable is the `azure.azPath` setting (IDE-launched servers don't always have the shell's `PATH`), else `az`.
3. *(Later, not in 2a; D2)* **VS Code's Microsoft sign-in**: the server asks the client with a server→client request `atn/azureToken {organization, tenant}`. The client answers with `vscode.authentication.getSession('microsoft', ['499b84ac-…/.default', 'VSCODE_TENANT:<tid>'])`. Untested (H§9.4).

**Tenant discovery.** Anonymous `GET https://dev.azure.com/{org}/_apis/connectionData`, cached per org for the server's lifetime. The header `x-vss-resourcetenant` gives:
- a GUID: an Entra org (the usual case, and the user's work org), so az uses `--tenant <GUID>`;
- all zeros: an MSA-backed org (the sandbox org), so az uses its default tenant (no `--tenant`);
- missing: public org or unknown, so az uses its default tenant.

**Every request** sends `Authorization: Bearer …`, `X-VSS-ForceMsaPassThrough: true`, `X-TFS-FedAuthRedirect: Suppress` and `Accept: application/json`. A 302 or an HTML body is treated as 401.

**Errors the user sees:**
- az missing: "Install the Azure CLI, then run `az login --allow-no-subscriptions`".
- az not signed in, or `AADSTS…`: the same, with `--tenant <tid>` when known.
- 401 `TF400813`: "Azure DevOps rejected the token for {org}. Signed in as {upn from the JWT}. Run `az login --tenant <tid> --allow-no-subscriptions`."
- 403: "You can't preview pipelines in {project}. You need the 'View builds' and 'Queue builds' permissions." A normal member of the work org could preview (H§9.2), so this is for stricter projects.

**Never:** log tokens, put them in URLs, issue reports or settings, or pass them on a command line. Git gets the token through `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_n` / `GIT_CONFIG_VALUE_n` env vars (git ≥ 2.31) with `credential.helper=` emptied, so it doesn't show in `ps`. With an older git, we push with the user's own git credentials (no header).

## 5. From the workspace to Azure

### 5.1 Organization, project and repository

- For each git repository in the workspace (`ChangeBase.Top`), `repos.ParseRemote(origin)` gives org, project and repo for `dev.azure.com/…/_git/…`, `*.visualstudio.com` and `ssh.dev.azure.com:v3/…`. `repositories.azureDevOpsOrganization` / `azureDevOpsProject` fill in what the remote can't tell.
- **GitHub** (`github.com` origin; issue #10): the remote names no organization or project. The organization is `repositories.azureDevOpsOrganization`, else the one of the Azure Repos pipelines in the same check, else the plan fails naming the setting. The project is `repositories.azureDevOpsProject`, else every project of the organization (`GET _apis/projects`, at most 100) is searched for definitions; each definition remembers the project it came from, and the preview goes there. Other hosts (GitHub Enterprise Server, Bitbucket) are skipped: *neither on Azure Repos nor on GitHub*.
- `GET {org}/{project}/_apis/git/repositories/{name}` gives the repository's `id` and `remoteUrl` (the push target).

### 5.2 Root pipeline → definition

- `GET {org}/{project}/_apis/build/definitions?repositoryId={id}&repositoryType=TfsGit&includeAllProperties=true&api-version=7.1`, paged with `continuationToken`. For GitHub, `repositoryType=GitHub&repositoryId=owner/repo` (Azure matches the id case-insensitively). The listing is filtered again on `repository.type`/`repository.id`.

**GitHub snapshots** (verified 2026-11-18 on the sandbox org, pipeline 4 on `qSharpy/atn-gh-spike`, service connection via OAuth):
- GitHub accepts pushes to `refs/atn/preview/…`. Preview takes that ref as `self.refName`, and `self.version` pins the base or the snapshot on it.
- A custom ref push started no run of a pipeline with an all-branches CI trigger. A control branch push did (run 44).
- `self.version` not reachable from `refName` → 400 `InvalidGitVersionSpec`. A missing ref → `PipelineValidationException` "Unable to resolve the reference", which is an error (no verdict), not a rejection.
- `self.refName=refs/heads/main` + `version=<base>` + `yamlOverride` works.

So a GitHub repository whose only changes are checked root pipelines pushes nothing: `self` is pinned to the base on the remote branch that holds it (`Git.RemoteBranch`), and the change side sends the root as `yamlOverride`. Otherwise its snapshot is pushed like an Azure Repos one, but to the `origin` remote with the user's own git credentials. `Git.Push` never puts the Azure DevOps token in the environment for a URL that isn't Azure DevOps. The organization comes from the settings, else the check's Azure Repos pipelines, else its Azure Repos repositories (a GitHub pipeline with its templates on Azure Repos). A `type: github` alias whose `name` is a workspace repository's `owner/repo` is snapshotted the same way and passed as an alias override (verified: pipeline 4 with alias `templates` → `qSharpy/atn-gh-templates`; a custom ref + `version` work, errors come as `/stages/deploy.yml@templates (Line …)`, a missing ref is "Could not get the latest source version", no run). The clients' remembered consent is per organization *and* GitHub, so a first GitHub push always asks.
- Keep definitions with `process.type == 2` (YAML). Normalise `process.yamlFilename`: trim a leading `/`, use `/` separators, compare case-insensitively (Azure Repos paths are case-sensitive, but users type them in the UI). Match it against the pipeline's path relative to the repository top.
- **Several matches** (CI and PR definitions of one file): use the lowest id and list the others in the result ("also: #12 PR build"). The expansion doesn't depend on the definition, except for its UI variables, and those don't reach `${{ }}` (H§5.6).
- **No match**: *not registered in Azure*. In 2b, the same pipeline can be checked through a host pipeline (§8.3). In 2a it's reported and skipped.
- Cached per server for 10 minutes, and dropped on `atn.refresh`.

### 5.3 The root's repositories (aliases)

Azure resolves `@alias` only against the **root file's own** `resources.repositories`. So the engine reads that file's declarations (`f.repos` of the root, not the workspace-wide alias registry, where the first declaration wins). For each alias it locates the repository the same way the engine resolves templates (`Repos.Locate`):

| Where the alias resolves | Snapshot? |
|---|---|
| A git repository that is a workspace root or contains one (it's in `ChangesReport.Bases`) | yes, when it has changes |
| A sibling or `localPaths` clone that isn't in the workspace | no. Phase 1 doesn't compare it either, so nothing of it counts as "your change". |
| The fetch cache (at the declared ref) | no. Azure reads the same ref. |

An alias override is only sent when the local repository's origin **is** the declared repository (`name: project/repo`, case-insensitive). Otherwise the user mapped the alias to some other clone with `localPaths`, and we report *alias `x` resolves to a different repository locally* and preview it at its declared ref.

## 6. Phase 2a: check the pipelines my changes reach

### 6.1 Plan (no side effects except reads)

Input: the current `ChangesReport` (`Changes()`, or `ChangesSince(ref)` for the CLI), optionally narrowed to some root pipelines. Output: an `AzurePlan`:

```jsonc
{
  "id": "p-8f3a…",                    // hash of everything below
  "organization": "fabrikam", "project": "platform",
  "user": "Test User",                // connectionData.authenticatedUser.providerDisplayName
  "pipelines": [
    { "path": "…/azure-pipelines.yml", "relativePath": "azure-pipelines.yml",
      "definition": { "id": 2, "name": "consumer-ci", "url": "https://dev.azure.com/…/_build?definitionId=2", "others": [] },
      "skip": "" }                    // or "not registered in Azure", "not on Azure Repos", …
  ],
  "snapshots": [
    { "repo": "consumer", "remoteUrl": "https://dev.azure.com/fabrikam/platform/_git/consumer",
      "ref": "refs/atn/preview/3f2a9c1e/feature-x", "base": "83bf411…",
      "files": [ { "relativePath": "local/steps.yml", "status": "modified" } ],
      "unpushedBase": false }         // the base commit isn't on the server: its history will be uploaded
  ],
  "summary": "Push 1 file to refs/atn/preview/3f2a9c1e/feature-x in consumer, then ask fabrikam/platform to compile 3 pipelines (6 previews). No run is created."
}
```

`unpushedBase` is checked with `git branch -r --contains <base>` against the remote-tracking refs. It's a hint for the consent text, not a guarantee.

### 6.2 The snapshot commit

Every repository with changes gets **one commit** that only differs from the base in your YAML:

```
tmp index:  GIT_INDEX_FILE=<tmp> git read-tree <base>
each added/modified file in ChangesReport.Files of this repo:
            git hash-object -w --stdin --path=<rel>     ← text from the engine (open, unsaved documents included)
            git update-index --add --cacheinfo 100644,<blob>,<rel>
each deleted file:
            git update-index --force-remove <rel>
            git write-tree                                → tree
            git commit-tree <tree> -p <base> -m "atn preview snapshot [skip ci]"   (fixed author: Azure Templates Navigator)
push:       git push --force --no-verify <remoteUrl> <commit>:<ref>
```

Why this and not `git stash create`:
- `stash create` skips untracked files, so a new template would fail with "File not found" (H§5.1, case D2).
- It also misses unsaved editor buffers, which our own verdict includes.
- Here, **exactly the files *My changes* lists** reach the server, and nothing else of your uncommitted work: source code, `.env` files and other non-YAML changes stay local.
- The user's index, working tree, branches and stash are never touched.
- `--path` applies the repository's clean filters (`core.autocrlf`), like a commit would.

**Unpushed commits.** The parent is the base, not HEAD, so commits between the base and HEAD are not uploaded unless the base itself isn't on the server. The base is normally where your branch splits from `origin/main`, which is already there.

**One ref per repository** serves both previews (§2, verified): base = `{refName: ref, version: base}`, change = `{refName: ref, version: snapshot}`.

**Ref name** (*Decision D4*): `refs/atn/preview/<user8>/<branch-slug>`.
- `user8` is the first 8 hex digits of `connectionData.authenticatedUser.id`: stable, and not an email.
- `branch-slug` is the checked-out branch, sanitised to `[A-Za-z0-9._-]` (`detached` when HEAD is detached).
- Stable names mean a crashed run's ref is overwritten and cleaned by the next run.

**A refused push** (branch policy, missing *Create branch* / *Contribute* permission, or a ref-name restriction). The push first goes with the token, then with the user's own git credentials on `origin`. When both fail (*Decision D5*), only that repository is lost: every pipeline that needs its snapshot (its own repository, or an alias to it) is *couldn't check* on both sides, with "couldn't push your changes to {ref} in {repo}: the server refused it: {the server's reason, e.g. TF401027 …}. You need permission to create refs in that repository", and gets no preview. The other pipelines are checked as usual, and the refs that were pushed are deleted. The reason also goes in the report's warnings. When no repository could be pushed, the whole check fails with that reason. No branch is ever created as a fallback. Pushing works in the work org (H§9.2), so this is the rare case.

**Cleanup** (*Decision D4*): `git push --no-verify <remoteUrl> :<ref>` after the last preview, unless `azure.keepSnapshots`.

A repository without changes gets no snapshot. Its `self` / alias is left alone, so Azure uses the definition's default branch or the declared `ref:`.

### 6.3 Previews

Per pipeline, two `POST …/pipelines/{def}/preview?api-version=7.1` calls, both with `{"previewRun": true, "resources": {"repositories": {…}}, "templateParameters": {…}}`:

- `self` → `{refName, version}` when the pipeline's own repository has a snapshot.
- each alias of §5.3 with a snapshot → `{refName, version}`.
- `templateParameters`: only for root runtime parameters **without a default**. Each gets the first of its `values:`, else a placeholder by type (`boolean` false, `number` 0, `string` `"atn"`, `object` `{}`). Without one, both sides would fail with "A value for the 'x' parameter must be provided" and there would be nothing to diff. The values used are listed in the result.

**Concurrency**: 4 previews in flight per run. **Rate limits**:
- On 429/503, honour `Retry-After` (seconds), up to 3 retries.
- Log `X-RateLimit-Cost` and `X-RateLimit-Remaining` at debug level.
- If `X-RateLimit-Delay` shows up, slow down to 1 in flight.

**Timeouts**: 60 s per call, 10 minutes per run. **Cancellation**: `atn/azureCancel`, or `Ctrl-C` in the CLI, cancels the context. The snapshot ref is still deleted (with a fresh 30 s context).

**Response handling:**

| Status | Meaning |
|---|---|
| 200 `{finalYaml}` | compiles |
| 400 `PipelineValidationException` | rejected: parse `message` (§6.4) |
| 500 `InvalidGitVersionSpec`, 400 "Could not get the latest source version" | a snapshot problem (a ref or commit the server can't see): reported as *couldn't check*, not as a verdict |
| 401/403/404/other | *couldn't check*, with the message |

### 6.4 Parsing Azure's problems

Split `message` on newlines. For each non-empty line:

```
^(/[^ ]+?)(?:@([A-Za-z0-9_.-]+))? \(Line: (\d+), Col: (\d+)\): ?(.*)$   → file, alias, line, col, text
^(/[^ :]+):\s(.*)$                                                       → file, no position (e.g. "/azure-pipelines.yml: Could not get…")
otherwise                                                                → no location
```

- Leading spaces in the text are trimmed (`L2` has `):  While parsing…`).
- Lines that are pure noise (`Object reference not set to an instance of an object.`) are kept, but folded into the previous problem's text.
- **Mapping to a local file:**
  - `/<path>`: the pipeline's repository top + path (Azure paths are repo-absolute).
  - `/<path>@<alias>`: that alias's location (§5.3) + path. When the alias has no local copy, the problem stays on the root pipeline, at line 1, with the Azure path in the text.
  - No location: on the root pipeline, line 1.
- Azure's lines and columns are 1-based. They point into the snapshot, which is the engine's text, so they map without adjustment.

`AzureProblem {path, relativePath, line, col, message, azurePath}`. The key for comparisons is `azurePath + message with the (Line…, Col…) part removed`, as a multiset (like `problemKey`).

### 6.5 Comparing

Per pipeline, from the base preview (`B`), the change preview (`C`) and our verdict now (`ours`, `PipelineReport`):

- `azure.verdict`: `compiles` | `rejected` | `unknown` (couldn't check) | `skipped`.
- `azure.new` / `azure.fixed` / `azure.existing`: C's problems against B's, so a pipeline that was already broken in Azure isn't blamed on your change.
- **Agreement**: only **our errors** are comparable to "compiles". Warnings mean "the run starts but may misbehave", which the preview can't tell.

  | Azure (change) | atn errors now | `agreement` |
  |---|---|---|
  | compiles | 0 | `agree` |
  | rejected | > 0 | `agree` |
  | rejected | 0 | `azureOnly`: Azure found something we don't check. Likely an engine gap, so offer *report*. |
  | compiles | > 0 | `atnOnly`: we report an error Azure accepts. Either a false positive, or a check Azure only does when a run is queued (e.g. `repository-missing-endpoint`). The UI says "Azure's compile check passed. atn's errors may still fail the run when it is queued." |

- Per problem: `alsoFoundByAtn` when we have an error in the same file within ±1 line.
- **Expansion diff**: when both previews compile, `diff = Myers(B.finalYaml, C.finalYaml)`, reported as `+added −removed`. The texts themselves are fetched on demand (§7), because they can be large.

Result model (`engine.AzureReport`):

```jsonc
{
  "state": "done",                     // running | done | cancelled | failed
  "error": "",                         // when failed (auth, network, git)
  "planId": "p-8f3a…", "organization": "fabrikam", "project": "platform",
  "base": "origin/main @ 83bf411", "finishedAt": "…",
  "stale": false,                      // the workspace changed since (diagGen differs)
  "totals": { "pipelines": 3, "compiles": 2, "rejected": 1, "newlyRejected": 1, "disagree": 1, "changedExpansions": 2, "skipped": 0 },
  "pipelines": [
    { "path": "…", "relativePath": "…", "definition": {…},
      "azure": { "verdict": "rejected", "base": "compiles", "new": [AzureProblem], "fixed": [], "existing": 0, "error": "" },
      "atn": { "verdict": "ok", "errors": 0 },
      "agreement": "azureOnly",
      "expansion": { "changed": true, "added": 12, "removed": 3 },
      "parameters": { "env": "dev" } }
  ]
}
```

The engine keeps the last report (`azMu`). `stale` is computed on read: `diagGen` or the base key differs from what the run saw.

## 7. LSP

| Request / notification | Shape |
|---|---|
| `atn/azurePlan {uris?: string[]}` → `AzurePlan` | Reads only: token, connectionData, repositories, definitions. Errors (auth, not on Azure Repos, nothing changed) come back as `{error}`, not an RPC error, so the client can show them. |
| `atn/azureValidate {planId}` → `{started: true}` or `{error}` | **Consent is enforced here.** The server re-plans, and refuses unless the new plan's id equals `planId` ("Your changes changed since you confirmed: check again"). Then it starts a background job and returns at once. A worker is never held. |
| `atn/azureProgress` (notification) | `{done, total, message}` while the job runs. VS Code shows it in `withProgress`, IntelliJ in a `Task.Backgroundable`. |
| `atn/azureCancel {}` | Cancels the job. |
| `atn/azureResults {}` → `AzureReport \| null` | The last report, with `stale` computed. Problems use the `lspProblem` shape (UTF-16 ranges). |
| `atn/azureExpansion {uri}` → `{base, change, title}` | The two `finalYaml` texts of a pipeline from the last report, for the client's diff editor. |
| `atn/didChange {reason: "azure"}` | Sent when a job finishes, so views reload. |
| `atn/changes`, `atn/pipelineCheck` | Each changed pipeline gains `azure: {verdict, agreement, new, expansion, stale}` when the last report has it. The views don't merge anything themselves. |

`initialize` gets nothing new. `azure.enabled` is a setting (§9).

Only one job runs at a time. `atn/azureValidate` during a run answers `{error: "A check is already running"}`.

## 8. Phase 2b: test every flow of a template

### 8.1 Entry points

- *Test flows in Azure DevOps…* on a template: code lens next to *N usages*, the editor context menu, and the Templates view.
- `atn validate --flows path/to/template.yml`.

### 8.2 The flow matrix (engine, offline, testable)

From the template's `pipeline.Param`s (`Type`, `Default`, `Values`) and its own text:

| Parameter | Candidate values |
|---|---|
| `boolean` | `true`, `false` |
| `values:` | each value |
| `string` / `number` without values | the default, plus every literal it is compared with in `${{ if/elseif }}` (`eq(parameters.env, 'prod')`, `in(…)`, `ne`) found by a scanner in `pipeline/` |
| `object`, `step(s)`, `job(s)`, `stage(s)`, … | the default only. A required one without a default gets an empty list or map. |

- **Combinations**: the full product when it is ≤ `azure.maxFlows` (default 32). Otherwise a greedy pairwise cover (every pair of values appears once), capped at `maxFlows`.
- **Every consumer's actual call** is added: the literal parameter values passed at each call site the engine knows (reverse edges, `PassedParam`). A call with expressions contributes only its literal parameters.
- Identical combinations are merged, and each flow is labelled (`env=prod, runTests=false`, `consumer: app/azure-pipelines.yml:12`).

### 8.3 Host pipeline and synthetic root

- **Host**: `azure.hostPipeline` (a definition id), else a YAML definition in the template's own repository (same-repo template paths then resolve), else none. In that case the user gets *Set a host pipeline: any pipeline in {project}*, with a picker of definitions. We never create one (H§8.2.6).
- **Synthetic root** (`yamlOverride`), per flow. The engine knows the template's kind:
  ```yaml
  trigger: none
  pr: none
  resources:
    repositories:
    - repository: atn_under_test        # omitted when the host's self is the template's repo
      type: git
      name: <project>/<repo>
  stages:                               # stages template: called directly
  - template: <path>@atn_under_test
    parameters: { …flow… }
  # jobs template: stages: [{stage: atn, jobs: [{template: …}]}]
  # steps template: …jobs: [{job: atn, steps: [{template: …}]}]
  # variables template: variables: [{template: …}] plus a one-step job
  ```
- Base and change for every flow: the alias (or `self`) gets `{refName: snapshotRef, version: base | snapshot}`, as in 2a.
- **Results**: flows grouped as *broke* (compiled at the base, rejected now), *changed* (the expansion differs, with a diff), *fixed* and *unchanged*. Identical `finalYaml`s are collapsed ("4 flows expand identically").
- **Cost**: 2 × flows previews. With the default of 32 that's 64 calls, about 10 TSTU, done in about 10 s at 4 in flight.

### 8.4 Later (not in 2b)

- Golden expansions committed next to the template.
- Cross-project templates and repositories the host isn't authorised for (H§9.3): reported as Azure says until tested.

## 9. Settings

Three places plus the parity tests (`engine/parity_test.go`, `SettingsParityTest.kt`), per CLAUDE.md:

| Key | Default | Meaning |
|---|---|---|
| `azure.enabled` | `false` | Opt-in. Shows the *Check in Azure DevOps* actions. Running the command while it's off offers to turn it on. |
| `azure.keepSnapshots` | `false` | Keep the `refs/atn/preview/…` refs after a check (*Decision D4*). |
| `azure.azPath` | `""` | The Azure CLI executable (`az` on `PATH` when empty). |
| `azure.hostPipeline` | `0` | *2b:* definition id used for synthetic roots (0 = choose automatically). |
| `azure.maxFlows` | `32` | *2b:* cap on parameter combinations per template. |

Org and project come from the git remote, and fall back to the existing `repositories.azureDevOpsOrganization` / `azureDevOpsProject`. No new settings for them.

## 10. User interface

### 10.1 Consent

Every run goes through the plan. The client shows `plan.summary`, the pushed files and the skipped pipelines in a modal, with the buttons *Check in Azure DevOps* and *Cancel*.

- **VS Code**: `showInformationMessage(…, {modal: true, detail})`, plus *Always for this workspace* (stored in `workspaceState`).
- **IntelliJ**: `MessageDialogBuilder.yesNo` with a "Don't ask again for this project" checkbox (`PropertiesComponent`).
- *Always* is dropped when the plan's org changes.
- The server still requires the `planId` (§7), so a client can't skip the plan.

### 10.2 Where results show (*Decision D3*)

**The Pipelines view, *My changes* mode** (both IDEs), because that's where Phase 1's answer already lives:

- **Title button** *Check in Azure DevOps* (`$(cloud-upload)` / `AllIcons.Actions.Execute`). It's visible when `azure.enabled` is on and the mode is *My changes*.
- **Summary row** under the base row, e.g. "Azure DevOps: 2 of 3 compile · 1 rejected by your change · 1 disagreement · 2 min ago". It says "(out of date)" when `stale`. A click runs the check again.
- **Per pipeline**, a first child row:
  - "Azure DevOps: ✓ compiles", "✗ rejected (+2 new)", or "? couldn't check: …";
  - an agreement badge in the description ("atn agrees" / "atn missed this" / "atn reports errors Azure accepts");
  - Azure's new problems as children, which open file and line;
  - "Expanded YAML changed (+12 −3)", which opens the diff.
- **Diff**:
  - VS Code: `vscode.diff` of two `atn-azure:` virtual documents, served by a `TextDocumentContentProvider` from `atn/azureExpansion`.
  - IntelliJ: `DiffManager.showDiff` with `DiffContentFactory.create(text, YAMLFileType)`.
- **Disagreements**: a context action *Report this disagreement* opens the existing issue builder with the title "Azure and atn disagree: <code or message head>". The body still holds **no workspace data**: versions and OS only, and the user pastes a minimal example (CLAUDE.md issue rules).
- **Code lens**: the verdict lens of a pipeline in the last report adds " · Azure ✓" / " · Azure ✗" (from the stored report, no network).

2b adds a Flows panel per template: a VS Code webview-free tree under the Pipelines view ("Flows of stages.yml"), or an IntelliJ tool-window tab, listing flows grouped by outcome, each opening its diff.

### 10.3 CLI

```
atn validate [--since REF] [--yes] [--format text|json] [--diff] [--keep-snapshots] [pipelines…]
```

- `--since` defaults to `auto`. As in `check --since`, the command fails with exit code 2 when it can't tell the base.
- Without `--yes`:
  - on a terminal, it prints the plan and asks `[y/N]`;
  - otherwise it exits 2 with "pass --yes to send your changes to Azure DevOps".
- **Text output**: one line per pipeline (`✓ compiles · atn agrees · expansion +12 −3`), Azure's new problems as `file:line:col: error: …`, and with `--diff` a unified diff of each changed expansion. `--format json` prints the `AzureReport`.
- **Exit codes**:
  - 0: no pipeline is newly rejected;
  - 1: your change makes Azure reject a pipeline (new Azure problems);
  - 2: the check couldn't run (auth, network, git, not Azure Repos).
- In Azure Pipelines it authenticates with `SYSTEM_ACCESSTOKEN: $(System.AccessToken)` and needs `fetchDepth: 0`. Without push rights for the build identity it fails with exit code 2 and an explanation.

### 10.4 Docs

- `docs/README.md` gets a section "Check in Azure DevOps" covering setup (az login), consent, results and the limits (H§8.4):
  - PR-only flows can't be previewed;
  - variables don't reach `${{ }}`;
  - "compiles" ≠ "will run";
  - `Build.SourceBranch` is the snapshot ref when your pipeline's own repository changed.
- The CHANGELOG gets an entry, and a tip goes in `shared/tips/tips.json` once screenshots exist.

## 11. Decisions (made 2026-11-17)

The user took the recommendation on all four.

| # | Question | Options | Decision |
|---|---|---|---|
| D1 | Scope now | 2a only (2b designed here, built next) · 2a + 2b | **2a now**: it's the base 2b needs (auth, snapshots, preview, parsing, diff), and it can ship on its own. |
| D2 | Token sources | az CLI everywhere (+ env token for CI) · also VS Code's built-in Microsoft sign-in | **az everywhere + env token**: verified on both org types. The VS Code provider is untested on MSA orgs and needs a server→client token request. Add it later if az is a hurdle. |
| D3 | Where results show | Pipelines view *My changes* mode (+ diff editor) · a new "Azure DevOps" view · CLI and output log only at first | **Pipelines view, My changes mode**: one place for "what did my changes break", both by atn and by Azure. |
| D4 | Snapshot refs | delete after each check · keep one per user and branch, overwritten (setting `keepSnapshots`) | **Delete by default**, `keepSnapshots` to keep. Stable names mean a crashed run is cleaned by the next one. |
| D5 | A refused snapshot push | stop the whole check with an explanation · check the other repositories and mark the pipelines that need the refused one *couldn't check* · push a `[skip ci]` branch instead (visible in the branch list, may start PR or branch-filtered builds) · check only work already on the server (no local changes) | **Per repository, *couldn't check*** (decided later the same day, after the work-org push test): it keeps D4's "no branches" and still checks what it can. The whole check fails only when no repository could be pushed. |

## 12. Tests (no network)

- **`azure` package**, with an `httptest.Server` fake of Azure DevOps:
  - The fake replays responses recorded from the sandbox org on 2026-11-17 (`testdata/preview/*.json`: A, C, G, H, H2, K, L, L2, T2, R1, R4, R8, F; `definitions.json`, `repositories.json`, `connectionData.json`, `unauthorized.json`).
  - The org name and user are replaced with `fabrikam` / `Test User`. The user's own pipeline 1 is left out.
  - Tests check the headers sent (pass-through, FedAuthRedirect, bearer), the retry on 429 with `Retry-After`, 302/HTML → 401, and definition paging.
- **Problems parser**: every recorded error message → the expected `(azurePath, alias, line, col, text)`. That includes the multi-line L2 and R1 and the location-less R4/R8.
- **Auth**: a fake `az` script written to `t.TempDir()` records its args (`--tenant` or not) and prints JSON. Cases: expiry caching, the error texts, and the env token taking precedence. Tenant discovery against the fake's `connectionData` with zeros, a GUID and no header.
- **Snapshot**: a scratch repository plus a local bare "server" repository (`file://`).
  - Build a snapshot with a modified, an added (untracked) and a deleted YAML file, plus an unsaved overlay text. Check that the commit's tree holds exactly base + those changes, and that its parent is the base.
  - Check that the working tree, the index (`git diff --cached` empty), HEAD and the stash are untouched.
  - Push it to `refs/atn/preview/…` in the bare repository, overwrite it, then delete it.
- **Diff**: Myers on small cases, the +/- counts, unified output.
- **Engine end-to-end** (`engine/azure_test.go`):
  - a scratch consumer repository and template repository, each with an `origin` that says `dev.azure.com/fabrikam/…`;
  - the fake server's `repositories` answer gives `remoteUrl = file://<bare>`, and `ATN_AZURE_URL` points the client at the fake;
  - the fake's preview handler reads the pushed ref from the bare repository (`git show <version>:<path>`), so the snapshot really is what's compiled. It returns 200 or a canned 400 depending on the content.
  - Covered: the plan, the consent `planId` check, the comparison table (all four agreement cases), new/fixed Azure problems, expansion diff counts, skip reasons (no definition, not Azure Repos), and the cleanup of refs.
- **LSP**: `atn/azurePlan` → `atn/azureValidate` → `atn/azureProgress` → `atn/didChange` → `atn/azureResults` against the fake.
- **CLI**: `atn validate --yes --format json` against the fake, plus the exit codes.
- **Clients**:
  - the VS Code E2E stubs the server's Azure fake through `ATN_AZURE_URL` and checks the summary row and the diff command;
  - IntelliJ gets unit tests for the tree building from a recorded `atn/azureResults` JSON.
- **Live check** (manual, documented in the PR): sandbox org, pipeline 2, a local clone of `atn-consumer` with a template edit, then `atn validate --yes --diff`.

## 13. Implementation order (2a)

1. `azure`: client and auth (tenant discovery, az, env), with tests.
2. `azure`: repositories and definitions mapping.
3. `azure`: snapshot build, push and delete.
4. `azure`: preview, problems parser and diff.
5. `engine/azure.go`: plan, run, comparison, last report.
6. CLI `atn validate`.
7. LSP requests and notifications.
8. VS Code: settings, commands, Pipelines view rows, diff provider, consent.
9. IntelliJ: settings (+ parity), action, tree rows, diff, consent.
10. `docs/README.md`, CHANGELOG, `samples/` note. `scripts/build.sh test`.

## 14. Status and what's next

**2a, implemented.** Everything in §6–§7 and §10, in the server, the CLI, VS Code and IntelliJ, behind `azure.enabled` (off by default). Verified live: the sandbox org (MSA) end to end through the CLI and through the LSP path (plan, a pause for consent, validate), and the work org (Entra) by hand for `/preview` and a `refs/atn/*` push as a normal member (H§9.1–9.2). A refused push is handled per repository (D5).

**Differences from this design:**
- Client tests: the VS Code E2E only checks that the feature is off by default, that turning it on gives a plan error for `samples/` (not on Azure Repos, so no network) and that the summary row runs the command. It doesn't drive a check against the fake. IntelliJ has no tree-building unit test yet (only compile + settings parity).
- `samples/` has no Azure note: the samples aren't on Azure Repos, so they can only show the "can't check" plan error.

**Still to verify:**
1. Use it on real changes in the work org for a while: rate of *atn found no error here* (`azureOnly`), problems that can't be placed on a file, timing with many pipelines.
2. `SYSTEM_ACCESSTOKEN` in a real pipeline (the build identity needs *Contribute* / *Create branch* on the repositories it pushes to), and document the YAML for it.
3. Click through the IntelliJ flow on 2026.3+ as well as the 2024.2 sandbox.
4. The E2E and IntelliJ client tests above.

**Gaps in 2a (candidates, smallest first):**
- A templates repository that is only a clone next to the project (`preferLocalClones`) or in the cache is read, not pushed: its changes count only when it is a workspace root (VS Code folder, IntelliJ module) or a second `--root`. Treating a changed local clone as a change source would close it.
- A rejected expansion has no diff (only compiling sides have `finalYaml`).
- No progress per preview in the lens; the summary row only.
- GitHub-hosted repositories (`type: github`) are *not checked*: a snapshot would need a push to GitHub (H§9.6).
- VS Code's own Microsoft sign-in as a token source (D2, H§9.4).

**Next phase: 2b** (§8): test every flow of a template, through the flow matrix (offline, in the engine), a host pipeline and synthetic roots. Open before starting: H§9.3 (cross-project and unauthorised repositories in a synthetic root), and the rate-limit cost of large matrices (H§9.7).

