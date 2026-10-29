package hybrid

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These tests exist because the delivery checks read text from the worktree, and this machine
// checks the repository out with core.autocrlf=true: Dockerfile.backend-integrated is CRLF on
// disk while the committed blob is LF. Before the read boundary normalised line endings, a
// line-anchored pattern (`(?m)^ARG GO_IMAGE=(\S+)$`) could not match a CRLF line, so the check
// reported that the file declares no builder default while the file plainly declares one.
//
// The pair below pins both halves of the repair: the same content is judged identically in
// either convention, and the checks are still checks - a body without the field, a body that
// only mentions the field in a comment, and a body naming a builder the module cannot compile
// are each still rejected by name.

// rawRepositoryFile returns the bytes of a repository file exactly as the checkout wrote them,
// bypassing the normalising reader. It is only used to show what the normalisation is for.
func rawRepositoryFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func crlf(text string) string { return strings.ReplaceAll(text, "\n", "\r\n") }

// TestBaselineLineEndingsJudgeTheIntegratedImageInEitherCheckout runs the real delivery rules
// over the real Dockerfile in both conventions. The judged text is normalised first, which is
// exactly what repositoryFile now does at the read boundary.
func TestBaselineLineEndingsJudgeTheIntegratedImageInEitherCheckout(t *testing.T) {
	raw := rawRepositoryFile(t, "Dockerfile.backend-integrated")
	lf := normalizeLineEndings(raw)
	baseline := repositoryFile(t, "Dockerfile.backend")
	series := regexp.MustCompile(`(?m)^go (\d+\.\d+)`).FindStringSubmatch(repositoryFile(t, filepath.Join("hybrid", "go.mod")))
	if series == nil {
		t.Fatal("hybrid/go.mod declares no go directive")
	}

	if strings.Contains(lf, "\r") {
		t.Fatal("the normalised body still carries a carriage return")
	}
	for _, variant := range []struct {
		name string
		body string
	}{
		{"the bytes as checked out", raw},
		{"the same content with LF endings", lf},
		{"the same content with CRLF endings", crlf(lf)},
	} {
		// The judge is reached through the normalising reader in production, so the variants are
		// normalised here the same way. Judging the raw CRLF bytes is the reproduction below.
		if problems := integratedImageJudged(normalizeLineEndings(variant.body), series[1], baseline); len(problems) > 0 {
			t.Errorf("%s is not accepted: %v", variant.name, problems)
		}
	}

	// The reader itself, not just the judge, has to hand over LF: every consumer of
	// repositoryFile is a content check, and one of them is a line-anchored pattern.
	if got := repositoryFile(t, "Dockerfile.backend-integrated"); strings.Contains(got, "\r") {
		t.Error("repositoryFile still returns carriage returns, so a line-anchored check would still miss them")
	}

	// The reported defect, reproduced: judged without normalisation the CRLF body cannot match
	// the line-anchored builder pattern and is rejected for a field it does contain. If this
	// ever stops reproducing, the normalisation is no longer the thing making the checks pass
	// and this file must be re-derived rather than kept as decoration.
	unfixed := integratedImageJudged(crlf(lf), series[1], baseline)
	if !containsProblem(unfixed, "declares no GO_IMAGE default") {
		t.Fatalf("the CRLF body was judged acceptable without normalisation (%v), so this file no longer reproduces the reported defect", unfixed)
	}
}

// TestBaselineLineEndingsStillRejectWhatIsMissingOrDowngraded keeps the repair from becoming a
// blanket acceptance: each mutation is applied to the real body in both conventions and must be
// reported by name in both.
func TestBaselineLineEndingsStillRejectWhatIsMissingOrDowngraded(t *testing.T) {
	lf := normalizeLineEndings(rawRepositoryFile(t, "Dockerfile.backend-integrated"))
	baseline := repositoryFile(t, "Dockerfile.backend")
	series := regexp.MustCompile(`(?m)^go (\d+\.\d+)`).FindStringSubmatch(repositoryFile(t, filepath.Join("hybrid", "go.mod")))
	if series == nil {
		t.Fatal("hybrid/go.mod declares no go directive")
	}
	builderLine := regexp.MustCompile(`(?m)^ARG GO_IMAGE=(\S+)$`).FindStringSubmatch(lf)
	if builderLine == nil {
		t.Fatal("the delivered Dockerfile no longer declares a GO_IMAGE default")
	}

	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"the builder default deleted",
			func(s string) string {
				return strings.Replace(s, "ARG GO_IMAGE="+builderLine[1]+"\n", "", 1)
			},
			"declares no GO_IMAGE default"},
		{"the builder default only mentioned in a comment",
			func(s string) string {
				return strings.Replace(s, "ARG GO_IMAGE="+builderLine[1], "# ARG GO_IMAGE="+builderLine[1], 1)
			},
			"declares no GO_IMAGE default"},
		{"a builder older than the module's go directive",
			func(s string) string {
				return strings.Replace(s, "ARG GO_IMAGE="+builderLine[1], "ARG GO_IMAGE=golang:1.20-bookworm", 1)
			},
			"nothing may be downgraded to fit it"},
		{"the baseline entry built as well",
			func(s string) string {
				return s + "RUN go build -o /out/gopherlearn ./cmd/gopherlearn\n"
			},
			"also builds the baseline entry"},
	}
	for _, tc := range cases {
		mutated := tc.mutate(lf)
		if mutated == lf {
			t.Fatalf("%s: the mutation changed nothing, so it proves nothing", tc.name)
		}
		for _, variant := range []struct {
			convention string
			body       string
		}{{"LF", mutated}, {"CRLF", crlf(mutated)}} {
			// A CRLF body is normalised at the read boundary before it is judged, which is the
			// production path; judging the raw bytes is covered by the reproduction above.
			problems := integratedImageJudged(normalizeLineEndings(variant.body), series[1], baseline)
			if !containsProblem(problems, tc.want) {
				t.Errorf("%s (%s): not reported (%v)", tc.name, variant.convention, problems)
			}
		}
	}

	// The normalisation removes a false negative as well as a false positive: the baseline-entry
	// rule is a containment check for "./cmd/gopherlearn" followed by a newline, which a CRLF
	// line would have walked past before the read boundary normalised it.
	smuggled := crlf(lf + "RUN go build -o /out/gopherlearn ./cmd/gopherlearn\n")
	if problems := integratedImageJudged(normalizeLineEndings(smuggled), series[1], baseline); !containsProblem(problems, "also builds the baseline entry") {
		t.Errorf("a CRLF body that builds the baseline entry was accepted: %v", problems)
	}

	// A missing baseline image is still a failure, in either convention.
	if problems := integratedImageJudged(lf, series[1], ""); !containsProblem(problems, "the baseline image changed") {
		t.Errorf("an empty baseline Dockerfile was accepted: %v", problems)
	}
	if problems := integratedImageJudged(lf, "", baseline); !containsProblem(problems, "declares no go directive") {
		t.Errorf("a missing module go directive was accepted: %v", problems)
	}
}
