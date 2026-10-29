package hybrid

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// repositoryFile reads a file relative to the repository root. These tests are about the
// delivery surface - which entry the build selects, which instance the configuration points
// at, and what a committed file is allowed to contain - so they read text instead of trusting
// documentation. They never invoke Docker and never connect to anything.
//
// The text is returned with LF line endings whatever the checkout used. This machine has
// core.autocrlf=true, so a worktree file can carry CRLF while the committed blob carries LF;
// the checks here are about what the file says, not about which convention the checkout chose.
// Without the normalisation a line-anchored pattern (`...$` under `(?m)`) silently misses the
// CR, and a `"...\n"` containment check misses it too, so the same content would be judged two
// different ways. The normalisation only removes carriage returns; no assertion is relaxed by
// it, and the byte-level corpus checks elsewhere read their inputs directly instead.
func repositoryFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", name))
	if err != nil {
		t.Fatal(err)
	}
	return normalizeLineEndings(string(data))
}

// normalizeLineEndings converts CRLF and lone CR to LF.
func normalizeLineEndings(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// integratedImageJudged is the single checker behind
// TestBuildConfigurationSelectsTheIntegratedEntry: it returns the reasons one
// Dockerfile.backend-integrated body cannot be the integrated image. It takes the body as text
// so the same rules can be run over the bytes on disk and over either line-ending convention,
// and it takes the two reference bodies it compares against rather than restating them.
func integratedImageJudged(dockerfile, moduleSeries, baselineDockerfile string) []string {
	var problems []string
	if !strings.Contains(dockerfile, "-o /out/gopherlearn-integrated ./cmd/gopherlearn-integrated") {
		problems = append(problems, "the integrated image does not build the integrated entry")
	}
	if strings.Contains(dockerfile, "./cmd/gopherlearn ") || strings.Contains(dockerfile, "./cmd/gopherlearn\n") {
		problems = append(problems, "the integrated image also builds the baseline entry")
	}
	// All three modules must reach the context: hybrid replaces GopherAI with ../v2 and the
	// root module with .. (hybrid/go.mod:94,96), so a build missing any one of them cannot
	// resolve the hybrid -> V2 -> root chain.
	for _, want := range []string{"COPY cmd ./cmd", "COPY internal ./internal", "COPY conversation ./conversation", "COPY v2 ./v2", "COPY hybrid ./hybrid"} {
		if !strings.Contains(dockerfile, want) {
			problems = append(problems, fmt.Sprintf("integrated build is missing %q: the hybrid -> V2 -> root chain needs every tree", want))
		}
	}
	// The same image-shape rules the baseline image is held to, applied to the new file.
	froms := regexp.MustCompile(`(?m)^FROM (\S+)`).FindAllStringSubmatch(dockerfile, -1)
	if len(froms) < 2 || froms[len(froms)-1][1] != "scratch" {
		problems = append(problems, fmt.Sprintf("the integrated image must be multi-stage ending in scratch, saw %d FROM lines", len(froms)))
	}
	runtime := ""
	if start := strings.LastIndex(dockerfile, "FROM scratch"); start >= 0 {
		runtime = dockerfile[start:]
	} else {
		problems = append(problems, "the integrated image has no FROM scratch stage")
	}
	for _, forbidden := range []string{".env", "spec/", "_test.go", "COPY . ", "config.integrated"} {
		if strings.Contains(runtime, forbidden) {
			problems = append(problems, fmt.Sprintf("the runtime image must not copy %q: configuration and credentials arrive at run time", forbidden))
		}
	}
	for _, required := range []string{"USER 65532:65532", "ca-certificates.crt", "CGO_ENABLED=0", "GOTOOLCHAIN=local"} {
		if !strings.Contains(dockerfile, required) {
			problems = append(problems, fmt.Sprintf("the integrated image lost %q", required))
		}
	}
	// The builder default has to satisfy the modules it compiles, read from their own
	// directives rather than restated here: a shared constant would drift.
	if moduleSeries == "" {
		problems = append(problems, "hybrid/go.mod declares no go directive")
	} else if builder := regexp.MustCompile(`(?m)^ARG GO_IMAGE=(\S+)$`).FindStringSubmatch(dockerfile); builder == nil {
		problems = append(problems, "Dockerfile.backend-integrated declares no GO_IMAGE default")
	} else if !strings.Contains(builder[1], "golang:"+moduleSeries) {
		problems = append(problems, fmt.Sprintf("the default builder %q cannot compile a module declaring go %s; nothing may be downgraded to fit it", builder[1], moduleSeries))
	}
	// The baseline image is kept, unchanged, as the historical entry.
	if !strings.Contains(baselineDockerfile, "-o /out/gopherlearn ./cmd/gopherlearn") {
		problems = append(problems, "the baseline image changed: it must keep building the historical entry")
	}
	return problems
}

// TestBuildConfigurationSelectsTheIntegratedEntry pins the point of the integration: an image
// that still built cmd/gopherlearn would ship the mainline agent loop while the documentation
// claimed the original V2 Runner was running. The rules live in integratedImageJudged so the
// same ones can be run over both line-ending conventions by
// TestBaselineLineEndingsJudgeTheIntegratedImageInEitherCheckout.
func TestBuildConfigurationSelectsTheIntegratedEntry(t *testing.T) {
	series := regexp.MustCompile(`(?m)^go (\d+\.\d+)`).FindStringSubmatch(repositoryFile(t, filepath.Join("hybrid", "go.mod")))
	if series == nil {
		t.Fatal("hybrid/go.mod declares no go directive")
	}
	problems := integratedImageJudged(repositoryFile(t, "Dockerfile.backend-integrated"), series[1], repositoryFile(t, "Dockerfile.backend"))
	if len(problems) > 0 {
		t.Fatalf("the integrated image declaration is not acceptable: %v", problems)
	}
}

// TestIntegratedStackDeclaresTheApprovedArtifacts reads the deployment files for the two image
// references the owner approved, the isolated resource names, and the credential discipline.
// Every assertion here is about a file: bringing the stack up is not verified by this round.
func TestIntegratedStackDeclaresTheApprovedArtifacts(t *testing.T) {
	stack := repositoryFile(t, "compose.integrated.yml")

	if !strings.Contains(stack, "dockerfile: Dockerfile.backend-integrated") {
		t.Fatal("the integrated stack does not build the integrated image")
	}
	if strings.Contains(stack, "dockerfile: Dockerfile.backend\n") {
		t.Fatal("the integrated stack builds the baseline image")
	}
	// Approved MySQL reference, digest-pinned. mysql:8.4 was never approved for this line and
	// the baseline 8B.1 digest belongs to a different container, so neither may appear here.
	if !strings.Contains(stack, "image: mysql@sha256:62fb722c78b24245ddff1796a0fcee4a49cc5b87e0aaaf20c92d1da9e0a2497b") {
		t.Fatal("the integrated stack does not pin the approved MySQL digest")
	}
	for _, forbiddenImage := range []string{"mysql:8.4", "mysql@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b"} {
		if strings.Contains(withoutCommentLines(stack), forbiddenImage) {
			t.Errorf("the integrated stack names an unapproved or baseline image %q", forbiddenImage)
		}
	}
	// No interpolated credential: `docker compose config` prints interpolated values.
	if bad := regexp.MustCompile(`\$\{[A-Za-z_]*(PASSWORD|SECRET|TOKEN|DSN|KEY|CREDENTIAL)`).FindString(stack); bad != "" {
		t.Errorf("the integrated stack interpolates a credential: %s", bad)
	}
	// The stack keeps its own credential file default instead of silently borrowing the
	// baseline's .env.compose.local.
	if strings.Contains(withoutCommentLines(stack), ".env.compose.local") || !strings.Contains(stack, ".env.compose.integrated.local") {
		t.Fatal("the integrated stack must default to its own env file, not the baseline's")
	}
	// Configuration mounts as a file whose suffix both ignore rules actually match.
	if !strings.Contains(stack, "source: ./config.integrated.toml.local") {
		t.Fatal("the backend does not mount the integrated configuration file")
	}
	if strings.Contains(stack, "config.integrated.local.toml") {
		t.Fatal("the old name survives: *.local matches a suffix, not a substring, so it was never ignored")
	}
	// Configuration and credentials arrive as operator-owned `.local` files; the only other host
	// paths this stack mounts are registered deployment scripts. The judge lives with the index
	// seam tests, which also mutate it.
	if problems := mountedSourcesJudged(withoutCommentLines(stack)); len(problems) > 0 {
		t.Errorf("the integrated stack mounts a host path it should not: %v", problems)
	}
	// MySQL needs more than Docker's default stop window to close InnoDB cleanly.
	window := regexp.MustCompile(`(?m)^\s*stop_grace_period: (\d+)s$`).FindAllStringSubmatch(stack, -1)
	if len(window) < 2 {
		t.Fatal("neither mysql nor the backend declares a stop window")
	}
	if seconds, err := strconv.Atoi(window[0][1]); err != nil || seconds < 30 {
		t.Errorf("mysql stop_grace_period=%v, want at least 30s", window[0][1])
	}
	// The index is a service of this file now, so its declaration is judged by
	// TestIntegratedIndexRunsAsARegisteredService in integrated_redis_seam_test.go, which also
	// mutates that judge. What stays here is the file-level shape: stop windows, and no baseline
	// resources.
	services := withoutCommentLines(stack)
	// Resource identity must not collide with the baseline stack's names. The list lives with the
	// index seam tests; this call site keeps the guard over the whole file.
	baselineStack := repositoryFile(t, "compose.app.yml")
	for _, shared := range baselineResourceNames {
		if strings.Contains(services, shared) {
			t.Fatalf("the integrated stack reuses the baseline resource %q", shared)
		}
	}
	if !strings.Contains(baselineStack, "dockerfile: Dockerfile.backend") {
		t.Fatal("the baseline stack stopped building the baseline image")
	}
}

// TestIntegratedConfigTemplateLoadsAndRefusesCrossings runs the operator-facing template
// through the same loader the entry uses, so a wrong key name is a test failure rather than a
// startup refusal in front of a user, and so the refusals the entry really implements stay
// implemented.
func TestIntegratedConfigTemplateLoadsAndRefusesCrossings(t *testing.T) {
	const name = "../config.integrated.example.toml"
	settings, err := loadEntry(name)
	if err != nil {
		t.Fatalf("the shipped template does not load: %v", err)
	}
	if !settings.v2.Entry.Isolated {
		t.Fatal("isolatedEntry.isolated must be explicitly true")
	}
	if settings.v2.Entry.Migrate {
		t.Fatal("the template must not ask for business migration")
	}
	// What the entry can actually check is that these are explicit, and this test checks that
	// the template names integrated-stack values. Whether a given schema belongs to the baseline
	// is not something a loader can know, so no wording here may claim it is refused.
	if dir := settings.v2.Config.KnowledgeConfig.StorageDir; dir == "" || dir == "data/knowledge" {
		t.Errorf("knowledgeConfig.storageDir = %q: the template must name this stack's own directory", dir)
	}
	if db := settings.v2.Config.MysqlDatabaseName; db == "" {
		t.Fatal("mysqlConfig.databaseName must be written explicitly or the entry refuses to start")
	}
	if settings.client.BaseURL == "" || settings.client.Model == "" || settings.client.APIKey == "" {
		t.Fatal("the ordinary client must be explicitly configured, not defaulted")
	}
	if settings.chat.Budget.MaxMessages <= 0 || settings.chat.SummaryPolicy.TriggerMessages <= 0 {
		t.Fatalf("ordinary Chat lost its budget or summary policy: %+v", settings.chat)
	}

	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	// A template carrying a real value would be a leak by construction.
	for _, key := range []string{"chatKey", "embeddingKey", "password", "key", "GOPHERLEARN_OPENAI_API_KEY"} {
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(trimmed, key) || strings.HasPrefix(trimmed, "#") {
				continue
			}
			value := trimmed[strings.Index(trimmed, "=")+1:]
			if !strings.Contains(value, "REPLACE_WITH_") && strings.TrimSpace(value) != `""` {
				t.Errorf("%s is not a placeholder: %s", key, trimmed)
			}
		}
	}

	// migrate = true is refused even when an operator sets it deliberately.
	switched := strings.Replace(text, "migrate = false", "migrate = true", 1)
	if switched == text {
		t.Fatal("the template no longer carries an explicit migrate = false to switch")
	}
	path := filepath.Join(t.TempDir(), "migrate.toml")
	if err := os.WriteFile(path, []byte(switched), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEntry(path); err == nil || !strings.Contains(err.Error(), "refuses business migration") {
		t.Fatalf("migrate = true was not refused with the named reason: %v", err)
	}

	// An Agent budget cannot be smuggled into the ordinary Chat section.
	path = filepath.Join(t.TempDir(), "smuggled.toml")
	if err := os.WriteFile(path, []byte(text+"\nGOPHERLEARN_AGENT_MAX_STEPS = \"3\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEntry(path); err == nil || !strings.Contains(err.Error(), "unsupported ordinaryChat setting") {
		t.Fatalf("an Agent budget in [ordinaryChat] was not refused: %v", err)
	}

	// Ordinary Chat may not fall back to the placeholder provider: an echo client answers
	// without a model, which would look like a working turn.
	echoed := strings.Replace(text, `GOPHERLEARN_AI_PROVIDER = "openai_compatible"`, `GOPHERLEARN_AI_PROVIDER = "echo"`, 1)
	if echoed == text {
		t.Fatal("the template no longer names the retained provider")
	}
	path = filepath.Join(t.TempDir(), "echo.toml")
	if err := os.WriteFile(path, []byte(echoed), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEntry(path); err == nil || !strings.Contains(err.Error(), "openai_compatible") {
		t.Fatalf("an echo ordinary client was accepted: %v", err)
	}

	// Missing a forced field is a refusal, and it happens before any resource is opened.
	dropped := strings.Replace(text, "databaseName = \"gopherlearn_v2_integration\"", "", 1)
	path = filepath.Join(t.TempDir(), "dropped.toml")
	if err := os.WriteFile(path, []byte(dropped), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEntry(path); err == nil || !strings.Contains(err.Error(), "explicit independent setting required") {
		t.Fatalf("a missing schema name was not refused: %v", err)
	}
}

// TestNoDeliveryFileStillNamesTheUnignoredConfigFile checks the name every committed file
// points at, with synthetic names only. `*.local` matches a suffix rather than a substring, so
// the earlier config.integrated.local.toml was ignored by neither rule - and no ignore pattern
// was widened to cover for that.
func TestNoDeliveryFileStillNamesTheUnignoredConfigFile(t *testing.T) {
	const obsolete = "config.integrated.local.toml"
	for _, name := range []string{"compose.integrated.yml", "README.md", "config.integrated.example.toml",
		filepath.Join("tutorial", "v2-production-entry", "README.md")} {
		if strings.Contains(repositoryFile(t, name), obsolete) {
			t.Errorf("%s still names %s, which neither ignore rule matches", name, obsolete)
		}
	}
	for _, name := range []string{".gitignore", ".dockerignore"} {
		if !hasExactIgnoreRule(repositoryFile(t, name), "*.local") {
			t.Errorf("%s no longer carries the *.local rule the credential files rely on", name)
		}
	}
	if !strings.HasSuffix("config.integrated.toml.local", ".local") {
		t.Fatal("the adopted name does not carry the ignored suffix")
	}
	// The rule is a suffix match, and that is exactly why the earlier name leaked: a path whose
	// last component ends in `.toml` is matched by neither `*.local` nor `**/*.local`. If this
	// check ever fails, the rename was unnecessary and the wording must be corrected instead.
	if strings.HasSuffix(obsolete, ".local") {
		t.Fatalf("the discarded name %q would in fact be ignored, so the rename was unnecessary", obsolete)
	}
}

func hasExactIgnoreRule(text, pattern string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == pattern {
			return true
		}
	}
	return false
}

// withoutCommentLines drops whole-line comments, so a rule about what a file *does* is not
// tripped by what it explains.
func withoutCommentLines(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
