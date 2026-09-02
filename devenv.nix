{
  pkgs,
  inputs,
  config,
  ...
}:
# dev-shared devenv module — the shared dev + CI toolchain for the mattwilkinsonn
# Rust tool repos (jj-hooks, jj-gt). A consumer declares this repo as an input
# and `imports:` this file; it inherits:
#
#   - the exact rust-toolchain.toml Rust pin, built by rust-overlay (pure-eval,
#     no hand-maintained sha256 — the orion pattern, orion/devenv.nix:31-33);
#   - the common linter package set the pre-push gate + CI run;
#   - the shared lint TASK set (ci:markdownlint / ci:actionlint / ci:nixfmt /
#     ci:deadnix) so root-lint coverage is defined once here, not per repo.
#
# Hook backends (pre-commit / prek / lefthook / pkl / hk) are NOT here — they
# are a jj-hooks test-suite concern and live in that repo's own devenv.nix.
#
# rust-toolchain.toml resolution: a relative path literal in a Nix module always
# resolves to the module file's OWN directory, so `./rust-toolchain.toml` here
# would pin every consumer to dev-shared's copy. To honor each consumer's own
# pin we resolve against `config.devenv.root` — devenv's absolute path to the
# importing project root — so the consumer's rust-toolchain.toml is the single
# Rust-version source. The transitivity verdict (does the rust-overlay input
# compose through `imports:`, or must a consumer redeclare it?) is recorded in
# README.md.
let
  rustToolchain =
    (inputs.rust-overlay.lib.mkRustBin { } pkgs).fromRustupToolchainFile
      "${config.devenv.root}/rust-toolchain.toml";
in
{
  packages = with pkgs; [
    # Rust toolchain (rust-toolchain.toml pin) + cc/ld for cargo's link step +
    # the test runner.
    rustToolchain
    stdenv.cc
    cargo-nextest

    # Nix linters — the set the pre-push gate + CI run.
    nixfmt-rfc-style
    deadnix
    statix
    nil

    # Shell + TOML linters.
    shellcheck
    shfmt
    taplo

    # CI / workflow + docs tooling.
    actionlint
    markdownlint-cli2

    # VCS: jj (Matt's review tool; release scripts shell out to it).
    jujutsu
  ];

  # Shared lint task set — root-lint coverage defined once. Each consumer's
  # aggregate `ci` task depends on these (plus its own crate fmt/clippy/test),
  # so markdown/workflow/nix hygiene is gated uniformly across repos.
  #
  # The nix tasks scope to `find -maxdepth 1` (not a recursive `.`) and
  # markdownlint carries inline ignores, both so a lint run never descends into
  # the generated `.devenv/` toolchain tree (its vendored cargo/clippy READMEs
  # and generated .nix files are not ours to lint). A consumer's own
  # `.markdownlint-cli2.jsonc` ignores are additive; these inline globs make the
  # task correct even in a repo that has not added that config yet.
  tasks = {
    "ci:markdownlint".exec =
      ''markdownlint-cli2 "**/*.md" "#**/node_modules/**" "#**/target/**" "#**/.devenv/**" "#**/.direnv/**" "#**/.jj/**"'';
    "ci:actionlint".exec = "actionlint";
    "ci:nixfmt".exec = ''find . -maxdepth 1 -name "*.nix" -print0 | xargs -0 nixfmt --check'';
    "ci:deadnix".exec = ''find . -maxdepth 1 -name "*.nix" -print0 | xargs -0 deadnix --fail'';
  };
}
