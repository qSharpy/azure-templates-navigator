# `extends:` samples

A small model of an organisation where platform teams publish templates and
app teams consume them with `extends:`.

```
extends/
├── platform-templates/        repo Platform/platform-templates   (alias @platform)
│   ├── pipelines/
│   │   ├── secure-base.yml       governed base: security stage + the team's stages
│   │   └── dotnet-service.yml    golden path, itself extends secure-base.yml
│   ├── stages/                   build-dotnet.yml, deploy-webapp.yml
│   ├── jobs/                     dotnet-build-job.yml
│   ├── steps/                    publish-artifact.yml
│   └── tests/secure-base-smoke.yml   smoke-test pipeline inside the template repo
├── security-templates/        repo Security/security-templates   (alias @security)
│   └── steps/                    credscan.yml, sbom.yml
└── teams/                     consumer pipelines
    ├── payments/azure-pipelines.yml   happy path through the golden path
    ├── search/azure-pipelines.yml     extends the base directly, several mistakes
    ├── mobile/release.yml             runtime parameters, ${{ if }}, parameters before template
    └── data/
        ├── azure-pipelines.yml        missing @security: fails at queue time (not detected yet)
        └── nightly.yml                '@platfrom' typo
```

Both repositories resolve locally through `samples/.azure-templates-navigator.json`
(`localPaths` keyed by repository name), so no network access is needed.

## Expected diagnostics

`atn check --root samples` should report these for `extends/` and nothing else:

| File | Line | Code |
|---|---|---|
| `platform-templates/pipelines/secure-base.yml` | 36 | `unused-param` (`legacyAgentPool`) |
| `teams/data/nightly.yml` | 24 | `unknown-repository-alias` (`@platfrom`) |
| `teams/search/azure-pipelines.yml` | 43 | `type-mismatch` (`breakGlass: 'yes'`) |
| `teams/search/azure-pipelines.yml` | 45 | `missing-required-param` (`projects`) |
| `teams/search/azure-pipelines.yml` | 48 | `value-not-allowed` (`Releas`) |
| `teams/search/azure-pipelines.yml` | 51 | `value-not-allowed` (`staging`) |
| `teams/search/azure-pipelines.yml` | 54 | `unknown-param` (`dependson`, did you mean `dependsOn`) |

## Known gaps

These would fail in Azure DevOps but are not reported yet:

- **`teams/data/azure-pipelines.yml`**: `secure-base.yml` uses `@security`, which the root
  pipeline never declares. Azure resolves every alias in the expanded pipeline against the
  root pipeline's `resources`, so the run fails at queue time.
- **`teams/search/azure-pipelines.yml`**: the `Reindex` stage `dependsOn: Deploy_staging`. That
  stage exists only because `staging` was passed, and `staging` is already rejected. Checking
  that `dependsOn` names a real stage or job needs the pipeline to be expanded first.
