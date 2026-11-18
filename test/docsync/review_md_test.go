package docsync

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

const (
	securityRule = "Always report security faults in the diff, including secrets, keys, tokens and injection."
	manualRule   = "Always review changes to REVIEW.md by hand."
)

var reviewHeadings = []string{
	"## What MAJOR means here",
	"## What not to report",
	"## Triggers (rules a diff can show)",
	"## Pruning",
}

var htmlBlockStart = regexp.MustCompile(`^ {0,3}</?[A-Za-z][A-Za-z0-9-]*([ \t/>]|$)`)

func checkReviewMD(root fs.FS) error {
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		return err
	}
	regular := false
	for _, e := range entries {
		if e.Name() == "REVIEW.md" {
			regular = e.Type().IsRegular()
		} else if strings.EqualFold(e.Name(), "REVIEW.md") {
			return fmt.Errorf("expected a regular file named exactly REVIEW.md: case collision with %q", e.Name())
		}
	}
	if !regular {
		return fmt.Errorf("expected a regular file named exactly REVIEW.md")
	}
	data, err := fs.ReadFile(root, "REVIEW.md")
	if err != nil {
		return err
	}

	lines := make(map[string]bool)
	var fence string
	var comment, rawHTML, dated bool
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimRight(line, " \t\r")
		left := strings.TrimLeft(line, " ")
		if fence == "" && !comment {
			if left == "" {
				rawHTML = false
			} else if htmlBlockStart.MatchString(line) {
				rawHTML = true
			}
		}
		if !comment && !rawHTML && len(line)-len(left) <= 3 && len(left) >= 3 && (left[0] == '`' || left[0] == '~') {
			mark := left[:1]
			run := len(left) - len(strings.TrimLeft(left, mark))
			if fence != "" && strings.HasPrefix(left, fence) && run == len(left) {
				fence = ""
				continue
			}
			if fence == "" && run >= 3 && (mark == "~" || !strings.Contains(left[run:], mark)) {
				fence = left[:run]
				continue
			}
		}
		if fence != "" {
			continue
		}
		// Comments can cross blank lines, including inside a raw HTML block.
		if comment || strings.Contains(line, "<!--") {
			for rest := line; ; {
				delimiter := "<!--"
				if comment {
					delimiter = "-->"
				}
				var found bool
				_, rest, found = strings.Cut(rest, delimiter)
				if !found {
					break
				}
				comment = !comment
			}
			continue
		}
		if rawHTML {
			continue
		}
		lines[line] = true
		if date, ok := strings.CutPrefix(line, "Prune by: "); ok {
			_, err := time.Parse("2006-01-02", date)
			dated = dated || err == nil
		}
	}
	for _, want := range slices.Concat(reviewHeadings, []string{securityRule, manualRule}) {
		if !lines[want] {
			return fmt.Errorf("REVIEW.md missing %q", want)
		}
	}
	if !dated {
		return fmt.Errorf("REVIEW.md missing a valid Prune by: YYYY-MM-DD line")
	}
	return nil
}

func TestReviewMD(t *testing.T) {
	const date = "Prune by: 2027-02-17"
	root := repoRoot()
	if bazeltest.IsBazel() {
		// Bazel projects even regular source files as symlinks. Require the
		// declared input, then inspect its source directory for case and type.
		if _, err := os.Stat(filepath.Join(root, "REVIEW.md")); err != nil {
			t.Fatal(err)
		}
		anchor, err := filepath.EvalSymlinks(filepath.Join(root, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		root = filepath.Dir(anchor)
	}
	full := strings.Join(reviewHeadings, "\n") + "\n" + securityRule + "\n" + manualRule + "\n" + date
	file := func(name, body string, mode fs.FileMode) fs.FS {
		return fstest.MapFS{name: &fstest.MapFile{Data: []byte(body), Mode: mode}}
	}
	const name = "REVIEW.md"
	type example struct {
		label string
		root  fs.FS
		want  string
	}
	cases := []example{
		{"complete", file(name, full, 0), ""},
		{"absent", fstest.MapFS{}, "regular file"},
		{"wrong case", file("review.md", full, 0), "regular file"},
		{"case collision", fstest.MapFS{
			name:        &fstest.MapFile{Data: []byte(full)},
			"Review.md": &fstest.MapFile{},
		}, "case collision"},
		{"symlink", file(name, full, fs.ModeSymlink), "regular file"},
		{"invalid date", file(name, strings.Replace(full, date, "Prune by: 2027-02-31", 1), 0), "valid Prune by"},
		{"past date", file(name, strings.Replace(full, date, "Prune by: 2001-01-01", 1), 0), ""},
		{"short fence closer", file(name, strings.Replace(full, "## Pruning", "````md\n```\n## Pruning\n````", 1), 0), "## Pruning"},
		{"tilde fence", file(name, strings.Replace(full, manualRule, "   ~~~\n"+manualRule+"\n   ~~~", 1), 0), manualRule},
		{"HTML comment", file(name, strings.Replace(full, securityRule, "<!--\n"+securityRule+"\n-->", 1), 0), securityRule},
		{"unclosed comment", file(name, strings.Replace(full, "## Pruning", "<!--\n## Pruning", 1), 0), "## Pruning"},
		{"comment in raw HTML", file(name, "<div>\n```\n<!--\n```\n</div>\n\n"+full, 0), "## What MAJOR means here"},
		{"real file", os.DirFS(root), ""},
	}
	for _, required := range slices.Concat(reviewHeadings, []string{securityRule, manualRule, date}) {
		body := strings.Replace(full, required, "", 1)
		cases = append(cases, example{"missing " + required, file(name, body, 0), strings.Split(required, ":")[0]})
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			err := checkReviewMD(tc.root)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
