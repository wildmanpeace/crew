// Package scaffold generates a project's crew setup.
//
// It exists because three of the settings in .crew/config.json fail silently
// when they are wrong. A test_file_suffix that matches nothing means no test
// is ever recognised as introduced by the branch, so no criterion ever needs a
// negative control. A verify_test_suffix the runner does not collect means the
// verifier writes tests that never execute. Build-failure markers from the
// wrong language mean a compile failure is read as a genuine assertion
// failure, and a control that proved nothing is recorded as evidence.
//
// None of those produce an error. They produce green ticks, which is why
// getting them right is worth detecting rather than documenting.
package scaffold

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wildmanpeace/crew/internal/config"
)

// Language is a project kind crew knows how to configure.
type Language string

const (
	Go         Language = "go"
	TypeScript Language = "typescript"
	CSharp     Language = "csharp"
	Dart       Language = "dart"
)

// Languages lists every language ProfileFor accepts, in a stable order.
func Languages() []Language { return []Language{Go, TypeScript, CSharp, Dart} }

// Profile is everything about a project that crew's config depends on.
type Profile struct {
	Language            Language
	CheckCommands       map[string]config.CheckCommand
	TestFileSuffix      string
	VerifyTestSuffix    string
	BuildFailureMarkers []string

	// PathsExample and CheckExample seed the TASKS.md shape in the generated
	// AGENTS.md, so the first mate proposes criteria in this project's idiom
	// rather than translating from another language's.
	PathsExample string
	CheckExample string
}

func cmd(argv []string, defaults ...string) config.CheckCommand {
	return config.CheckCommand{Argv: argv, DefaultArgs: defaults}
}

// ProfileFor returns the profile for a language, before any project-specific
// adjustment such as which test runner a package.json actually depends on.
func ProfileFor(lang Language) (Profile, error) {
	switch lang {
	case Go:
		return Profile{
			Language: Go,
			CheckCommands: map[string]config.CheckCommand{
				"test":  cmd([]string{"go", "test"}, "./..."),
				"build": cmd([]string{"go", "build"}, "./..."),
				"lint":  cmd([]string{"go", "vet"}, "./..."),
			},
			TestFileSuffix:   "_test.go",
			VerifyTestSuffix: "_crewverify_test.go",
			// Go reports a package that failed to compile rather than failing
			// an assertion, and both exit non-zero.
			BuildFailureMarkers: []string{"[build failed]", "undefined: ", "cannot find package"},
			PathsExample:        "internal/ratelimit/**",
			CheckExample:        "crew-check test ./internal/ratelimit/... -run TestSomething",
		}, nil

	case TypeScript:
		return Profile{
			Language: TypeScript,
			CheckCommands: map[string]config.CheckCommand{
				"test":  cmd([]string{"npx", "vitest", "run"}),
				"build": cmd([]string{"npx", "tsc", "--noEmit"}),
				"lint":  cmd([]string{"npx", "eslint", "."}),
			},
			TestFileSuffix:   ".test.ts",
			VerifyTestSuffix: ".crewverify.test.ts",
			BuildFailureMarkers: []string{
				"error TS2304", "Cannot find name", "Failed to resolve import",
				"does not provide an export named",
			},
			PathsExample: "src/billing/**",
			CheckExample: "crew-check test src/billing/total.crewverify.test.ts",
		}, nil

	case CSharp:
		return Profile{
			Language: CSharp,
			CheckCommands: map[string]config.CheckCommand{
				"test":  cmd([]string{"dotnet", "test"}),
				"build": cmd([]string{"dotnet", "build"}),
				"lint":  cmd([]string{"dotnet", "format", "--verify-no-changes"}),
			},
			TestFileSuffix:   "Tests.cs",
			VerifyTestSuffix: "CrewVerifyTests.cs",
			BuildFailureMarkers: []string{
				"error CS0103", "error CS0246", "error CS1061", "Build FAILED",
			},
			PathsExample: "src/Billing/**",
			CheckExample: "crew-check test --filter FullyQualifiedName~TotalTests",
		}, nil

	case Dart:
		return Profile{
			Language: Dart,
			CheckCommands: map[string]config.CheckCommand{
				"test":  cmd([]string{"dart", "test"}),
				"build": cmd([]string{"dart", "analyze"}),
				"lint":  cmd([]string{"dart", "analyze"}),
			},
			TestFileSuffix:   "_test.dart",
			VerifyTestSuffix: "_crewverify_test.dart",
			BuildFailureMarkers: []string{
				"Error: The method", "Error: Undefined name", "isn't defined",
				"Compilation failed",
			},
			PathsExample: "lib/billing/**",
			CheckExample: "crew-check test test/total_crewverify_test.dart",
		}, nil
	}
	return Profile{}, fmt.Errorf("unknown language %q; want one of %s", lang, joinLanguages())
}

func joinLanguages() string {
	var out []string
	for _, l := range Languages() {
		out = append(out, string(l))
	}
	return strings.Join(out, ", ")
}

// Detect works out a project's language from the marker files at its root.
//
// It refuses on ambiguity rather than picking. A wrong guess here produces a
// config that runs, reports green, and proves nothing — strictly worse than
// being asked which language this is.
func Detect(root string) (Profile, error) {
	var found []Language
	for _, lang := range Languages() {
		if hasMarker(root, lang) {
			found = append(found, lang)
		}
	}

	switch len(found) {
	case 0:
		return Profile{}, fmt.Errorf(
			"no go.mod, package.json, .csproj/.sln, or pubspec.yaml in %s; pass --lang <%s>",
			root, joinLanguages())
	case 1:
		p, err := ProfileFor(found[0])
		if err != nil {
			return Profile{}, err
		}
		return adjust(root, p), nil
	default:
		var names []string
		for _, l := range found {
			names = append(names, string(l))
		}
		return Profile{}, fmt.Errorf(
			"%s could be %s; pass --lang to say which crew should drive",
			root, strings.Join(names, " or "))
	}
}

func hasMarker(root string, lang Language) bool {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	switch lang {
	case Go:
		return exists("go.mod")
	case TypeScript:
		return exists("package.json")
	case Dart:
		return exists("pubspec.yaml")
	case CSharp:
		for _, glob := range []string{"*.csproj", "*.sln"} {
			if m, _ := filepath.Glob(filepath.Join(root, glob)); len(m) > 0 {
				return true
			}
		}
	}
	return false
}

// adjust refines a language's defaults using what the project actually
// declares, so a jest project is not handed a vitest command.
func adjust(root string, p Profile) Profile {
	if p.Language != TypeScript {
		return p
	}
	raw, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return p
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return p
	}
	if dependsOn("jest", pkg.Dependencies, pkg.DevDependencies) &&
		!dependsOn("vitest", pkg.Dependencies, pkg.DevDependencies) {
		p.CheckCommands["test"] = cmd([]string{"npx", "jest"})
	}
	return p
}

// dependsOn reports whether any of the dependency sets names the package.
func dependsOn(name string, sets ...map[string]string) bool {
	return slices.ContainsFunc(sets, func(m map[string]string) bool {
		_, ok := m[name]
		return ok
	})
}
