package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectByMarkerFile(t *testing.T) {
	for _, tc := range []struct {
		name, marker, body string
		want               Language
	}{
		{"go", "go.mod", "module example.com/x\n", Go},
		{"typescript", "package.json", `{"devDependencies":{"vitest":"^2"}}`, TypeScript},
		{"csharp project", "Api.csproj", "<Project/>", CSharp},
		{"csharp solution", "Api.sln", "", CSharp},
		{"dart", "pubspec.yaml", "name: x\n", Dart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.marker, tc.body)

			got, err := Detect(dir)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got.Language != tc.want {
				t.Errorf("Language = %q, want %q", got.Language, tc.want)
			}
		})
	}
}

// The suffixes and markers are the settings that fail silently when wrong: a
// bad test_file_suffix means no test is ever recognised as new, and bad
// markers mean a compile failure reads as a real assertion failure. Detection
// exists mostly to get these right.
func TestDetectFillsTheSilentlyWrongSettings(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"devDependencies":{"vitest":"^2"}}`)

	got, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.TestFileSuffix != ".test.ts" {
		t.Errorf("TestFileSuffix = %q", got.TestFileSuffix)
	}
	// A verifier's tests must still be collected by the runner, or they are
	// written and never executed.
	if !strings.HasSuffix(got.VerifyTestSuffix, got.TestFileSuffix) {
		t.Errorf("VerifyTestSuffix %q is not collected by a runner matching %q",
			got.VerifyTestSuffix, got.TestFileSuffix)
	}
	if len(got.BuildFailureMarkers) == 0 {
		t.Error("no build-failure markers; a compile failure would count as evidence")
	}
}

// Every language's verify suffix has to satisfy the same rule, since a
// verifier that writes uncollected tests looks exactly like one that found
// nothing wrong.
func TestEveryProfileWritesTestsTheRunnerCollects(t *testing.T) {
	for _, lang := range Languages() {
		p, err := ProfileFor(lang)
		if err != nil {
			t.Fatalf("ProfileFor(%q): %v", lang, err)
		}
		if !strings.HasSuffix(p.VerifyTestSuffix, p.TestFileSuffix) {
			t.Errorf("%s: VerifyTestSuffix %q is not collected by %q",
				lang, p.VerifyTestSuffix, p.TestFileSuffix)
		}
		for _, verb := range []string{"test", "build", "lint"} {
			if len(p.CheckCommands[verb].Argv) == 0 {
				t.Errorf("%s: no argv for %q", lang, verb)
			}
		}
	}
}

func TestDetectReadsTheTestRunnerFromPackageJSON(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"devDependencies":{"jest":"^29"}}`)

	got, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if argv := strings.Join(got.CheckCommands["test"].Argv, " "); !strings.Contains(argv, "jest") {
		t.Errorf("test argv = %q, want jest", argv)
	}
}

// Guessing wrong is worse than not guessing: a half-right config fails
// silently, so an ambiguous or bare directory has to ask.
func TestDetectRefusesRatherThanGuess(t *testing.T) {
	t.Run("nothing recognised", func(t *testing.T) {
		if _, err := Detect(t.TempDir()); err == nil {
			t.Fatal("Detect succeeded on an empty directory")
		} else if !strings.Contains(err.Error(), "--lang") {
			t.Errorf("error %q does not say how to proceed", err)
		}
	})

	t.Run("two markers", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "go.mod", "module example.com/x\n")
		write(t, dir, "package.json", "{}")

		_, err := Detect(dir)
		if err == nil {
			t.Fatal("Detect picked one of two languages")
		}
		for _, want := range []string{"go", "typescript", "--lang"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}
