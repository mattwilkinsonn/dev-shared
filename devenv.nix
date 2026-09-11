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

  # jj-hooks, pinned to a released tag. Ships two binaries; `jj-hp` is the one
  # that matters here — the release script's tag push goes through
  # `jj-hp push-tags`, because jj has no native tag push (checked through 0.43).
  #
  # Pinned in-file rather than as a flake input on purpose: module inputs do not
  # compose through `imports:` (README.md), so an input would have to be
  # redeclared in every consumer's devenv.yaml. Both hashes are re-pinned on a
  # jj-hooks release; `cargoHash` changes only when Cargo.lock does.
  #
  # jj-hooks itself overrides this with a build of its own working tree, so its
  # shell tests the code being edited instead of the pin.
  jj-hooks = pkgs.rustPlatform.buildRustPackage {
    pname = "jj-hooks";
    version = "0.3.12";
    src = pkgs.fetchFromGitHub {
      owner = "mattwilkinsonn";
      repo = "jj-hooks";
      rev = "73dc3dd1830a637ff4a774b44738d4b329d566cd";
      hash = "sha256-1jJg199ppkqtDPAxrJtc1G42kXjP0BJDEWv619nYVus=";
    };
    cargoHash = "sha256-nQ5gVsHVPNfIqTUaP6ep4EeY5KfMrj4KV5DhY3ZzqGI=";
    # The suite drives real jj repos and hook backends; jj-hooks' own CI gates
    # that. Building it here only needs the binaries.
    doCheck = false;
  };
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

    # VCS: jj (Matt's review tool; release scripts shell out to it) plus
    # jj-hp, which the release script uses to push tags.
    jujutsu
    jj-hooks
  ];

  # The release driver, shipped once from here rather than copied into each
  # tool repo. `${./release/index.ts}` is a path literal, so it resolves
  # against THIS file's directory and reaches dev-shared's copy even when a
  # consumer imports the module. Running bun by store path keeps it off the
  # shell PATH, so a consumer's shell does not gain a bun it never asked for.
  scripts.release.exec = ''exec ${pkgs.bun}/bin/bun ${./release/index.ts} "$@"'';

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
