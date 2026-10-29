package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The files this round adds are deployment contract, not documentation: a credential in an
// image layer, a proxy that buffers the event stream, or a second network the client
// container can use to reach the database are all failures that no Go test would otherwise
// notice. These checks read the text of those files. They never invoke Docker, so the
// default suite still needs no daemon - and they fail on the shapes that matter rather than
// on formatting.

func repoText(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestComposeStackCarriesNoSecretsAndNoExtraServices(t *testing.T) {
	text := repoText(t, "compose.app.yml")

	// Any interpolated secret would land in `docker compose config` output, which is
	// routine reading for an operator. Names, ports and profile markers may interpolate.
	if bad := regexp.MustCompile(`\$\{[A-Za-z_]*(PASSWORD|SECRET|TOKEN|DSN|KEY|CREDENTIAL)`).FindString(text); bad != "" {
		t.Errorf("compose.app.yml interpolates a credential: %s", bad)
	}
	for _, required := range []string{
		"env_file:", "condition: service_healthy", "GOPHERLEARN_STORAGE: mysql",
		"GOPHERLEARN_AI_PROVIDER: echo", "GOPHERLEARN_LOCAL_LEARNING_MODEL: deterministic",
		"GOPHERLEARN_HTTP_ADDR: 0.0.0.0:8080", "restart: unless-stopped",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("compose.app.yml lost %q", required)
		}
	}
	if count := strings.Count(text, "env_file:"); count < 2 {
		t.Errorf("both mysql and backend must take credentials from the shared env file, got %d env_file entries", count)
	}
	for _, forbidden := range []string{"redis", "rabbitmq", "valkey", "minio", "kafka", "nats"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Errorf("this stack adds no %s service", forbidden)
		}
	}
	// Backend stays dark to the host; the client's loopback port is the only one this file
	// publishes, and no bind mount may smuggle host source or state into a container.
	if strings.Contains(text, "8080:8080") || strings.Contains(text, "3306:3306") {
		t.Error("backend or MySQL must not be published to the host by this file")
	}
	if !strings.Contains(text, `"127.0.0.1:8081:8080"`) {
		t.Error("frontend must be published on the loopback interface only")
	}
	if hostMount := regexp.MustCompile(`(?m)^\s*-\s+\.{0,2}/`).FindString(text); hostMount != "" {
		t.Errorf("no bind mounts: images are built, host source is not mounted (%q)", hostMount)
	}
	for _, name := range regexp.MustCompile(`name: (\S+)`).FindAllStringSubmatch(text, -1) {
		if !strings.HasPrefix(name[1], "gopherlearn-") {
			t.Errorf("network or volume %q is not named after this project", name[1])
		}
	}
	// The stack gets its own empty volume and its own container: 8B.1's volume keeps its data
	// and 8B.1's stopped container is never recreated by an overlay `up`.
	if !strings.Contains(text, "name: gopherlearn-8c1-mysql-data") {
		t.Error("the stack must use a fresh round-specific MySQL volume")
	}
	if strings.Contains(withoutComments(text), "gopherlearn-mysql-8b1-data") {
		t.Error("the stack must not mount the 8B.1 volume")
	}
	// Rotating .env.compose.local does not rotate the passwords MySQL already wrote into an
	// initialised data directory, so pointing a new env file at an old volume produces a
	// connection failure that looks like a broken stack. Each round gets a volume of its own.
	if strings.Contains(withoutComments(text), "name: gopherlearn-8c-mysql-data") {
		t.Error("the stack must not mount a previous round's volume")
	}
	if !strings.Contains(text, "container_name: gopherlearn-mysql-8c") {
		t.Error("the stack's MySQL container must not be the 8B.1 one")
	}
	// Measured, not theorised: with `data` a plain bridge, a frontend container that is not
	// even on it still connected to MySQL's container IP on 3306, because one Docker host
	// routes between bridges. Only an internal network has no such route.
	if !strings.Contains(text, "internal: true") {
		t.Error("the MySQL network must be internal, or any container on the host can reach port 3306 by IP")
	}
	// The client must not be attached to the network the database listens on.
	frontend := sectionOf(t, text, "  frontend:")
	if strings.Contains(frontend, "- data") {
		t.Error("frontend must not join the MySQL network")
	}
	if !strings.Contains(frontend, "- web") {
		t.Error("frontend must join the internal web network")
	}
}

func TestRuntimeImagesHoldNeitherToolchainNorSource(t *testing.T) {
	backend := repoText(t, "Dockerfile.backend")
	froms := regexp.MustCompile(`(?m)^FROM (\S+)`).FindAllStringSubmatch(backend, -1)
	if len(froms) < 2 {
		t.Fatalf("the backend image must be multi-stage, saw %d FROM lines", len(froms))
	}
	if froms[len(froms)-1][1] != "scratch" {
		t.Errorf("final backend stage must be scratch, got %q", froms[len(froms)-1][1])
	}
	goMod := repoText(t, "go.mod")
	series := regexp.MustCompile(`(?m)^go (\d+\.\d+)`).FindStringSubmatch(goMod)
	if series == nil {
		t.Fatal("go.mod declares no go directive")
	}
	// The base images are build arguments so a mirrored network can repoint them, which
	// means the canonical name lives in the ARG default rather than the FROM line. The
	// default is what an unmodified build uses, so it is the thing worth checking.
	builder := regexp.MustCompile(`(?m)^ARG GO_IMAGE=(\S+)$`).FindStringSubmatch(backend)
	if builder == nil {
		t.Fatal("Dockerfile.backend declares no GO_IMAGE default")
	}
	if !strings.Contains(builder[1], "golang:"+series[1]) {
		t.Errorf("the default builder image %q does not match the go.mod directive go %s", builder[1], series[1])
	}
	for _, required := range []string{"USER 65532:65532", "EXPOSE 8080", "ca-certificates.crt", "CGO_ENABLED=0"} {
		if !strings.Contains(backend, required) {
			t.Errorf("Dockerfile.backend lost %q", required)
		}
	}
	runtime := backend[strings.LastIndex(backend, "FROM scratch"):]
	for _, forbidden := range []string{".env", "spec/", "_test.go", "COPY . "} {
		if strings.Contains(runtime, forbidden) {
			t.Errorf("the runtime image must not copy %q", forbidden)
		}
	}

	front := repoText(t, "Dockerfile.frontend")
	for _, required := range []string{
		"ARG NODE_IMAGE=node:", "FROM ${NODE_IMAGE}", "ARG NGINX_IMAGE=nginx:", "FROM ${NGINX_IMAGE}",
	} {
		if !strings.Contains(front, required) {
			t.Errorf("Dockerfile.frontend lost %q (Node builds, Nginx serves)", required)
		}
	}
	// BuildKit only substitutes an arg in a FROM line if that arg is in global scope, so
	// declaring NGINX_IMAGE next to its own FROM made the runtime base resolve to "" and the
	// build failed with "base name (${NGINX_IMAGE}) should not be blank". An ARG sitting
	// after the first FROM therefore looks tidy and breaks the image.
	firstFrom := regexp.MustCompile(`(?m)^FROM `).FindStringIndex(front)
	for _, arg := range []string{"ARG NODE_IMAGE=", "ARG NGINX_IMAGE="} {
		at := strings.Index(front, arg)
		if firstFrom == nil || at < 0 || at > firstFrom[0] {
			t.Errorf("Dockerfile.frontend must declare %q before the first FROM", arg)
		}
	}
	_, frontRuntime, _ := strings.Cut(front, "FROM ${NGINX_IMAGE}")
	for _, forbidden := range []string{"node_modules", "web/src", ".env", "npm ci"} {
		if strings.Contains(frontRuntime, forbidden) {
			t.Errorf("the frontend runtime image must not contain %q", forbidden)
		}
	}
}

func TestProxyLeavesTheBodyLimitToTheBackendAndStreamsUnbuffered(t *testing.T) {
	nginx := repoText(t, "nginx.frontend.conf")

	// The authoritative ceiling is the HTTP layer's own, so read it from the source rather
	// than restating a number that could drift out of sync with it. It is a named constant
	// now because decodeJSON has to see one byte past it, which an inline shift literal
	// could not express.
	server := repoText(t, filepath.Join("internal", "httpapi", "server.go"))
	limit := regexp.MustCompile(`(?m)^const maxRequestBytes = (\d+)\s*<<\s*(\d+)$`).FindStringSubmatch(server)
	if limit == nil {
		t.Fatal("the request body limit is no longer a `const maxRequestBytes = N << M` in internal/httpapi")
	}
	mantissa, err1 := strconv.Atoi(limit[1])
	shift, err2 := strconv.Atoi(limit[2])
	if err1 != nil || err2 != nil {
		t.Fatalf("unparsable body limit %q", limit[0])
	}
	backendLimit := int64(mantissa) << shift
	if !strings.Contains(server, "io.LimitReader(r.Body, maxRequestBytes+1)") {
		t.Error("decodeJSON no longer reads one byte past the ceiling, so an oversized body would be truncated rather than refused")
	}

	raw := regexp.MustCompile(`client_max_body_size\s+(\d+)([mk])`).FindStringSubmatch(nginx)
	if raw == nil {
		t.Fatal("nginx.frontend.conf declares no client_max_body_size")
	}
	size, err := strconv.Atoi(raw[1])
	if err != nil {
		t.Fatal(err)
	}
	proxyLimit := int64(size)
	if raw[2] == "k" {
		proxyLimit *= 1024
	} else {
		proxyLimit *= 1024 * 1024
	}
	if proxyLimit <= backendLimit {
		t.Fatalf("the proxy would pre-empt the backend: nginx allows %d bytes, the backend %d", proxyLimit, backendLimit)
	}

	stream := blockOf(t, nginx, "location ~ ^/api/v1/sessions/[^/]+/stream$")
	for _, required := range []string{
		"proxy_buffering off", "proxy_cache off", "gzip off",
		"proxy_http_version 1.1", `proxy_set_header Connection "";`, "proxy_read_timeout",
	} {
		if !strings.Contains(stream, required) {
			t.Errorf("the SSE route must keep %q, or events arrive as one blob", required)
		}
	}
	if !strings.Contains(nginx, "try_files $uri $uri/ /index.html") {
		t.Error("the SPA needs a fallback so a client route is not a 404")
	}
	if !strings.Contains(nginx, "proxy_pass http://gopherlearn_backend") {
		t.Error("/api must reach the backend service")
	}
}

func TestDockerignoreExcludesCredentialsAndSealedRecords(t *testing.T) {
	text := repoText(t, ".dockerignore")
	lines := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	for _, required := range []string{
		".git", ".env", ".env.*", "*.local", "data/", "uploads/",
		"web/node_modules/", "web/dist/", "spec/",
		"*.exe", "*.log", "*.tmp", "*.bak", "*.orig",
	} {
		if !lines[required] {
			t.Errorf(".dockerignore no longer excludes %q", required)
		}
	}
}

// withoutComments drops whole-line comments, so a check about what a file *does* is not
// tripped up by a check about what a file *explains*.
func withoutComments(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// blockOf returns one brace-delimited nginx block, from the marker to the closing brace at the
// marker's own indentation. sectionOf cannot do this: inside a server block every directive is
// indented at least four spaces, so no following line ever looks "less indented" and the result
// swallows the next location too - a proxy_buffering off moved out of the stream block would
// still read as present.
func blockOf(t *testing.T, text, marker string) string {
	t.Helper()
	at := strings.Index(text, marker)
	if at < 0 {
		t.Fatalf("%q is missing", marker)
	}
	lineStart := strings.LastIndex(text[:at], "\n") + 1
	indent := len(text[lineStart:at]) - len(strings.TrimLeft(text[lineStart:at], " "))
	closing := "\n" + strings.Repeat(" ", indent) + "}"
	end := strings.Index(text[at:], closing)
	if end < 0 {
		t.Fatalf("%q has no closing brace at indentation %d", marker, indent)
	}
	return text[at : at+end]
}

// TestMySQLContainerIsGivenTimeToCloseItself pins the one deployment fact that 8C.1 could only
// report as a finding: MySQL needs more than Docker's 10-second default to flush and close, and
// under that default it exited 137 - killed, not shut down - with every row still intact. The
// data was never at risk; the exit status was, because 137 is what an operator reads as a crash.
// The rest of the assertions exist because the fix must stay a timeout: the image digest, the
// loopback port, the charset, the healthcheck, the volume and the container name are 8B.1's and
// the standalone start must keep meaning what it meant then.
func TestMySQLContainerIsGivenTimeToCloseItself(t *testing.T) {
	standalone := repoText(t, "compose.mysql.yml")
	service := sectionOf(t, standalone, "\n  mysql:")

	window, ok := stopGraceSeconds(service)
	if !ok {
		t.Fatal("compose.mysql.yml gives the mysql service no stop_grace_period")
	}
	if window < 30 {
		t.Errorf("stop_grace_period=%ds, want at least 30s so InnoDB can close without SIGKILL", window)
	}
	for _, untouched := range []string{
		"image: mysql@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b",
		`"127.0.0.1:3308:3306"`,
		"--character-set-server=utf8mb4",
		"--collation-server=utf8mb4_0900_ai_ci",
		"mysqladmin ping",
		"container_name: gopherlearn-mysql-8b1",
	} {
		if !strings.Contains(service, untouched) {
			t.Errorf("the mysql service changed something other than its stop window: %q", untouched)
		}
	}
	if !strings.Contains(standalone, "name: gopherlearn-mysql-8b1-data") {
		t.Error("8B.1's volume name must not move")
	}

	// The overlay is allowed to repeat the window but never to shorten it: a merged run is the
	// same database and needs the same time to close.
	app := repoText(t, "compose.app.yml")
	if overlaid := sectionOf(t, app, "\n  mysql:"); strings.Contains(overlaid, "stop_grace_period") {
		merged, ok := stopGraceSeconds(overlaid)
		if !ok || merged < window {
			t.Errorf("compose.app.yml shortens the mysql stop window to %d (the standalone file sets %d)", merged, window)
		}
	}
}

// stopGraceSeconds reads a compose duration of the form `30s` or `1m`.
func stopGraceSeconds(text string) (int, bool) {
	match := regexp.MustCompile(`(?m)^\s*stop_grace_period: (\d+)(s|m|h)$`).FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	switch match[2] {
	case "m":
		return value * 60, true
	case "h":
		return value * 3600, true
	default:
		return value, true
	}
}

// sectionOf returns the text from a marker to the next top-level-ish construct, which is
// enough to scope an assertion to one block of these small files.
func sectionOf(t *testing.T, text, marker string) string {
	t.Helper()
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("%q is missing", marker)
	}
	rest := text[start:]
	end := regexp.MustCompile("\n(?://|#|\\S|  \\S)").FindStringIndex(rest[1:])
	if end == nil {
		return rest
	}
	return rest[:end[0]+1]
}
