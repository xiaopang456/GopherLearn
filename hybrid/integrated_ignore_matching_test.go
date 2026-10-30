package hybrid

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Ignore rules are read here through a matcher, not through prose: a claim that a credential
// "is ignored" is only worth something if the rule set actually decides it for the path in
// question, and the earlier failure in this line was exactly a name whose suffix no rule matched.
//
// One reading is still unsettled and is not settled by a test written here. For a pattern with no
// separator, the documented Docker table matches only at the root of the build context, while the
// pattern matcher moby ships follows .gitignore and matches at any depth. These tests assert the
// outcome under *both* readings, so nothing in the delivery depends on which one a real
// `docker build` uses. That reading was not exercised this round: no Docker was invoked.

const (
	anyLevelRootOnly   = true  // the documented table: `*.local` matches only at the context root
	anyLevelUnanchored = false // gitignore semantics: `*.local` matches at any depth
)

type ignoreRule struct {
	negate  bool
	dirOnly bool
	pattern *regexp.Regexp
	source  string
}

// compileIgnoreRules reads an ignore file the way a build or a commit decision does: whole-line
// comments and blank lines are not rules, a trailing slash makes a rule about a directory, `!`
// re-includes, and the last rule that matches a path decides.
func compileIgnoreRules(text string, rootOnlyForSeparatorless bool) []ignoreRule {
	var rules []ignoreRule
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		rules = append(rules, compileIgnoreRule(trimmed, rootOnlyForSeparatorless))
	}
	return rules
}

func compileIgnoreRule(pattern string, rootOnlyForSeparatorless bool) ignoreRule {
	rule := ignoreRule{source: pattern, dirOnly: strings.HasSuffix(pattern, "/")}
	body := strings.TrimSuffix(pattern, "/")
	if strings.HasPrefix(body, "!") {
		rule.negate = true
		body = body[1:]
	}
	leadingSlash := strings.HasPrefix(body, "/")
	if leadingSlash {
		body = body[1:]
	}
	hasSeparator := strings.Contains(body, "/")
	anchored := leadingSlash || hasSeparator || (rootOnlyForSeparatorless && !hasSeparator)

	glob := ignoreGlob(body)
	if anchored {
		glob = "^" + glob + "$"
	} else {
		glob = "^(?:.*/)?" + glob + "$"
	}
	rule.pattern = regexp.MustCompile(glob)
	return rule
}

// ignoreGlob translates the wildcard forms these files use: `**/` crosses directory boundaries,
// `/**` covers everything below, `*` and `?` stop at a separator.
func ignoreGlob(body string) string {
	var out strings.Builder
	for i := 0; i < len(body); {
		switch rest := body[i:]; {
		case strings.HasPrefix(rest, "**/"):
			out.WriteString("(?:[^/]+/)*")
			i += 3
		case strings.HasPrefix(rest, "/**"):
			out.WriteString("(?:/.*)?")
			i += 3
		case strings.HasPrefix(rest, "**"):
			out.WriteString(".*")
			i += 2
		case rest[0] == '*':
			out.WriteString("[^/]*")
			i++
		case rest[0] == '?':
			out.WriteString("[^/]")
			i++
		default:
			out.WriteString(regexp.QuoteMeta(string(rest[0])))
			i++
		}
	}
	return out.String()
}

func (r ignoreRule) matchesPath(path string) bool {
	if r.dirOnly {
		// A directory rule covers the directory and everything below it.
		components := strings.Split(path, "/")
		for i := 1; i < len(components); i++ {
			if r.pattern.MatchString(strings.Join(components[:i], "/")) {
				return true
			}
		}
		return false
	}
	return r.pattern.MatchString(path)
}

func ignoredBy(rules []ignoreRule, path string) bool {
	excluded := false
	for _, rule := range rules {
		if rule.matchesPath(path) {
			excluded = !rule.negate
		}
	}
	return excluded
}

// TestIgnoreMatcherFormsAreJudgedByTheMatcher gives the matcher its own falsifiable basis: the
// expectations below are written by hand against the documented forms, not derived from the code.
func TestIgnoreMatcherFormsAreJudgedByTheMatcher(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.local", "a.local", true},
		{"*.local", "deploy/a.local", true},
		{"*.local", "a.local.toml", false},
		{"*.local", "local", false},
		{"**/*.local", "a.local", true},
		{"**/*.local", "deploy/a.local", true},
		{"**/*.local", "deploy/keep.toml", false},
		{"/config.local", "config.local", true},
		{"/config.local", "deploy/config.local", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "web/docs/a.md", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"logs/**", "logs/a/b", true},
		{"spec/", "spec/x.go", true},
		{"spec/", "aspec/x.go", false},
		{"spec/", "web/spec/x.go", true},
		{"fi?e", "fire", true},
		{"fi?e", "f/ire", false},
		{".env.*", ".env.compose.local", true},
		{".env.*", ".env", false},
		{"coverage.out", "coverage.out", true},
		{"coverage.out", "out/coverage.out", true},
	}
	for _, tc := range cases {
		rules := compileIgnoreRules(tc.pattern, anyLevelUnanchored)
		if got := ignoredBy(rules, tc.path); got != tc.want {
			t.Errorf("pattern %q against %q: got excluded=%v, want %v (regex %s)",
				tc.pattern, tc.path, got, tc.want, rules[0].pattern.String())
		}
	}

	// Ordering and negation: the last rule that matches decides, so a re-include has to come
	// after the rule it reverses, and not the other way round.
	if got := ignoredBy(compileIgnoreRules("*.local\n!important.local", anyLevelUnanchored), "important.local"); got {
		t.Error("a later ! rule failed to re-include the file it names")
	}
	if got := ignoredBy(compileIgnoreRules("!important.local\n*.local", anyLevelUnanchored), "important.local"); !got {
		t.Error("an earlier ! rule survived the later exclusion, so the matcher is not last-match-wins")
	}

	// Comments and blanks are not rules: a file that only explains must not exclude anything.
	if got := ignoredBy(compileIgnoreRules("# *.local\n\nconfig.integrated.toml.local", anyLevelUnanchored), "config.integrated.toml.local"); !got {
		t.Error("an explicit rule was ignored, which means the reader is not reading rules")
	}
	if got := ignoredBy(compileIgnoreRules("# *.local", anyLevelUnanchored), "config.integrated.toml.local"); got {
		t.Error("a comment excluded a file")
	}
}

// TestDockerignoreAndGitignoreCoverTheOperatorFiles states the outcome the deployment depends on,
// under both readings of a separatorless pattern.
func TestDockerignoreAndGitignoreCoverTheOperatorFiles(t *testing.T) {
	dockerIgnore := repositoryFile(t, ".dockerignore")
	gitIgnore := repositoryFile(t, ".gitignore")

	// The three files this stack mounts, plus a nested one: the mount paths in
	// compose.integrated.yml and the MySQL env file default are all root-level, and a nested
	// credential is what the recursive rule in .dockerignore is there for.
	excluded := []string{
		"config.integrated.toml.local",
		"redis.integrated.conf.local",
		".env.compose.integrated.local",
		"deploy/redis.integrated.conf.local",
		"sub/dir/nested.local",
	}
	// Names that must stay in the context and in Git: the wrong name from the earlier round ends
	// in `.toml`, so no suffix rule reaches it, and the public template is documentation.
	included := []string{
		"config.integrated.local.toml",
		"deploy/config.integrated.local.toml",
		"config.integrated.example.toml",
		"redis.integrated.conf.example",
	}

	for _, reading := range []struct {
		name     string
		rootOnly bool
	}{
		{"separatorless matches at any depth", anyLevelUnanchored},
		{"separatorless matches only at the root", anyLevelRootOnly},
	} {
		dockerRules := compileIgnoreRules(dockerIgnore, reading.rootOnly)
		for _, path := range excluded {
			if !ignoredBy(dockerRules, path) {
				t.Errorf("under the reading where %q: .dockerignore does not exclude %s", reading.name, path)
			}
		}
		for _, path := range included {
			if ignoredBy(dockerRules, path) {
				t.Errorf("under the reading where %q: .dockerignore excludes %s, which is meant to stay in the context", reading.name, path)
			}
		}

		gitRules := compileIgnoreRules(gitIgnore, anyLevelUnanchored)
		for _, path := range excluded {
			if !ignoredBy(gitRules, path) {
				t.Errorf(".gitignore does not ignore %s (git ignores always match a separatorless pattern at any depth)", path)
			}
		}
		for _, path := range included {
			if ignoredBy(gitRules, path) {
				t.Errorf(".gitignore ignores %s, which the delivery reads as a committed file", path)
			}
		}
	}

	// Which rule does the work for the nested credential. Under the root-only reading it is the
	// recursive line, and removing it must break the exclusion; under the any-depth reading the
	// single-level line already covers it. Either way the file carries both, so the outcome does
	// not depend on the unsettled reading.
	nested := "deploy/redis.integrated.conf.local"
	withoutRecursive := strings.ReplaceAll(dockerIgnore, "**/*.local", "")
	if ignoredBy(compileIgnoreRules(withoutRecursive, anyLevelRootOnly), nested) {
		t.Error("the nested credential was excluded without the recursive rule under the root-only reading, so that reading was never what the rule is for")
	}
	if !ignoredBy(compileIgnoreRules(withoutRecursive, anyLevelUnanchored), nested) {
		t.Error("*.local stopped matching a nested path, which is the only claim the recursive rule is not carrying")
	}
	withoutBoth := withoutRecursive
	for _, line := range []string{"*.local", ".env.*"} {
		withoutBoth = strings.ReplaceAll(withoutBoth, line, "")
	}
	for _, path := range excluded {
		if strings.HasSuffix(path, ".local") && ignoredBy(compileIgnoreRules(withoutBoth, anyLevelUnanchored), path) {
			t.Errorf("%s is still excluded after both suffix rules were removed, so the assertion above is not the rule doing the work", path)
		}
	}

	// The rules must not reach the program this suite reads. Documentation is deliberately kept
	// out of the build context (`tutorial/` is an existing rule) while staying tracked, so the
	// two checks are separate: the context excludes learning records, Git must not.
	for _, path := range []string{
		"compose.integrated.yml", "config.integrated.example.toml",
		filepath.ToSlash(indexPreflightOnDisk), filepath.ToSlash(indexProbeOnDisk),
		"hybrid/integrated_redis_seam_test.go",
	} {
		if ignoredBy(compileIgnoreRules(dockerIgnore, anyLevelRootOnly), path) {
			t.Errorf("%s is excluded from the build context, so the stack that mounts it cannot be assembled", path)
		}
	}
	for _, path := range []string{
		"tutorial/README.md", "tutorial/v2-entry/isolated.example.toml",
		"docs/INTEGRATED-DEPLOYMENT.md", "docs/CREDENTIAL-HANDLING.md",
		"hybrid/integrated_ignore_matching_test.go",
	} {
		if ignoredBy(compileIgnoreRules(gitIgnore, anyLevelUnanchored), path) {
			t.Errorf("%s is ignored by Git, so the delivery could never commit it", path)
		}
	}
}
