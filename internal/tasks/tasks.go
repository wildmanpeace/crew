// Package tasks parses TASKS.md, which is hand-authored intent only and
// never carries status. Status lives exclusively in .crew/state.json.
//
// The file is markdown with "## task: <id>" headings; each body is a YAML
// sequence of single-key mappings, matching the bullet style the spec uses so
// the file stays comfortable to edit by hand.
package tasks

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Criterion is one acceptance criterion: either mechanically checkable via a
// command, or explicitly delegated to judgment. Never both, never neither.
type Criterion struct {
	Check       string
	Judged      bool
	Description string
}

// IsMechanical reports whether this criterion carries a concrete check command.
func (c Criterion) IsMechanical() bool { return c.Check != "" }

// Task is one unit of work as declared in TASKS.md.
type Task struct {
	ID                 string
	DependsOn          []string
	Paths              []string
	Brief              string
	AcceptanceCriteria []Criterion
}

const taskHeading = "## task:"

// ParseFile reads and parses a TASKS.md at the given path.
func ParseFile(p string) ([]Task, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open TASKS.md: %w", err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads TASKS.md content and returns the declared tasks in file order.
func Parse(r io.Reader) ([]Task, error) {
	sections, err := splitSections(r)
	if err != nil {
		return nil, err
	}
	var out []Task
	seen := make(map[string]bool, len(sections))
	for _, s := range sections {
		if seen[s.id] {
			return nil, fmt.Errorf("task %q: declared more than once", s.id)
		}
		seen[s.id] = true
		t, err := parseSection(s)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := validateDeps(out, seen); err != nil {
		return nil, err
	}
	return out, nil
}

type section struct {
	id   string
	body string
}

func splitSections(r io.Reader) ([]section, error) {
	var (
		out       []section
		cur       *section
		body      strings.Builder
		fenceChar byte
		fenceLen  int
	)
	flush := func() {
		if cur != nil {
			cur.body = body.String()
			out = append(out, *cur)
			body.Reset()
		}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()

		if fenceChar != 0 {
			// Inside a fenced code block: nothing is a heading, including a
			// worked example's "## task:" line, until the fence closes.
			if ch, n, ok := fenceMarker(line); ok && ch == fenceChar && n >= fenceLen {
				fenceChar, fenceLen = 0, 0
			}
			if cur != nil {
				body.WriteString(line)
				body.WriteByte('\n')
			}
			continue
		}

		if ch, n, ok := fenceMarker(line); ok {
			fenceChar, fenceLen = ch, n
			if cur != nil {
				body.WriteString(line)
				body.WriteByte('\n')
			}
			continue
		}

		if strings.HasPrefix(strings.TrimSpace(line), taskHeading) {
			flush()
			id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), taskHeading))
			if id == "" {
				return nil, fmt.Errorf("task heading with no id: %q", line)
			}
			cur = &section{id: id}
			continue
		}
		if cur != nil {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read TASKS.md: %w", err)
	}
	flush()
	return out, nil
}

// fenceMarker reports whether line opens or closes a fenced code block, per
// CommonMark: a run of three or more identical backticks or tildes, indented
// at most three spaces. A closing fence must consist of the run and nothing
// but trailing whitespace; a backtick fence's opening line cannot itself
// contain a backtick (that ambiguity is left to the writer, not this
// heuristic).
func fenceMarker(line string) (ch byte, n int, ok bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || trimmed == "" {
		return 0, 0, false
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0, false
	}
	i := 0
	for i < len(trimmed) && trimmed[i] == c {
		i++
	}
	if i < 3 {
		return 0, 0, false
	}
	rest := strings.TrimSpace(trimmed[i:])
	if c == '`' && strings.ContainsRune(rest, '`') {
		// Backtick fences cannot carry a backtick in their info string.
		return 0, 0, false
	}
	return c, i, true
}

// rawCriterion mirrors the YAML shape so a missing key is distinguishable
// from a zero value.
type rawCriterion struct {
	Check       *string `yaml:"check"`
	Judged      *bool   `yaml:"judged"`
	Description *string `yaml:"description"`
}

func parseSection(s section) (Task, error) {
	// The body is a YAML sequence of single-key mappings; merge them into one
	// mapping so field order in the file does not matter.
	var entries []map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(s.body), &entries); err != nil {
		return Task{}, fmt.Errorf("task %q: %w", s.id, err)
	}
	fields := make(map[string]yaml.Node)
	for _, e := range entries {
		for k, v := range e {
			if _, dup := fields[k]; dup {
				return Task{}, fmt.Errorf("task %q: duplicate field %q", s.id, k)
			}
			fields[k] = v
		}
	}

	t := Task{ID: s.id}
	for k := range fields {
		switch k {
		case "depends_on", "paths", "brief", "acceptance_criteria":
		case "status":
			return Task{}, fmt.Errorf("task %q: TASKS.md is intent only and must not carry %q; status lives in .crew/state.json", s.id, k)
		default:
			return Task{}, fmt.Errorf("task %q: unknown field %q", s.id, k)
		}
	}

	if n, ok := fields["brief"]; ok {
		if err := n.Decode(&t.Brief); err != nil {
			return Task{}, fmt.Errorf("task %q: brief: %w", s.id, err)
		}
		t.Brief = strings.TrimSpace(t.Brief)
	}
	if t.Brief == "" {
		return Task{}, fmt.Errorf("task %q: brief is required", s.id)
	}

	if n, ok := fields["depends_on"]; ok {
		var raw string
		if err := n.Decode(&raw); err != nil {
			return Task{}, fmt.Errorf("task %q: depends_on: %w", s.id, err)
		}
		t.DependsOn = splitList(raw, true)
	}
	if n, ok := fields["paths"]; ok {
		var raw string
		if err := n.Decode(&raw); err != nil {
			return Task{}, fmt.Errorf("task %q: paths: %w", s.id, err)
		}
		t.Paths = splitList(raw, false)
	}

	n, ok := fields["acceptance_criteria"]
	if !ok {
		return Task{}, fmt.Errorf("task %q: acceptance_criteria is required", s.id)
	}
	var raws []rawCriterion
	if err := n.Decode(&raws); err != nil {
		return Task{}, fmt.Errorf("task %q: acceptance_criteria: %w", s.id, err)
	}
	if len(raws) == 0 {
		return Task{}, fmt.Errorf("task %q: acceptance_criteria must not be empty", s.id)
	}
	for i, rc := range raws {
		c, err := buildCriterion(s.id, i, rc)
		if err != nil {
			return Task{}, err
		}
		t.AcceptanceCriteria = append(t.AcceptanceCriteria, c)
	}
	return t, nil
}

func buildCriterion(taskID string, i int, rc rawCriterion) (Criterion, error) {
	hasCheck := rc.Check != nil && strings.TrimSpace(*rc.Check) != ""
	hasJudged := rc.Judged != nil && *rc.Judged
	switch {
	case hasCheck && hasJudged:
		return Criterion{}, fmt.Errorf("task %q criterion %d: cannot be both check and judged", taskID, i)
	case !hasCheck && !hasJudged:
		return Criterion{}, fmt.Errorf("task %q criterion %d: must be tagged with either a check command or judged: true", taskID, i)
	}
	if rc.Description == nil || strings.TrimSpace(*rc.Description) == "" {
		return Criterion{}, fmt.Errorf("task %q criterion %d: description is required", taskID, i)
	}
	c := Criterion{Judged: hasJudged, Description: strings.TrimSpace(*rc.Description)}
	if hasCheck {
		c.Check = strings.TrimSpace(*rc.Check)
	}
	return c, nil
}

func validateDeps(ts []Task, known map[string]bool) error {
	for _, t := range ts {
		for _, d := range t.DependsOn {
			if d == t.ID {
				return fmt.Errorf("task %q: depends on itself", t.ID)
			}
			if !known[d] {
				return fmt.Errorf("task %q: depends_on references undefined task %q", t.ID, d)
			}
		}
	}
	return nil
}

// splitList splits a comma-separated field. When allowNone is set, the
// sentinel "none" yields an empty list.
func splitList(raw string, allowNone bool) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || (allowNone && strings.EqualFold(raw, "none")) {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ByID indexes tasks by their id.
func ByID(ts []Task) map[string]Task {
	m := make(map[string]Task, len(ts))
	for _, t := range ts {
		m[t.ID] = t
	}
	return m
}

// Overlaps reports whether two paths: declarations could touch the same
// files. It is deliberately conservative: a pattern that might overlap counts
// as overlapping, because the caller refuses the spawn rather than risking two
// workers editing the same tree.
func Overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if patternsOverlap(x, y) {
				return true
			}
		}
	}
	return false
}

func patternsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	return segmentsOverlap(globSegments(a), globSegments(b))
}

// globSegments splits a path pattern on "/". A trailing bare "*" is promoted
// to "**": a task author writing "src/*" means "everything under src", and
// the conservative contract (refuse on possible overlap) means we should
// read it that way rather than silently narrowing it to direct children only.
func globSegments(pattern string) []string {
	segs := strings.Split(pattern, "/")
	if n := len(segs); n > 0 && segs[n-1] == "*" {
		segs[n-1] = "**"
	}
	return segs
}

// segmentsOverlap reports whether two "/"-delimited segment sequences could
// describe overlapping sets of paths. "**" matches zero or more whole
// segments, crossing what path.Match alone would treat as directory
// boundaries; every other segment is compared with segmentsCompatible. Like
// Overlaps, it is conservative: an alignment it cannot rule out counts as an
// overlap.
func segmentsOverlap(a, b []string) bool {
	memo := make(map[[2]int]bool, len(a)*len(b))
	var walk func(i, j int) bool
	walk = func(i, j int) bool {
		if i == len(a) && j == len(b) {
			return true
		}
		key := [2]int{i, j}
		if v, ok := memo[key]; ok {
			return v
		}
		result := false
		switch {
		case i < len(a) && a[i] == "**":
			result = walk(i+1, j) || (j < len(b) && walk(i, j+1))
		case j < len(b) && b[j] == "**":
			result = walk(i, j+1) || (i < len(a) && walk(i+1, j))
		case i == len(a) || j == len(b):
			result = false
		default:
			result = segmentsCompatible(a[i], b[j]) && walk(i+1, j+1)
		}
		memo[key] = result
		return result
	}
	return walk(0, 0)
}

// segmentsCompatible reports whether two single, already-split path segments
// (never "**") could describe the same path component. A bare "*" is
// universal. When only one side carries glob syntax, path.Match settles it
// exactly. When both sides carry glob syntax we cannot resolve symbolically,
// we assume they could collide rather than risk a false "no overlap".
func segmentsCompatible(x, y string) bool {
	if x == y || x == "*" || y == "*" {
		return true
	}
	xWild := strings.ContainsAny(x, "*?[")
	yWild := strings.ContainsAny(y, "*?[")
	switch {
	case xWild && !yWild:
		ok, err := path.Match(x, y)
		return err == nil && ok
	case yWild && !xWild:
		ok, err := path.Match(y, x)
		return err == nil && ok
	case xWild && yWild:
		return true
	default:
		return false
	}
}
