// Cut a full release: bump the Rust workspace version, commit, tag, and push.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9._-]+)?\z`)
var repositoryPattern = regexp.MustCompile(`(?m)^\s*repository\s*=\s*"([^"]+)"`)
var githubRepositoryPattern = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+?)(?:\.git)?$`)

type commandResult struct {
	exitCode int
	stdout   string
	stderr   string
}

type dependencies struct {
	run      func(name string, args []string, capture bool) (commandResult, error)
	readFile func(path string) (string, error)
	stdout   io.Writer
	stderr   io.Writer
}

func validateVersion(version string) (string, error) {
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("usage: release vX.Y.Z (or vX.Y.Z-rc.1); got: '%s'", version)
	}
	return strings.TrimPrefix(version, "v"), nil
}

func verifyBumps(cargoToml, bare string) []string {
	wanted := `version = "` + bare + `"`
	for _, line := range strings.Split(cargoToml, "\n") {
		if strings.HasPrefix(line, wanted) {
			return []string{}
		}
	}

	messages := []string{"error: workspace Cargo.toml version didn't bump to " + bare}
	for _, line := range strings.Split(cargoToml, "\n") {
		if strings.HasPrefix(line, "version = ") {
			messages = append(messages, line)
		}
	}
	return messages
}

func actionsURL(readFile func(string) (string, error)) string {
	const generic = "   the release workflow on GitHub Actions"

	text, err := readFile("Cargo.toml")
	if err != nil {
		return generic
	}
	repository := repositoryPattern.FindStringSubmatch(text)
	if len(repository) < 2 {
		return generic
	}
	github := githubRepositoryPattern.FindStringSubmatch(repository[1])
	if len(github) < 3 {
		return generic
	}
	return "   https://github.com/" + github[1] + "/" + github[2] + "/actions/workflows/release.yml"
}

func runCommand(name string, args []string, capture bool) (commandResult, error) {
	command := exec.Command(name, args...)
	result := commandResult{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if capture {
		command.Stdout = &stdout
		command.Stderr = &stderr
	} else {
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
	}

	err := command.Run()
	if capture {
		result.stdout = stdout.String()
		result.stderr = stderr.String()
	}
	if err == nil {
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.exitCode = exitError.ExitCode()
		return result, nil
	}
	var commandError *exec.Error
	if errors.As(err, &commandError) {
		result.exitCode = 127
		if capture {
			result.stderr = err.Error() + "\n"
		} else {
			slog.Error("release command could not start", "command", name, "error", commandError.Err)
		}
		return result, nil
	}
	return result, fmt.Errorf("starting %s: %w", name, err)
}

func runOnce(deps dependencies, argv []string) (int, error) {
	version := ""
	if len(argv) > 0 {
		version = argv[0]
	}
	bare, err := validateVersion(version)
	if err != nil {
		if writeErr := writeLine(deps.stderr, err.Error()); writeErr != nil {
			return 1, writeErr
		}
		return 1, nil
	}

	diff, err := run(deps, "jj", []string{"diff", "--summary", "--ignore-working-copy"}, true)
	if err != nil {
		return 1, err
	}
	if strings.TrimRight(diff.stdout, "\n") != "" {
		if err := writeLine(deps.stderr, "error: working copy @ has uncommitted changes; finalize them first"); err != nil {
			return 1, err
		}
		return 1, nil
	}

	ancestor, err := run(deps, "jj", []string{
		"--ignore-working-copy", "log", "-r", "main & ::@", "-T", "change_id", "--no-graph",
	}, true)
	if err != nil {
		return 1, err
	}
	if !hasNonemptyLine(ancestor.stdout) {
		if err := writeLine(deps.stderr, "error: @ is not a descendant of main (run: jj rebase -d main)"); err != nil {
			return 1, err
		}
		return 1, nil
	}

	tags, err := run(deps, "jj", []string{
		"--ignore-working-copy", "tag", "list", "-T", `name ++ "\n"`,
	}, true)
	if err != nil {
		return 1, err
	}
	for _, tag := range strings.Split(tags.stdout, "\n") {
		if tag == version {
			if err := writeLine(deps.stderr, "error: tag "+version+" already exists"); err != nil {
				return 1, err
			}
			return 1, nil
		}
	}

	cargoEdit, err := run(deps, "cargo", []string{"set-version", "--help"}, true)
	if err != nil {
		return 1, err
	}
	if cargoEdit.exitCode != 0 {
		if err := writeLine(deps.stderr, "error: cargo-edit not installed (run: cargo install --locked cargo-edit)"); err != nil {
			return 1, err
		}
		return 1, nil
	}

	if err := writeLine(deps.stdout, "==> Bumping Rust workspace + members to "+bare+"..."); err != nil {
		return 1, err
	}
	setVersion, err := run(deps, "cargo", []string{"set-version", "--workspace", bare}, false)
	if err != nil {
		return 1, err
	}
	if setVersion.exitCode != 0 {
		return setVersion.exitCode, nil
	}
	if err := writeLine(deps.stdout, ""); err != nil {
		return 1, err
	}

	if err := writeLine(deps.stdout, "==> Updating Cargo.lock..."); err != nil {
		return 1, err
	}
	cargoUpdate, err := run(deps, "cargo", []string{"update", "--workspace"}, false)
	if err != nil {
		return 1, err
	}
	if cargoUpdate.exitCode != 0 {
		return cargoUpdate.exitCode, nil
	}
	if err := writeLine(deps.stdout, ""); err != nil {
		return 1, err
	}

	if err := writeLine(deps.stdout, "==> Verifying bumps..."); err != nil {
		return 1, err
	}
	cargoToml, err := deps.readFile("Cargo.toml")
	if err != nil {
		return 1, fmt.Errorf("reading Cargo.toml: %w", err)
	}
	if failures := verifyBumps(cargoToml, bare); len(failures) > 0 {
		for _, line := range failures {
			if err := writeLine(deps.stderr, line); err != nil {
				return 1, err
			}
		}
		return 1, nil
	}
	if err := writeLine(deps.stdout, ""); err != nil {
		return 1, err
	}

	if err := writeLine(deps.stdout, "==> Committing release bump as a new jj change on top of @..."); err != nil {
		return 1, err
	}
	commit, err := run(deps, "jj", []string{"commit", "-m", "release: " + version}, false)
	if err != nil {
		return 1, err
	}
	if commit.exitCode != 0 {
		return commit.exitCode, nil
	}
	if err := writeLine(deps.stdout, ""); err != nil {
		return 1, err
	}

	if err := writeLine(deps.stdout, "==> Tagging @- with "+version+"..."); err != nil {
		return 1, err
	}
	tag, err := run(deps, "jj", []string{"tag", "set", version, "-r", "@-"}, false)
	if err != nil {
		return 1, err
	}
	if tag.exitCode != 0 {
		return tag.exitCode, nil
	}
	if err := writeLine(deps.stdout, ""); err != nil {
		return 1, err
	}

	if err := writeLine(deps.stdout, "==> Advancing main to the release commit..."); err != nil {
		return 1, err
	}
	bookmark, err := run(deps, "jj", []string{"bookmark", "set", "main", "-r", "@-"}, false)
	if err != nil {
		return 1, err
	}
	if bookmark.exitCode != 0 {
		return bookmark.exitCode, nil
	}
	if err := writeLine(deps.stdout, ""); err != nil {
		return 1, err
	}

	if err := writeLine(deps.stdout, "==> Exporting refs to git..."); err != nil {
		return 1, err
	}
	if _, err := run(deps, "jj", []string{"--ignore-working-copy", "git", "export"}, true); err != nil {
		return 1, err
	}
	if err := writeLine(deps.stdout, ""); err != nil {
		return 1, err
	}

	if err := writeLine(deps.stdout, "==> Pushing main..."); err != nil {
		return 1, err
	}
	pushMain, err := run(deps, "jj", []string{"git", "push", "-b", "main"}, false)
	if err != nil {
		return 1, err
	}
	if pushMain.exitCode != 0 {
		return pushMain.exitCode, nil
	}
	if err := writeLine(deps.stdout, ""); err != nil {
		return 1, err
	}

	if err := writeLine(deps.stdout, "==> Pushing tag "+version+" (triggers release.yml)..."); err != nil {
		return 1, err
	}
	pushTags, err := run(deps, "jj-hp", []string{"push-tags", version}, false)
	if err != nil {
		return 1, err
	}
	if pushTags.exitCode != 0 {
		return pushTags.exitCode, nil
	}
	if err := writeLine(deps.stdout, ""); err != nil {
		return 1, err
	}

	if err := writeLine(deps.stdout, "✅ Done. Watch the release workflow:"); err != nil {
		return 1, err
	}
	if err := writeLine(deps.stdout, actionsURL(deps.readFile)); err != nil {
		return 1, err
	}
	return 0, nil
}

func run(deps dependencies, name string, args []string, capture bool) (commandResult, error) {
	result, err := deps.run(name, args, capture)
	if err != nil {
		return result, fmt.Errorf("running %s: %w", name, err)
	}
	return result, nil
}

func hasNonemptyLine(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			return true
		}
	}
	return false
}

func writeLine(w io.Writer, line string) error {
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		return fmt.Errorf("writing release output: %w", err)
	}
	return nil
}

func main() {
	deps := dependencies{
		run:      runCommand,
		readFile: readFile,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	}
	code, err := runOnce(deps, os.Args[1:])
	if err != nil {
		if writeErr := writeLine(os.Stderr, "error: "+err.Error()); writeErr != nil {
			slog.Error("release failed", "error", writeErr)
		}
		os.Exit(1)
	}
	os.Exit(code)
}

func readFile(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(contents), nil
}
