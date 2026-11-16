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
    ├── analytics/azure-pipelines.yml  correct template calls, but a dependsOn typo and an output variable without isOutput
    └── data/
        ├── azure-pipelines.yml        missing @security: fails at queue time
        └── nightly.yml                '@platfrom' typo
```

Both repositories resolve locally through `samples/.azure-templates-navigator.json`
(`localPaths` keyed by repository name), so no network access is needed.

## Expected diagnostics

`atn check --root samples` should report these for `extends/` and nothing else:

| File | Line | Code |
|---|---|---|
| `platform-templates/pipelines/secure-base.yml` | 36 | `unused-param` (`legacyAgentPool`) |
| `teams/analytics/azure-pipelines.yml` | 41 | `output-variable-not-output` (`metrics.rowCount`) |
| `teams/analytics/azure-pipelines.yml` | 45 | `unknown-dependency` (`Tset`, did you mean `Test`) |
| `teams/data/azure-pipelines.yml` | 25 | `repository-not-declared` (`@security`, used by `secure-base.yml`) |
| `teams/data/nightly.yml` | 24 | `unknown-repository-alias` (`@platfrom`) |
| `teams/search/azure-pipelines.yml` | 43 | `type-mismatch` (`breakGlass: 'yes'`) |
| `teams/search/azure-pipelines.yml` | 45 | `missing-required-param` (`projects`) |
| `teams/search/azure-pipelines.yml` | 48 | `value-not-allowed` (`Releas`) |
| `teams/search/azure-pipelines.yml` | 51 | `value-not-allowed` (`staging`) |
| `teams/search/azure-pipelines.yml` | 54 | `unknown-param` (`dependson`, did you mean `dependsOn`) |

In the `Pipelines:` verdicts printed after the list, `analytics`, both `data` pipelines and
`search` should be ✗. `payments`, `mobile` and `platform-templates/tests/secure-base-smoke.yml`
should be ✓.

## Known gaps

- **`teams/search/azure-pipelines.yml`**: the `Reindex` stage `dependsOn: Deploy_staging`, which
  is not reported. The stage is named `Deploy_${{ parameters.environment }}`, and a name built
  from parameters matches any value. `staging` itself is already rejected by `value-not-allowed`.
