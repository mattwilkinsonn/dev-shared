package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type recordedCommand struct {
	name string
	args []string
}

type fakeOptions struct {
	responses map[string]commandResult
	files     map[string]string
	fileError error
}

type fakeRunner struct {
	options  fakeOptions
	commands []recordedCommand
	stdout   strings.Builder
	stderr   strings.Builder
}

func (f *fakeRunner) dependencies() dependencies {
	return dependencies{
		run:      f.run,
		readFile: f.readFile,
		stdout:   &f.stdout,
		stderr:   &f.stderr,
	}
}

func (f *fakeRunner) run(name string, args []string, _ bool) (commandResult, error) {
	f.commands = append(f.commands, recordedCommand{name: name, args: append([]string{}, args...)})
	full := name + " " + strings.Join(args, " ")
	matchedLength := -1
	var result commandResult
	for key, response := range f.options.responses {
		if strings.HasPrefix(full, key) && len(key) > matchedLength {
			matchedLength = len(key)
			result = response
		}
	}
	return result, nil
}

func (f *fakeRunner) readFile(path string) (string, error) {
	if f.options.fileError != nil {
		return "", f.options.fileError
	}
	return f.options.files[path], nil
}

func TestValidateVersion(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		wantErr  bool
	}{
		{name: "stable", input: "v0.3.0", expected: "0.3.0"},
		{name: "prerelease", input: "v1.2.3-rc.4", expected: "1.2.3-rc.4"},
		{name: "missing prefix", input: "0.3.0", wantErr: true},
		{name: "garbage", input: "banana", wantErr: true},
		{name: "two components", input: "v1.2", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateVersion(test.input)
			if err != nil {
				if test.wantErr {
					return
				}
				t.Fatalf("validateVersion() error = %v", err)
			}
			if test.wantErr {
				t.Error("validateVersion() accepted an invalid version")
			}
			if got != test.expected {
				t.Errorf("validateVersion() = %q, want %q", got, test.expected)
			}
		})
	}
}

func TestVerifyBumps(t *testing.T) {
	tests := []struct {
		name      string
		cargoToml string
		bare      string
		wantError bool
	}{
		{
			name:      "updated version",
			cargoToml: "version = \"0.4.0\"\nedition = \"2024\"\n",
			bare:      "0.4.0",
		},
		{
			name:      "stale version",
			cargoToml: "version = \"0.3.7\"\nedition = \"2024\"\n",
			bare:      "0.4.0",
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := verifyBumps(test.cargoToml, test.bare)
			if (len(got) > 0) != test.wantError {
				t.Errorf("verifyBumps() returned %d failures, want error %t", len(got), test.wantError)
			}
		})
	}
}

func TestActionsURL(t *testing.T) {
	tests := []struct {
		name    string
		content string
		readErr error
		wantURL string
		generic bool
	}{
		{
			name:    "github repository",
			content: "[package]\nname = \"jj-hooks\"\nrepository = \"https://github.com/mattwilkinsonn/jj-hooks\"\n",
			wantURL: "https://github.com/mattwilkinsonn/jj-hooks/actions/workflows/release.yml",
		},
		{
			name:    "repository with git suffix",
			content: "repository = \"https://github.com/mattwilkinsonn/jj-gt.git\"\n",
			wantURL: "https://github.com/mattwilkinsonn/jj-gt/actions/workflows/release.yml",
		},
		{name: "missing repository", content: "[package]\nname = \"x\"\n", generic: true},
		{name: "non-github repository", content: "repository = \"https://gitlab.com/x/y\"\n", generic: true},
		{name: "unreadable Cargo manifest", readErr: errors.New("missing"), generic: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := actionsURL(func(string) (string, error) {
				return test.content, test.readErr
			})
			if test.generic {
				if strings.Contains(got, "https://github.com/") {
					t.Error("actionsURL() returned a repository URL without a valid repository")
				}
				return
			}
			if !strings.Contains(got, test.wantURL) {
				t.Errorf("actionsURL() did not return the derived workflow URL")
			}
		})
	}
}

func TestRunOnceGuards(t *testing.T) {
	happyResponses := map[string]commandResult{
		"jj diff":                           {},
		"jj --ignore-working-copy log":      {stdout: "abcdef123456\n"},
		"jj --ignore-working-copy tag list": {stdout: "v0.1.0\nv0.2.0\n"},
		"cargo set-version --help":          {},
	}
	tests := []struct {
		name      string
		version   string
		options   fakeOptions
		wantCode  int
		wantCalls int
	}{
		{name: "invalid version", version: "nope", wantCode: 1, wantCalls: 0},
		{
			name: "dirty working copy", version: "v0.4.0",
			options: fakeOptions{responses: map[string]commandResult{
				"jj diff": {stdout: "M Cargo.toml\n"},
			}},
			wantCode: 1, wantCalls: 1,
		},
		{
			name: "not descended from main", version: "v0.4.0",
			options: fakeOptions{responses: map[string]commandResult{
				"jj diff":                      {},
				"jj --ignore-working-copy log": {stdout: "\n"},
			}},
			wantCode: 1, wantCalls: 2,
		},
		{
			name: "existing tag", version: "v0.4.0",
			options: fakeOptions{responses: map[string]commandResult{
				"jj diff":                           {},
				"jj --ignore-working-copy log":      {stdout: "abcdef\n"},
				"jj --ignore-working-copy tag list": {stdout: "v0.3.9\nv0.4.0\n"},
			}},
			wantCode: 1, wantCalls: 3,
		},
		{
			name: "missing cargo-edit", version: "v0.4.0",
			options: fakeOptions{responses: map[string]commandResult{
				"jj diff":                           {},
				"jj --ignore-working-copy log":      {stdout: "abcdef\n"},
				"jj --ignore-working-copy tag list": {stdout: ""},
				"cargo set-version --help":          {exitCode: 1},
			}},
			wantCode: 1, wantCalls: 4,
		},
		{
			name: "bump verification failure", version: "v0.4.0",
			options: fakeOptions{
				responses: happyResponses,
				files:     map[string]string{"Cargo.toml": "version = \"0.3.7\"\n"},
			},
			wantCode: 1, wantCalls: 6,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeRunner{options: test.options}
			got, err := runOnce(fake.dependencies(), []string{test.version})
			if err != nil {
				t.Fatalf("runOnce() error = %v", err)
			}
			if got != test.wantCode {
				t.Errorf("runOnce() = %d, want %d", got, test.wantCode)
			}
			if len(fake.commands) != test.wantCalls {
				t.Errorf("runOnce() issued %d commands, want %d", len(fake.commands), test.wantCalls)
			}
			if fake.stderr.Len() == 0 {
				t.Error("runOnce() did not report the guard failure")
			}
		})
	}
}

func TestRunOnceHappyPath(t *testing.T) {
	fake := &fakeRunner{options: fakeOptions{
		responses: map[string]commandResult{
			"jj --ignore-working-copy log":      {stdout: "abcdef123456\n"},
			"jj --ignore-working-copy tag list": {stdout: "v0.1.0\nv0.2.0\n"},
		},
		files: map[string]string{
			"Cargo.toml": "version = \"0.4.0\"\nrepository = \"https://github.com/mattwilkinsonn/jj-hooks\"\n",
		},
	}}
	got, err := runOnce(fake.dependencies(), []string{"v0.4.0"})
	if err != nil {
		t.Fatalf("runOnce() error = %v", err)
	}
	if got != 0 {
		t.Fatalf("runOnce() = %d, want 0", got)
	}
	want := []recordedCommand{
		{name: "jj", args: []string{"diff", "--summary", "--ignore-working-copy"}},
		{name: "jj", args: []string{"--ignore-working-copy", "log", "-r", "main & ::@", "-T", "change_id", "--no-graph"}},
		{name: "jj", args: []string{"--ignore-working-copy", "tag", "list", "-T", `name ++ "\n"`}},
		{name: "cargo", args: []string{"set-version", "--help"}},
		{name: "cargo", args: []string{"set-version", "--workspace", "0.4.0"}},
		{name: "cargo", args: []string{"update", "--workspace"}},
		{name: "jj", args: []string{"commit", "-m", "release: v0.4.0"}},
		{name: "jj", args: []string{"tag", "set", "v0.4.0", "-r", "@-"}},
		{name: "jj", args: []string{"bookmark", "set", "main", "-r", "@-"}},
		{name: "jj", args: []string{"--ignore-working-copy", "git", "export"}},
		{name: "jj", args: []string{"git", "push", "-b", "main"}},
		{name: "jj-hp", args: []string{"push-tags", "v0.4.0"}},
	}
	if len(fake.commands) != len(want) {
		t.Fatalf("runOnce() issued %d commands, want %d", len(fake.commands), len(want))
	}
	for index, command := range want {
		got := fake.commands[index]
		if got.name != command.name || strings.Join(got.args, "\x00") != strings.Join(command.args, "\x00") {
			t.Errorf("command %d = %#v, want %#v", index, got, command)
		}
	}
	if !strings.HasSuffix(fake.stdout.String(), "   https://github.com/mattwilkinsonn/jj-hooks/actions/workflows/release.yml\n") {
		t.Error("success output omitted the workflow link")
	}
}

func TestRunOnceFailureStopsAtCommand(t *testing.T) {
	tests := []struct {
		name         string
		command      string
		args         []string
		wantExit     int
		wantCommands int
	}{
		{name: "set version", command: "cargo", args: []string{"set-version", "--workspace", "0.4.0"}, wantExit: 11, wantCommands: 5},
		{name: "cargo update", command: "cargo", args: []string{"update", "--workspace"}, wantExit: 12, wantCommands: 6},
		{name: "commit", command: "jj", args: []string{"commit", "-m", "release: v0.4.0"}, wantExit: 13, wantCommands: 7},
		{name: "set tag", command: "jj", args: []string{"tag", "set", "v0.4.0", "-r", "@-"}, wantExit: 14, wantCommands: 8},
		{name: "set bookmark", command: "jj", args: []string{"bookmark", "set", "main", "-r", "@-"}, wantExit: 15, wantCommands: 9},
		{name: "push main", command: "jj", args: []string{"git", "push", "-b", "main"}, wantExit: 16, wantCommands: 11},
		{name: "push tag", command: "jj-hp", args: []string{"push-tags", "v0.4.0"}, wantExit: 17, wantCommands: 12},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newHappyRunner()
			key := test.command + " " + strings.Join(test.args, " ")
			fake.options.responses[key] = commandResult{exitCode: test.wantExit}

			got, err := runOnce(fake.dependencies(), []string{"v0.4.0"})
			if err != nil {
				t.Fatalf("runOnce() error = %v", err)
			}
			if got != test.wantExit {
				t.Errorf("runOnce() = %d, want %d", got, test.wantExit)
			}
			if len(fake.commands) != test.wantCommands {
				t.Fatalf("runOnce() issued %d commands, want %d", len(fake.commands), test.wantCommands)
			}
			last := fake.commands[len(fake.commands)-1]
			if last.name != test.command || strings.Join(last.args, "\x00") != strings.Join(test.args, "\x00") {
				t.Errorf("last command = %#v, want command %q", last, test.command)
			}
			if strings.Contains(fake.stdout.String(), "✅ Done.") {
				t.Error("failed release printed completion output")
			}
		})
	}
}

func TestRunOnceIgnoresGitExportFailure(t *testing.T) {
	fake := newHappyRunner()
	fake.options.responses["jj --ignore-working-copy git export"] = commandResult{exitCode: 1}

	got, err := runOnce(fake.dependencies(), []string{"v0.4.0"})
	if err != nil {
		t.Fatalf("runOnce() error = %v", err)
	}
	if got != 0 {
		t.Errorf("runOnce() = %d, want 0", got)
	}
	if !strings.Contains(fake.stdout.String(), "✅ Done.") {
		t.Error("release did not complete after git export failed")
	}
}

func newHappyRunner() *fakeRunner {
	return &fakeRunner{options: fakeOptions{
		responses: map[string]commandResult{
			"jj --ignore-working-copy log":      {stdout: "abcdef123456\n"},
			"jj --ignore-working-copy tag list": {stdout: "v0.1.0\nv0.2.0\n"},
		},
		files: map[string]string{
			"Cargo.toml": "version = \"0.4.0\"\nrepository = \"https://github.com/mattwilkinsonn/jj-hooks\"\n",
		},
	}}
}

func TestRunCommand(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		args       []string
		stdin      string
		wantStdout string
		wantExit   int
		wantError  bool
	}{
		{
			name:       "stdin forwarded",
			command:    "sh",
			args:       []string{"-c", "cat"},
			stdin:      "input data",
			wantStdout: "input data",
		},
		{name: "nonzero exit", command: "sh", args: []string{"-c", "exit 23"}, wantExit: 23},
		{name: "signal termination", command: "sh", args: []string{"-c", "kill -TERM $$"}, wantExit: 143},
		{name: "missing executable", command: "release-command-not-found-test", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalStdin := os.Stdin
			stdin, err := os.CreateTemp(t.TempDir(), "stdin")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(stdin, test.stdin); err != nil {
				t.Fatal(err)
			}
			if _, err := stdin.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			os.Stdin = stdin
			t.Cleanup(func() {
				os.Stdin = originalStdin
				if err := stdin.Close(); err != nil {
					t.Errorf("closing test stdin: %v", err)
				}
			})

			result, err := runCommand(test.command, test.args, true)
			if (err != nil) != test.wantError {
				t.Fatalf("runCommand() error = %v, want error %t", err, test.wantError)
			}
			if test.wantError {
				return
			}
			if result.exitCode != test.wantExit {
				t.Errorf("runCommand() exit code = %d, want %d", result.exitCode, test.wantExit)
			}
			if result.stdout != test.wantStdout {
				t.Errorf("runCommand() stdout = %q, want %q", result.stdout, test.wantStdout)
			}
		})
	}
}
