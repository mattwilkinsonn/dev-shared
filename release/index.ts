// Cut a full release: bump the Rust workspace version, commit, tag, push.
//
//   release v0.3.0
//
//   1. Validate the version string + working-copy state.
//   2. Bump the workspace Cargo.toml to the new version.
//      `cargo set-version --workspace` handles all Rust members + any
//      internal path-dep version field in one shot.
//   3. Commit "release: vX.Y.Z" as a new jj change on top of @.
//   4. Tag @- with the version.
//   5. Advance the local `main` bookmark to the release commit.
//   6. Push main + the tag — the tag push triggers release.yml.
//
// Tag format: vX.Y.Z (stable) or vX.Y.Z-rc.N (pre-release). release.yml
// skips the crates.io publish job for pre-releases. Homebrew tap version
// bumping is owned by the tap's own `bump-tap` CI job, not this driver.
//
// Exit codes:
//   0 - release cut successfully
//   1 - a guard failed (bad version, dirty @, non-descendant, existing
//       tag, missing cargo-edit, or a bump that didn't take)
//   N - a shelled release step (cargo/jj/jj-hp) exited N; propagated
//       verbatim (bash `set -e`).

import { $ } from "bun";

// Version tag shape: vX.Y.Z (stable) or vX.Y.Z-<pre> (pre-release).
const VERSION_RE = /^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9._-]+)?$/;

// Validate the CLI version arg and strip the leading `v` to the bare
// SemVer used by cargo/package.json version fields. Returns the exact
// usage string bash emitted on a bad arg.
function validateVersion(v: string): { bare: string } | { error: string } {
	if (!VERSION_RE.test(v)) {
		return {
			error: `usage: release vX.Y.Z (or vX.Y.Z-rc.1); got: '${v}'`,
		};
	}
	return { bare: v.slice(1) };
}

// Port of the post-bump `grep -q` verification block. Returns the exact
// stderr lines bash would emit for the FIRST failing check (bash exits
// at the first failure), or [] when the bump took.
function verifyBumps(cargoToml: string, bare: string): string[] {
	// grep -q "^version = \"$bare\"" Cargo.toml
	const wanted = `version = "${bare}"`;
	if (!cargoToml.split("\n").some((line) => line.startsWith(wanted))) {
		const msgs = [`error: workspace Cargo.toml version didn't bump to ${bare}`];
		// grep "^version = " Cargo.toml >&2
		for (const line of cargoToml.split("\n")) {
			if (line.startsWith("version = ")) msgs.push(line);
		}
		return msgs;
	}
	return [];
}

// Derive the release-workflow URL from the consuming repo's Cargo.toml
// `[package].repository` field. One shared copy of this driver runs in
// multiple repos, so the slug can't be hard-coded. This runs AFTER a
// successful push, so it must never throw and never change the return
// code — any parse/read failure falls back to a generic message.
async function actionsUrl(
	readFile: (path: string) => Promise<string>,
): Promise<string> {
	const generic = "   the release workflow on GitHub Actions";
	let text: string;
	try {
		text = await readFile("Cargo.toml");
	} catch {
		return generic;
	}
	// Grab the first `repository = "..."` line. Cargo.toml here is a
	// single-package manifest, so a plain scan is enough — no TOML dep.
	const match = text.match(/^\s*repository\s*=\s*"([^"]+)"/m);
	const repo = match?.[1];
	if (!repo) return generic;
	// https://github.com/<owner>/<name>[.git]
	const gh = repo.match(/^https:\/\/github\.com\/([^/]+)\/([^/]+?)(?:\.git)?$/);
	if (!gh) return generic;
	return `   https://github.com/${gh[1]}/${gh[2]}/actions/workflows/release.yml`;
}

type ShResult = { exitCode: number; stdout: string; stderr: string };
type ShOpts = { capture?: boolean };

// Dependencies runOnce takes from its environment. Tests pass fakes;
// production wires real Bun.$/Bun.file + console.
type Deps = {
	// Run a binary. `capture: true` mirrors bash `$(...)` / `>/dev/null`
	// (stdout/stderr captured, exit code inspected); otherwise stdio is
	// inherited so cargo/jj stream to the terminal like the bash did.
	sh: (cmd: string, args: string[], opts?: ShOpts) => Promise<ShResult>;
	readFile: (path: string) => Promise<string>;
	log: (msg: string) => void;
	err: (msg: string) => void;
};

async function runOnce(deps: Deps, argv: string[]): Promise<number> {
	const { sh, readFile, log, err } = deps;

	const version = argv[0] ?? "";
	const parsed = validateVersion(version);
	if ("error" in parsed) {
		err(parsed.error);
		return 1;
	}
	const bare = parsed.bare;

	// Require a clean @ — release commits should not include unrelated work.
	const diff = await sh("jj", ["diff", "--summary", "--ignore-working-copy"], {
		capture: true,
	});
	if (diff.stdout.replace(/\n+$/, "").length > 0) {
		err("error: working copy @ has uncommitted changes; finalize them first");
		return 1;
	}

	// Require `main` to be an ancestor of `@` so the release commit lands
	// on top of main. Otherwise advancing main to @- would move it
	// backwards or sideways onto an unrelated branch.
	const ancestor = await sh(
		"jj",
		[
			"--ignore-working-copy",
			"log",
			"-r",
			"main & ::@",
			"-T",
			"change_id",
			"--no-graph",
		],
		{ capture: true },
	);
	// bash: `... | grep -q .` — succeeds iff some line has ≥1 char.
	if (!ancestor.stdout.split("\n").some((line) => line.length > 0)) {
		err("error: @ is not a descendant of main (run: jj rebase -d main)");
		return 1;
	}

	// Refuse to re-tag an existing version.
	const tags = await sh(
		"jj",
		["--ignore-working-copy", "tag", "list", "-T", 'name ++ "\\n"'],
		{ capture: true },
	);
	// bash: `... | grep -qx "$version"` — a whole line equal to version.
	if (tags.stdout.split("\n").some((line) => line === version)) {
		err(`error: tag ${version} already exists`);
		return 1;
	}

	const cargoEdit = await sh("cargo", ["set-version", "--help"], {
		capture: true,
	});
	if (cargoEdit.exitCode !== 0) {
		err(
			"error: cargo-edit not installed (run: cargo install --locked cargo-edit)",
		);
		return 1;
	}

	log(`==> Bumping Rust workspace + members to ${bare}...`);
	const setVersion = await sh("cargo", ["set-version", "--workspace", bare]);
	if (setVersion.exitCode !== 0) return setVersion.exitCode;
	log("");

	log("==> Updating Cargo.lock...");
	const cargoUpdate = await sh("cargo", ["update", "--workspace"]);
	if (cargoUpdate.exitCode !== 0) return cargoUpdate.exitCode;
	log("");

	log("==> Verifying bumps...");
	const failures = verifyBumps(await readFile("Cargo.toml"), bare);
	if (failures.length > 0) {
		for (const line of failures) err(line);
		return 1;
	}
	log("");

	log("==> Committing release bump as a new jj change on top of @...");
	const commit = await sh("jj", ["commit", "-m", `release: ${version}`]);
	if (commit.exitCode !== 0) return commit.exitCode;
	log("");

	log(`==> Tagging @- with ${version}...`);
	const tag = await sh("jj", ["tag", "set", version, "-r", "@-"]);
	if (tag.exitCode !== 0) return tag.exitCode;
	log("");

	log("==> Advancing main to the release commit...");
	const bookmark = await sh("jj", ["bookmark", "set", "main", "-r", "@-"]);
	if (bookmark.exitCode !== 0) return bookmark.exitCode;
	log("");

	log("==> Exporting refs to git...");
	// bash: `... >/dev/null 2>&1 || true` — output discarded, failure ignored.
	await sh("jj", ["--ignore-working-copy", "git", "export"], { capture: true });
	log("");

	log("==> Pushing main...");
	const pushMain = await sh("jj", ["git", "push", "-b", "main"]);
	if (pushMain.exitCode !== 0) return pushMain.exitCode;
	log("");

	log(`==> Pushing tag ${version} (triggers release.yml)...`);
	const pushTags = await sh("jj-hp", ["push-tags", version]);
	if (pushTags.exitCode !== 0) return pushTags.exitCode;
	log("");

	log("✅ Done. Watch the release workflow:");
	log(await actionsUrl(readFile));
	return 0;
}

export type { Deps, ShOpts, ShResult };
export { actionsUrl, runOnce, validateVersion, verifyBumps };

// `import.meta.main` is true only when this file is the entry point
// (`bun run index.ts`). Under `bun test`, the test file is the entry and
// this stays false, so main never runs.
if (import.meta.main) {
	process.exit(
		await runOnce(
			{
				sh: async (cmd, args, opts) => {
					if (opts?.capture) {
						const r = await $`${cmd} ${args}`.nothrow().quiet();
						return {
							exitCode: r.exitCode,
							stdout: r.stdout.toString(),
							stderr: r.stderr.toString(),
						};
					}
					const r = await $`${cmd} ${args}`.nothrow();
					return { exitCode: r.exitCode, stdout: "", stderr: "" };
				},
				readFile: (path) => Bun.file(path).text(),
				log: (msg) => console.log(msg),
				err: (msg) => console.error(msg),
			},
			process.argv.slice(2),
		),
	);
}
