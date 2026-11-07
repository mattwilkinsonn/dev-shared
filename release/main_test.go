package main

import (
	"errors"
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
			if (err != nil) != test.wantErr {
				t.Fatalf("validateVersion(%q) error = %v, want error %t", test.input, err, test.wantErr)
			}
			if got != test.expected {
				t.Errorf("validateVersion(%q) = %q, want %q", test.input, got, test.expected)
			}
		})
	}
}

func TestVerifyBumps(t *testing.T) {
	tests := []struct {
		name      string
		cargoToml string
		bare      string
		want      []string
	}{
		{
			name:      "updated version",
			cargoToml: "version = \"0.4.0\"\nedition = \"2024\"\n",
			bare:      "0.4.0",
			want:      []string{},
		},
		{
			name:      "stale version",
			cargoToml: "version = \"0.3.7\"\nedition = \"2024\"\n",
			bare:      "0.4.0",
			want: []string{
				"error: workspace Cargo.toml version didn't bump to 0.4.0",
				"version = \"0.3.7\"",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := verifyBumps(test.cargoToml, test.bare)
			if strings.Join(got, "\n") != strings.Join(test.want, "\n") {
				t.Errorf("verifyBumps() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestActionsURL(t *testing.T) {
	const generic = "   the release workflow on GitHub Actions"
	tests := []struct {
		name    string
		content string
		readErr error
		want    string
	}{
		{
			name:    "github repository",
			content: "[package]\nname = \"jj-hooks\"\nrepository = \"https://github.com/mattwilkinsonn/jj-hooks\"\n",
			want:    "   https://github.com/mattwilkinsonn/jj-hooks/actions/workflows/release.yml",
		},
		{
			name:    "repository with git suffix",
			content: "repository = \"https://github.com/mattwilkinsonn/jj-gt.git\"\n",
			want:    "   https://github.com/mattwilkinsonn/jj-gt/actions/workflows/release.yml",
		},
		{name: "missing repository", content: "[package]\nname = \"x\"\n", want: generic},
		{name: "non-github repository", content: "repository = \"https://gitlab.com/x/y\"\n", want: generic},
		{name: "unreadable Cargo manifest", readErr: errors.New("missing"), want: generic},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := actionsURL(func(string) (string, error) {
				return test.content, test.readErr
			})
			if got != test.want {
				t.Errorf("actionsURL() = %q, want %q", got, test.want)
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
	goodFiles := map[string]string{
		"Cargo.toml": "version = \"0.4.0\"\nrepository = \"https://github.com/mattwilkinsonn/jj-hooks\"\n",
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
			name:    "dirty working copy",
			version: "v0.4.0",
			options: fakeOptions{
				responses: map[string]commandResult{"jj diff": {stdout: "M Cargo.toml\n"}},
				files:     goodFiles,
			},
			wantCode: 1, wantCalls: 1,
		},
		{
			name:    "not descended from main",
			version: "v0.4.0",
			options: fakeOptions{responses: map[string]commandResult{
				"jj --ignore-working-copy log": {stdout: "\n"},
			}},
			wantCode: 1, wantCalls: 2,
		},
		{
			name:    "existing tag",
			version: "v0.4.0",
			options: fakeOptions{responses: map[string]commandResult{
				"jj --ignore-working-copy log":      {stdout: "abcdef\n"},
				"jj --ignore-working-copy tag list": {stdout: "v0.3.9\nv0.4.0\n"},
			}},
			wantCode: 1, wantCalls: 3,
		},
		{
			name:    "missing cargo-edit",
			version: "v0.4.0",
			options: fakeOptions{responses: map[string]commandResult{
				"jj --ignore-working-copy log":      {stdout: "abcdef\n"},
				"cargo set-version --help":          {exitCode: 1},
				"jj --ignore-working-copy tag list": {stdout: ""},
			}},
			wantCode: 1, wantCalls: 4,
		},
		{
			name:    "bump verification failure",
			version: "v0.4.0",
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
		t.Fatalf("runOnce() = %d, want 0; stderr = %q", got, fake.stderr.String())
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
		t.Errorf("success output does not include the derived workflow URL: %q", fake.stdout.String())
	}
}

func TestRunOncePropagatesCommandExitCode(t *testing.T) {
	fake := &fakeRunner{options: fakeOptions{
		responses: map[string]commandResult{
			"jj --ignore-working-copy log":  {stdout: "abcdef123456\n"},
			"cargo set-version --workspace": {exitCode: 101},
		},
		files: map[string]string{"Cargo.toml": "version = \"0.4.0\"\n"},
	}}
	got, err := runOnce(fake.dependencies(), []string{"v0.4.0"})
	if err != nil {
		t.Fatalf("runOnce() error = %v", err)
	}
	if got != 101 {
		t.Errorf("runOnce() = %d, want 101", got)
	}
}
