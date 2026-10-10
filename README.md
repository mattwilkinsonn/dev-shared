# dev-shared

Shared dev tooling for the `mattwilkinsonn` Rust tool repos (`jj-hooks`,
`jj-gt`). One place to fix a toolchain, CI, or Renovate change and inherit it
everywhere, instead of N copies drifting across N repos.

Four surfaces, each consumed independently.

## 1. devenv module (`devenv.nix`)

The shared dev + CI toolchain: the `rust-toolchain.toml` Rust pin (built by
`rust-overlay`, pure-eval, no hand-maintained hash), the linter set
(nixfmt/deadnix/statix/nil, shellcheck/shfmt/taplo, actionlint,
markdownlint-cli2), `cargo-nextest`, `jujutsu`, and the shared lint **task set**
(`ci:markdownlint` / `ci:actionlint` / `ci:nixfmt` / `ci:deadnix`).

Consume it from a repo's `devenv.yaml`:

```yaml
inputs:
  nixpkgs:
    url: github:cachix/devenv-nixpkgs/rolling
  # REQUIRED — see "Input transitivity" below. The module uses
  # inputs.rust-overlay, and module inputs do NOT compose transitively, so each
  # consumer redeclares this 2-line input itself.
  rust-overlay:
    url: github:oxalica/rust-overlay
    inputs:
      nixpkgs:
        follows: nixpkgs
  dev-shared:
    url: github:mattwilkinsonn/dev-shared
    flake: false
imports:
  - dev-shared
```

The consumer's own `rust-toolchain.toml` (at its repo root) is the Rust-version
source — the module resolves `${config.devenv.root}/rust-toolchain.toml`, i.e.
the *consumer's* file, not this repo's.

### Input transitivity (verified)

**Module inputs do NOT compose transitively into a consumer.** Importing the
module via `imports: [dev-shared]` reaches the module code but leaves
`inputs.rust-overlay` unresolved (`error: attribute 'rust-overlay' missing`).
**Each consumer MUST redeclare the 2-line `rust-overlay` input** in its own
`devenv.yaml` (shown above); with it redeclared, the shared module builds the
toolchain correctly. Verified against devenv 2.1.2 with a local-path consumer
fixture (evaluated `rustc --version` → 1.96.0). Two lines per repo; no
workaround needed beyond that.

## 2. Composite action (`setup-devenv/`)

Installs Nix + devenv, warms the shell, and caches devenv's eval state (the Nix
store is cached by the bundled Determinate action). The CI bootstrap for every
consumer. The caller job MUST grant `id-token: write` (the Determinate Nix
action needs OIDC):

```yaml
- uses: mattwilkinsonn/dev-shared/setup-devenv@v1
```

## 3. Reusable CI workflow (`.github/workflows/rust-devenv-ci.yml`)

The uniform single-crate gate — one `gate` job running `devenv tasks run ci`.
A consumer stubs it:

```yaml
# .github/workflows/ci.yml
name: ci
on:
  pull_request:
jobs:
  ci:
    uses: mattwilkinsonn/dev-shared/.github/workflows/rust-devenv-ci.yml@v1
    permissions:
      contents: read
      id-token: write
```

A repo with extra jobs (e.g. `jj-gt`'s fork-gated live tests) does NOT use this
workflow — it hand-writes `ci.yml` and calls the composite action (2) directly.

**Consumer wiring (how the shared lint tasks join the gate):** `devenv tasks run
ci` is a **namespace-prefix selector** — it runs every task named `ci:*`. The
module's `ci:markdownlint` / `ci:actionlint` / `ci:nixfmt` / `ci:deadnix` join
automatically just by being in the `ci:` namespace, alongside the consumer's own
`ci:test` / `ci:clippy` / `ci:fmt`. No `before`/`after` wiring and no aggregate
task is needed — and note devenv **rejects a task literally named `ci`**
(names must be `namespace:name`), so a consumer adds `ci:<crate-check>` tasks,
never a bare `ci`.

## 4. Renovate preset (`renovate-preset.json5`)

```json5
{ extends: ["github>mattwilkinsonn/dev-shared//renovate-preset.json5"] }
```

Read the preset header for the two hosted-Renovate limits it documents
(rust-overlay lockstep needs a self-hosted bot; the `nix` manager can't see
`devenv.lock`). The devenv-lock update path is the scheduled workflow (below).

## Scheduled devenv-lock updates (`.github/workflows/devenv-update.yml`)

Hosted Renovate cannot update `devenv.lock`, so this workflow opens a weekly
`devenv update` PR. It opens the PR with a GitHub App installation token, so the
PR's CI runs (a `GITHUB_TOKEN`-opened PR triggers no workflows) and no repo needs
"Allow GitHub Actions to create and approve pull requests". A consumer stubs it:

```yaml
jobs:
  devenv-update:
    uses: mattwilkinsonn/dev-shared/.github/workflows/devenv-update.yml@v1
    permissions:
      contents: read
      id-token: write
    with:
      app-client-id: ${{ vars.DEVENV_UPDATE_APP_CLIENT_ID }}
    secrets:
      DEVENV_UPDATE_APP_PRIVATE_KEY: ${{ secrets.DEVENV_UPDATE_APP_PRIVATE_KEY }}
```

The App needs `contents: write` and `pull-requests: write`, installed on the
consumer. The bot push runs with git hooks disabled; the PR's CI runs the gate.

## Versioning

Consumers pin `@v1` (a moving major tag): a fix propagates on the next run
without a per-repo bump. A moving tag gives Renovate's `github-actions` manager
nothing to track, so propagation is entirely tag-move-driven — switch to
`@<sha>` pins if you want each bump reviewed.

**The `v1` tag must exist for anything to resolve.** This repo's own scheduled
`devenv-update` self-references `setup-devenv@v1`, and every consumer pins `@v1`,
so `v1` MUST be created (pointed at the merge commit) immediately after this PR
merges — otherwise the first cron / first consumer run fails to resolve the
action. dev-shared's *own PR* CI is unaffected (it uses `./setup-devenv`, a local
checkout).
