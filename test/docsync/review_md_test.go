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

var reviewSentences = []string{
	securityRule,
	manualRule,
	"A line the diff does not add or change is not a finding of the diff, unless the diff changes what that line does or who it reaches.",
	"A line the diff adds or changes is judged by what the change itself newly shows and to whom, even when the same value already appears somewhere else on the base branch.",
	"A value that the same line already held on the base branch, still shown to the same audience, is reported once as already on the base and is never graded above NOTE for this diff.",
	"A change that removes, weakens or hides a rule of this file is MAJOR, and the finding says which rule and what later reviews lose.",
	"A PR that removes a rule under Pruning, and shows that no review cited it and no diff triggered it since the last pruning date, is not a finding.",
	"When the diff and the repository do not show which route a request takes, the finding is not PRECONDITION.",
	"A PRECONDITION names what it needs by file and line.",
}

var reviewHeadings = []string{
	"## What MAJOR means here",
	"## What not to report",
	"## Triggers (rules a diff can show)",
	"## Pruning",
}

var (
	reviewBackticks = regexp.MustCompile("`+")
	reviewBlankLine = regexp.MustCompile(`\n[ \t\r]*\n`)
)

// Count prose around unambiguous inline examples. If delimiters could belong
// to different blocks or table cells, this paragraph cannot prove a rule.
func stripReviewCodeSpans(paragraph string) string {
	const omitted = "\x00"
	runs := reviewBackticks.FindAllStringIndex(paragraph, -1)
	if len(runs)%2 != 0 {
		return omitted
	}
	var prose strings.Builder
	start := 0
	for i := 0; i < len(runs); i += 2 {
		opener, closer := runs[i], runs[i+1]
		if opener[1]-opener[0] != closer[1]-closer[0] || strings.HasSuffix(paragraph[:opener[0]], "\\") ||
			strings.ContainsAny(paragraph[opener[1]:closer[0]], "\n|") {
			return omitted
		}
		prose.WriteString(paragraph[start:opener[0]])
		prose.WriteString(omitted)
		start = closer[1]
	}
	return prose.String() + paragraph[start:]
}

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

	seen := make(map[string]bool)
	dated := false
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case strings.Contains(line, "<"):
			return fmt.Errorf("REVIEW.md contains an angle bracket (<)")
		case strings.Contains(line, "```"):
			return fmt.Errorf("REVIEW.md contains a backtick fence")
		case strings.Contains(line, "~"):
			return fmt.Errorf("REVIEW.md contains a tilde (strikethrough or fence)")
		case strings.Contains(line, "["):
			return fmt.Errorf("REVIEW.md contains a bracket (link, link reference, footnote or image)")
		case strings.HasPrefix(line, "    ") || strings.HasPrefix(strings.TrimLeft(line, " "), "\t"):
			return fmt.Errorf("REVIEW.md contains an indented code block")
		}

		if strings.HasPrefix(line, "## ") {
			seen[strings.Join(strings.Fields(line), " ")] = true
		}
		if date, ok := strings.CutPrefix(strings.TrimRight(line, " \t\r"), "Prune by: "); ok {
			_, err := time.Parse("2006-01-02", date)
			dated = dated || err == nil
		}
	}
	for _, heading := range reviewHeadings {
		if !seen[heading] {
			return fmt.Errorf("REVIEW.md missing heading %q on its own line without closing hashes", heading)
		}
	}
	paragraphs := reviewBlankLine.Split(string(data), -1)
	for i, paragraph := range paragraphs {
		paragraphs[i] = stripReviewCodeSpans(paragraph)
	}
	prose := strings.Join(strings.Fields(strings.Join(paragraphs, "\n\n")), " ")
	for _, sentence := range reviewSentences {
		if !strings.Contains(prose, sentence) {
			return fmt.Errorf("REVIEW.md missing %q in prose outside a code span", sentence)
		}
	}
	if !dated {
		return fmt.Errorf("REVIEW.md missing a valid Prune by: YYYY-MM-DD line without emphasis or trailing remarks")
	}
	return nil
}

func TestReviewMD(t *testing.T) {
	const date = "Prune by: 2027-02-17"
	const citation = "A PRECONDITION names what it needs by file and line."
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
	full := strings.Join(slices.Concat(reviewHeadings, reviewSentences, []string{date}), "\n")
	file := func(name, body string, mode fs.FileMode) fs.FS {
		return fstest.MapFS{name: &fstest.MapFile{Data: []byte(body), Mode: mode}}
	}
	const name = "REVIEW.md"
	t.Run("ambiguous code span boundaries", func(t *testing.T) {
		for _, paragraph := range []string{"| sample `one | two` |", "### sample `one\ntwo`"} {
			if prose := stripReviewCodeSpans(paragraph); strings.Contains(prose, "sample") {
				t.Errorf("ambiguous paragraph counted as prose: %q", paragraph)
			}
		}
	})
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
		{"real file", os.DirFS(root), ""},
	}
	for _, edit := range []struct{ label, old, new, want string }{
		{"processing instruction", "", "<?x?>\n", "angle bracket"},
		{"CDATA", "", "<![CDATA[]]>\n", "angle bracket"},
		{"declaration", "", "<!X>\n", "angle bracket"},
		{"link reference", "", "   [reference]: elsewhere\n", "link reference"},
		{"preformatted", "", "<pre>\n", "angle bracket"},
		{"caption", "", "<em>Example</em>\n", "angle bracket"},
		{"embedded frame", "", "<iframe>\n", "angle bracket"},
		{"folded text", "", "Notes <details>\n", "angle bracket"},
		{"backtick fence", "", "  ```md\n", "fence"},
		{"tilde fence", "", "   ~~~~\n", "fence"},
		{"image", "", "See ![figure](figure.png)\n", "image"},
		{"double-quoted link title", "", "[label](#section \"caption\")\n", "link"},
		{"single-quoted link title", "", "[label](#section 'caption')\n", "link"},
		{"parenthesized link title", "", "[label](#section (caption))\n", "link"},
		{"empty-text link title", "", "[](#section \"caption\")\n", "link"},
		{"two-line link title", "", "[label](#section\n  \"caption\")\n", "link"},
		{"multiple empty-text titles", "", "[](#a \"first\")\n[](#b \"second\")\n", "link"},
		{"quoted footnote", "", "> [^1]: note\n", "footnote"},
		{"quoted comment-like reference", "", "> [//]: # (comment)\n", "reference"},
		{"list comment-like reference", "", "- [//]: # (comment)\n", "reference"},
		{"two-line reference label", "", "[two\nlines]: #section\n", "reference"},
		{"strikethrough", "", "~~example~~\n", "strikethrough"},
		{"code span", citation, "`" + citation + "`", citation},
		{"multiline double-backtick span", citation, "``example `\n" + citation + "\n` example``", "code span"},
		{"indented code block", "", "    example\n", "indent"},
		{"tab-indented code block", "", "  \texample\n", "indent"},
		{"quoted fence", "", "> ```\n> example\n> ```\n", "fence"},
		{"table cell", citation, "| Rule |\n| --- |\n| " + citation + " |", ""},
		{"heading", citation, "### " + citation + " ###", ""},
		{"trailing entity", citation, citation + "&#8203;", ""},
		{"reflow", "and injection.", "and\r\n  injection.", ""},
		{"list", manualRule, "- " + manualRule, ""},
		{"bold", manualRule, "**" + manualRule + "**", ""},
		{"second sentence", manualRule, manualRule + " Use the base.", ""},
		{"blockquote", manualRule, "> " + manualRule, ""},
		{"placeholder", "", "<file>:<line>\n", "angle bracket"},
		{"inline example", "", "The marker is `<!--`.\n", "angle bracket"},
		{"empty comment", "", "<!-->\n", "angle bracket"},
		{"annotated date", date, date + " (extended)", "Prune by:"},
		{"bold date label", date, "**Prune by:** 2027-02-17", "Prune by:"},
		{"closing hashes", "## Pruning", "## Pruning ##", "## Pruning"},
	} {
		body := strings.Replace(full, edit.old, edit.new, 1)
		cases = append(cases, example{edit.label, file(name, body, 0), edit.want})
	}
	for _, required := range slices.Concat(reviewHeadings, reviewSentences, []string{date}) {
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
