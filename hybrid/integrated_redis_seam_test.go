package hybrid

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The index is a service of the integrated stack, so these tests judge the deployment seam
// itself: what this image's entrypoint really reads, what the two checked-in scripts decide when
// they are executed, and whether the compose structure says what the documentation claims.
// Nothing here contacts a server or calls Docker - the scripts run against substitute commands
// and a synthetic credential, and the compose file is read as the structure it declares.

const (
	indexServiceName     = "redis"
	indexConfTarget      = "/redis-stack.conf"
	indexPreflightPath   = "/redis-stack-preflight.sh"
	indexProbePath       = "/redis-stack-healthcheck.sh"
	indexPreflightOnDisk = "deploy/redis-stack-preflight.sh"
	indexProbeOnDisk     = "deploy/redis-stack-healthcheck.sh"
	indexImageDigest     = "redis/redis-stack-server@sha256:39adcbb083b240642cd5738a75c4959c0393ef2a40fa545f2e1c7b55798eeda4"
	syntheticSecret      = "synthetic-redis-credential-0123456789abcdef"
)

// baselineResourceNames are owned by the baseline stack and its data. Hand-copied from
// compose.app.yml and from the recorded baseline Redis file: deriving them from a value the
// delivery test reads would make this check vacuous.
var baselineResourceNames = []string{
	"gopherlearn-mysql-8c",
	"gopherlearn-8c1-mysql-data",
	"gopherlearn-app-data",
	"gopherlearn-app-web",
	"gopherlearn-redis-11a-data",
	`"127.0.0.1:8081:8080"`,
	`"127.0.0.1:6380:6379"`,
}

// composeText returns a file with whole-line comments removed. A rule about what a file *does*
// must not be satisfied by what it explains, and must not be tripped by a name that only appears
// in prose.
func composeText(t *testing.T, name string) string {
	t.Helper()
	var kept []string
	for _, line := range strings.Split(repositoryFile(t, name), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, strings.TrimRight(line, "\r"))
	}
	return strings.Join(kept, "\n")
}

// composeBlock returns the indented body of one top-level mapping, following the file's own
// indentation. It is a structure reader, not a text search: a directive belonging to another
// service must not satisfy an assertion about this one.
func composeBlock(t *testing.T, text, marker string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == marker {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%q is missing", marker)
	}
	var body []string
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			break
		}
		body = append(body, line)
	}
	if len(body) == 0 {
		t.Fatalf("%q has no body", marker)
	}
	return strings.Join(body, "\n")
}

// serviceBlockOf extracts one service's mapping from the services body.
func serviceBlockOf(t *testing.T, services, name string) string {
	t.Helper()
	lines := strings.Split(services, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == "  "+name+":" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("service %q is not declared", name)
	}
	var body []string
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "    ") {
			break
		}
		body = append(body, line)
	}
	if len(body) == 0 {
		t.Fatalf("service %q has no body", name)
	}
	return strings.Join(body, "\n")
}

// mountFor returns the mount entry whose target is the given path, tolerating the long form
// (`target: X`) and the short form (`- ./host:X`).
func mountFor(target, service string) string {
	if strings.Contains(service, "target: "+target) {
		for _, piece := range strings.Split(service, "- type:") {
			if strings.Contains(piece, "target: "+target) {
				return "- type:" + piece
			}
		}
	}
	for _, line := range strings.Split(service, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") && strings.HasSuffix(trimmed, ":"+target) {
			return line
		}
	}
	return ""
}

// indexServiceJudged is the single checker behind the positive case and behind every negative
// case: it returns the reasons an index declaration is not acceptable for this stack.
func indexServiceJudged(service string) []string {
	var problems []string
	if !strings.Contains(service, "image: "+indexImageDigest) {
		problems = append(problems, "the index is not pinned to the accepted digest")
	}
	if regexp.MustCompile(`image: [^\s@]*:\S`).MatchString(service) {
		problems = append(problems, "a digest-pinned image must not also carry a tag")
	}
	for _, notSearch := range []string{"image: redis@sha256", "image: redis:", "image: valkey"} {
		if strings.Contains(service, notSearch) {
			problems = append(problems, notSearch+" is not a Stack image, so it cannot answer FT._LIST")
		}
	}
	if !strings.Contains(service, `entrypoint: ["/bin/sh", "`+indexPreflightPath+`"]`) {
		problems = append(problems, "the container does not start through the preflight wrapper")
	}
	// The credential may only reach the server through its own configuration file. argv is
	// visible in `ps` and in `docker inspect`; REDIS_ARGS is the image's own argument channel,
	// which the sealed entry evidence shows is appended to the server command verbatim.
	for _, channel := range []string{"--requirepass", "REDIS_ARGS"} {
		if strings.Contains(service, channel) {
			problems = append(problems, "the credential seam uses "+channel+", which is visible in the process arguments")
		}
	}
	if strings.Contains(strings.ToLower(service), "requirepass ") {
		problems = append(problems, "the compose file carries a credential value instead of mounting the operator's configuration")
	}
	// The Compose key for "a missing bind source must not be created" is
	// `bind.create_host_path` - `create_host_directory` is not a Compose key at all, and
	// Docker Compose v5.1.3 refuses to parse a file that carries it ("additional properties
	// 'create_host_directory' not allowed"). The declaration therefore carries the correct
	// key, and the misspelling is rejected here so it cannot come back.
	if strings.Contains(service, "create_host_directory") {
		problems = append(problems, "the declaration carries bind.create_host_directory, which is not a Compose key; the key is create_host_path")
	}
	conf := mountFor(indexConfTarget, service)
	if conf == "" {
		problems = append(problems, "no configuration is mounted at "+indexConfTarget+", the only path this image's entrypoint reads")
	} else {
		if !strings.Contains(conf, "read_only: true") {
			problems = append(problems, "the server configuration is mounted writable")
		}
		if !strings.Contains(conf, "create_host_path: false") {
			problems = append(problems, "the configuration mount does not forbid creating the missing source path, so an absent configuration would be created instead of refused")
		}
	}
	for _, script := range []string{indexPreflightPath, indexProbePath} {
		if mountFor(script, service) == "" {
			problems = append(problems, "the checked-in script "+script+" is not mounted")
		}
	}
	if !strings.Contains(service, `test: ["CMD", "/bin/sh", "`+indexProbePath+`"]`) {
		problems = append(problems, "the healthcheck does not run the strict probe script")
	}
	if !strings.Contains(service, "REDIS_DATA_DIR: /data") || mountFor("/data", service) == "" {
		problems = append(problems, "the data directory and its mount must agree")
	}
	if regexp.MustCompile(`(?m)^\s*ports:`).MatchString(service) {
		problems = append(problems, "the index must not publish a port while the network it joins is internal")
	}
	if !strings.Contains(service, "container_name: gopherlearn-integrated-redis") {
		problems = append(problems, "the index has no container name of its own")
	}
	return problems
}

// integratedStackJudged covers what only the whole file can show: the stack borrows nothing from
// the baseline and the index keeps storage and a private network of its own.
func integratedStackJudged(stack string) []string {
	var problems []string
	for _, baseline := range baselineResourceNames {
		if strings.Contains(stack, baseline) {
			problems = append(problems, "the integrated stack reuses the baseline resource "+baseline)
		}
	}
	if !strings.Contains(stack, "gopherlearn-integrated-redis-data:\n    name: gopherlearn-integrated-redis-data") {
		problems = append(problems, "the index data volume is not named and declared by this stack")
	}
	if !strings.Contains(stack, "name: gopherlearn-integrated-data") {
		problems = append(problems, "the network the index joins has no name of its own")
	}
	if !strings.Contains(stack, "internal: true") {
		problems = append(problems, "the network the index joins is reachable from the host")
	}
	return problems
}

// indexProbeJudged reads the probe script itself: the health gate has to ask the search module,
// and must not move the credential into argv. Whether the reply is read *structurally* is not
// checked here - it is checked by executing the script below.
func indexProbeJudged(script string) []string {
	var problems []string
	if !strings.Contains(script, "FT._LIST") {
		problems = append(problems, "the probe does not ask the search module, so a listening port would read as healthy")
	}
	if !strings.Contains(script, "REDISCLI_AUTH=") {
		problems = append(problems, "the probe does not use the client's non-argv credential channel")
	}
	if regexp.MustCompile(`\$CLI.*(?:-a|--pass)\b`).MatchString(script) {
		problems = append(problems, "the probe passes the credential as an argument, which is visible in ps")
	}
	if !strings.Contains(script, "2>&1") {
		problems = append(problems, "the probe discards the error replies it is supposed to classify")
	}
	return problems
}

// indexWiringJudged is the part no compose file can show on its own: the application's own
// configuration has to name the container this stack starts, at the database the accepted batches
// ran on.
func indexWiringJudged(host string, db int, containerName string) []string {
	var problems []string
	if host != containerName {
		problems = append(problems, fmt.Sprintf("redisConfig.host = %q, want the index container %q: pointing elsewhere silently leaves the stack without search", host, containerName))
	}
	if db != 0 {
		problems = append(problems, fmt.Sprintf("redisConfig.db = %d, want 0: isolation comes from this instance, and the accepted batches ran on db 0", db))
	}
	return problems
}

func containsProblem(problems []string, needle string) bool {
	for _, problem := range problems {
		if strings.Contains(problem, needle) {
			return true
		}
	}
	return false
}

// TestIntegratedIndexRunsAsARegisteredService reads the actual service structure of the stack
// file: declared, gated by the preflight, health-gated by the strict probe, private to this
// stack, and a startup dependency of the backend.
func TestIntegratedIndexRunsAsARegisteredService(t *testing.T) {
	stack := composeText(t, "compose.integrated.yml")
	services := composeBlock(t, stack, "services:")
	service := serviceBlockOf(t, services, indexServiceName)

	if problems := indexServiceJudged(service); len(problems) > 0 {
		t.Fatalf("the integrated index declaration is not acceptable: %v", problems)
	}
	if problems := integratedStackJudged(stack); len(problems) > 0 {
		t.Fatalf("the integrated stack is not acceptable: %v", problems)
	}
	if problems := indexProbeJudged(repositoryFile(t, indexProbeOnDisk)); len(problems) > 0 {
		t.Fatalf("the probe script is not acceptable: %v", problems)
	}

	// The mount that matters is the one the image actually reads, sourced from the operator file.
	conf := mountFor(indexConfTarget, service)
	if !strings.Contains(conf, "source: ./redis.integrated.conf.local") {
		t.Errorf("the configuration mount does not come from the operator's ignored local file: %s", conf)
	}

	// Where the "a missing configuration file refuses the start" guarantee now lives. It is not
	// in the compose file any more (see indexServiceJudged): the container's entrypoint is the
	// checked-in preflight, and that script refuses a missing configuration before it hands
	// over to the image entrypoint. This is the file-level statement of that link; the behaviour
	// itself is executed in TestPreflightRefusesBeforeTheServerStarts.
	preflight := repositoryFile(t, indexPreflightOnDisk)
	for _, want := range []string{`[ -f "$CONF" ] || refuse "configuration missing"`, `exec "$ENTRY"`} {
		if !strings.Contains(preflight, want) {
			t.Errorf("the preflight does not carry %q, so the container is not the thing that refuses a missing configuration", want)
		}
	}

	// Backend ordering, and its own single configuration path.
	backend := serviceBlockOf(t, services, "backend")
	if !strings.Contains(backend, "      redis:\n        condition: service_healthy") {
		t.Error("the backend does not wait for the index to be healthy")
	}
	if !strings.Contains(backend, `command: ["-config", "/config/integrated.toml"]`) {
		t.Error("the backend does not name its single configuration file")
	}

	// One startup story: one compose file, one documented command, and no leftover service file
	// that has to be brought up first.
	if _, err := os.Stat(filepath.Join("..", "deploy", "compose.integrated.redis.yml")); !os.IsNotExist(err) {
		t.Errorf("the second startup file still exists, so two startup stories remain possible: %v", err)
	}
	if commands := regexp.MustCompile(`(?m)^#\s+docker compose .*$`).
		FindAllString(repositoryFile(t, "compose.integrated.yml"), -1); len(commands) != 1 {
		t.Errorf("the stack file should document exactly one startup command, saw %d: %v", len(commands), commands)
	}

	// The wiring the owner asked to be caught rather than assumed.
	settings, err := loadEntry("../config.integrated.example.toml")
	if err != nil {
		t.Fatalf("the shipped template does not load: %v", err)
	}
	container := regexp.MustCompile(`(?m)^\s*container_name: (gopherlearn-integrated-redis)$`).FindStringSubmatch(service)
	if container == nil {
		t.Fatal("the index declares no container name for the configuration to point at")
	}
	if problems := indexWiringJudged(settings.v2.Config.RedisHost, settings.v2.Config.RedisDb, container[1]); len(problems) > 0 {
		t.Errorf("the configuration does not reach this index: %v", problems)
	}

	// Isolation comes from a separate server, not from renamed indexes: the production names are
	// read back from the source that uses them instead of being restated in prose.
	for _, want := range []struct{ file, constant, name string }{
		{filepath.Join("v2", "common", "redis", "key.go"), "KnowledgeChunkIndexName", "gopherai_knowledge_chunks_v3_idx"},
		{filepath.Join("v2", "common", "redis", "episode_index.go"), "EpisodeIndexName", "gopherai_episode_v2_idx"},
		{filepath.Join("v2", "common", "redis", "memory_index.go"), "MemoryIndexName", "gopherai_memory_v2_idx"},
	} {
		got := regexp.MustCompile(`(?m)^\s*` + want.constant + `\s*=\s*"([^"]*)"`).FindStringSubmatch(repositoryFile(t, want.file))
		if got == nil || got[1] != want.name {
			t.Errorf("%s in %s = %v, want %q: this stack does not rename the production indexes", want.constant, want.file, got, want.name)
		}
	}
}

// TestIndexDeclarationChecksRejectTheWrongShapes proves the judges above are judges: every
// mutation is a realistic wrong declaration and each must be reported by name.
func TestIndexDeclarationChecksRejectTheWrongShapes(t *testing.T) {
	base := strings.Join([]string{
		`    image: ` + indexImageDigest,
		`    container_name: gopherlearn-integrated-redis`,
		`    entrypoint: ["/bin/sh", "` + indexPreflightPath + `"]`,
		`    volumes:`,
		`      - type: bind`,
		`        source: ./redis.integrated.conf.local`,
		`        target: ` + indexConfTarget,
		`        read_only: true`,
		`        bind:`,
		`          create_host_path: false`,
		`      - type: bind`,
		`        source: ./deploy/redis-stack-preflight.sh`,
		`        target: ` + indexPreflightPath,
		`        read_only: true`,
		`      - type: bind`,
		`        source: ./deploy/redis-stack-healthcheck.sh`,
		`        target: ` + indexProbePath,
		`        read_only: true`,
		`      - type: volume`,
		`        source: gopherlearn-integrated-redis-data`,
		`        target: /data`,
		`    environment:`,
		`      REDIS_DATA_DIR: /data`,
		`    healthcheck:`,
		`      test: ["CMD", "/bin/sh", "` + indexProbePath + `"]`,
	}, "\n")

	if problems := indexServiceJudged(base); len(problems) > 0 {
		t.Fatalf("the reference declaration this list is built from is itself unacceptable: %v", problems)
	}

	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"configuration mounted where the entrypoint does not read it",
			func(s string) string {
				return strings.ReplaceAll(s, indexConfTarget, "/usr/local/etc/redis/redis.conf")
			},
			"only path this image's entrypoint reads"},
		{"credential passed as a server argument",
			func(s string) string { return s + "\n      command: [\"--requirepass\", \"hunter2\"]" },
			"visible in the process arguments"},
		{"credential smuggled through the image's own argument variable",
			func(s string) string {
				return strings.Replace(s, "      REDIS_DATA_DIR: /data", "      REDIS_DATA_DIR: /data\n      REDIS_ARGS: \"--requirepass hunter2\"", 1)
			},
			"REDIS_ARGS"},
		{"credential written into the compose file",
			func(s string) string { return s + "\n      requirepass hunter2-0123456789abcdef" },
			"instead of mounting"},
		{"healthcheck that reads any reply as healthy",
			func(s string) string {
				return strings.Replace(s, `      test: ["CMD", "/bin/sh", "`+indexProbePath+`"]`,
					`      test: ["CMD", "redis-cli", "PING"]`, 1)
			},
			"strict probe script"},
		{"no preflight, so an unconfigured server starts",
			func(s string) string {
				return strings.Replace(s, `    entrypoint: ["/bin/sh", "`+indexPreflightPath+`"]`, "", 1)
			},
			"preflight wrapper"},
		{"writable configuration mount",
			func(s string) string {
				return strings.Replace(s, "        target: /redis-stack.conf\n        read_only: true", "        target: /redis-stack.conf", 1)
			},
			"mounted writable"},
		{"the missing source path would be created instead of refused",
			func(s string) string {
				return strings.Replace(s, "        bind:\n          create_host_path: false\n", "", 1)
			},
			"does not forbid creating the missing source path"},
		{"the misspelled bind key comes back",
			func(s string) string {
				return strings.Replace(s, "          create_host_path: false\n", "          create_host_directory: false\n", 1)
			},
			"create_host_directory"},
		{"index published to the host",
			func(s string) string { return s + "\n    ports:\n      - \"127.0.0.1:6390:6379\"" },
			"publish a port"},
		{"unaccepted image reference with a tag",
			func(s string) string {
				return strings.Replace(s, "image: "+indexImageDigest, "image: redis/redis-stack-server:latest", 1)
			},
			"accepted digest"},
		{"plain redis, which has no search module",
			func(s string) string { return strings.Replace(s, "image: "+indexImageDigest, "image: redis:7", 1) },
			"not a Stack image"},
		{"no script mounted, so the preflight cannot run",
			func(s string) string {
				return strings.Replace(s, `      - type: bind
        source: ./deploy/redis-stack-preflight.sh
        target: /redis-stack-preflight.sh
        read_only: true
`, "", 1)
			},
			"is not mounted"},
		{"data directory and mount disagree",
			func(s string) string {
				return strings.Replace(s, "      REDIS_DATA_DIR: /data", "      REDIS_DATA_DIR: /var/lib/redis", 1)
			},
			"must agree"},
		{"index without its own container name",
			func(s string) string {
				return strings.Replace(s, "    container_name: gopherlearn-integrated-redis\n", "", 1)
			},
			"container name of its own"},
	}

	for _, tc := range cases {
		mutated := tc.mutate(base)
		if mutated == base {
			t.Fatalf("%s: the mutation changed nothing, so it proves nothing", tc.name)
		}
		if problems := indexServiceJudged(mutated); !containsProblem(problems, tc.want) {
			t.Errorf("%s: not reported (%v)", tc.name, problems)
		}
	}

	// Stack-level shapes, on the real file with one thing broken at a time.
	stack := composeText(t, "compose.integrated.yml")
	for _, tc := range []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"borrowing the baseline index volume",
			func(s string) string {
				return strings.Replace(s, "      source: gopherlearn-integrated-redis-data", "      source: gopherlearn-redis-11a-data", 1)
			}, "baseline resource"},
		{"putting the index on a network the host can reach",
			func(s string) string { return strings.Replace(s, "    internal: true", "", 1) }, "reachable from the host"},
		{"an index volume this stack does not declare",
			func(s string) string {
				return strings.Replace(s, "gopherlearn-integrated-redis-data:\n    name: gopherlearn-integrated-redis-data", "", 1)
			}, "index data volume"},
	} {
		mutated := tc.mutate(stack)
		if mutated == stack {
			t.Fatalf("%s: the mutation changed nothing, so it proves nothing", tc.name)
		}
		if problems := integratedStackJudged(mutated); !containsProblem(problems, tc.want) {
			t.Errorf("%s: not reported (%v)", tc.name, problems)
		}
	}

	// The probe script's own gates, each removed one at a time.
	probe := repositoryFile(t, indexProbeOnDisk)
	if problems := indexProbeJudged(probe); len(problems) > 0 {
		t.Fatalf("the delivered probe script is unacceptable: %v", problems)
	}
	for _, tc := range []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"asks the server to ping instead of asking the module",
			func(s string) string { return strings.ReplaceAll(s, "FT._LIST", "PING") }, "search module"},
		{"moves the credential into argv",
			func(s string) string {
				return strings.Replace(s, `REDISCLI_AUTH="$secret" "$CLI" -2 --json`, `"$CLI" -a "$secret" -2 --json`, 1)
			}, "visible in ps"},
		{"drops the non-argv channel entirely",
			func(s string) string { return strings.ReplaceAll(s, `REDISCLI_AUTH="$secret" `, "") }, "credential channel"},
		{"reads only stdout, so an error reply is invisible",
			func(s string) string { return strings.Replace(s, " 2>&1)", ")", 1) }, "error replies"},
	} {
		mutated := tc.mutate(probe)
		if mutated == probe {
			t.Fatalf("%s: the mutation changed nothing, so it proves nothing", tc.name)
		}
		if problems := indexProbeJudged(mutated); !containsProblem(problems, tc.want) {
			t.Errorf("%s: not reported (%v)", tc.name, problems)
		}
	}

	// A configuration pointing somewhere else must not read as wired up.
	container := regexp.MustCompile(`(?m)^\s*container_name: (\S+)$`).
		FindStringSubmatch(serviceBlockOf(t, stack, indexServiceName))[1]
	if problems := indexWiringJudged("gopherlearn-redis-11a", 0, container); !containsProblem(problems, "pointing elsewhere") {
		t.Errorf("a wrong host was accepted (%v)", problems)
	}
	if problems := indexWiringJudged(container, 2, container); !containsProblem(problems, "want 0") {
		t.Errorf("a non-zero database was accepted as a usable search deployment (%v)", problems)
	}
}

// deploymentScriptsMounted are the checked-in plumbing this stack mounts into the index
// container. They are the only host paths allowed besides the operator's own `.local` files, so
// the registry is deliberately two entries long and each must exist.
var deploymentScriptsMounted = map[string]bool{
	"./deploy/redis-stack-preflight.sh":   true,
	"./deploy/redis-stack-healthcheck.sh": true,
}

// mountedSourcesJudged reads the bind sources the stack actually declares.
func mountedSourcesJudged(stack string) []string {
	return mountSourcesJudgedWith(deploymentScriptsMounted, stack)
}

// mountSourcesJudged rejects anything mounted besides the operator's `.local` files and the
// registered scripts: a host source tree mounted into the runtime container would make the built
// image a lie, and a file tracked in Git mounted as configuration would put a credential where a
// template belongs.
func mountSourcesJudgedWith(registered map[string]bool, stack string) []string {
	var problems []string
	for _, mount := range regexp.MustCompile(`(?m)^\s*source: (\.\/\S+)`).FindAllStringSubmatch(stack, -1) {
		source := mount[1]
		if strings.HasSuffix(source, ".local") {
			continue
		}
		if !registered[source] {
			problems = append(problems, source+" is neither an ignored credential-suffixed file nor a registered deployment script")
			continue
		}
		if info, err := os.Stat(filepath.Join("..", strings.TrimPrefix(source, "./"))); err != nil || info.IsDir() {
			problems = append(problems, "the registered deployment script "+source+" is not a file in this repository")
		}
	}
	return problems
}

// TestMountedSourcesRejectAHostTree keeps the mount rule from becoming a formality: the real
// stack passes, and each of three wrong declarations is reported.
func TestMountedSourcesRejectAHostTree(t *testing.T) {
	stack := composeText(t, "compose.integrated.yml")
	if problems := mountedSourcesJudged(stack); len(problems) > 0 {
		t.Fatalf("the delivered stack is unacceptable: %v", problems)
	}
	for _, tc := range []struct {
		name       string
		registered map[string]bool
		mutate     func(string) string
		want       string
	}{
		{"the host source tree mounted into the runtime container", deploymentScriptsMounted,
			func(s string) string {
				return s + "\n      - type: bind\n        source: ./hybrid\n        target: /src\n"
			}, "./hybrid"},
		{"a configuration file that is tracked in Git, not the operator's own", deploymentScriptsMounted,
			func(s string) string {
				return strings.ReplaceAll(s, "./config.integrated.toml.local", "./config.integrated.example.toml")
			},
			"credential-suffixed file"},
		// A registry that vouches for a script the repository does not contain: the existence
		// check is the only thing standing between a typo and a container that cannot start.
		{"a registered script that is not in the repository",
			map[string]bool{"./deploy/redis-stack-preflight.sh": true, "./deploy/redis-stack-healthcheck.sh": true, "./deploy/does-not-exist.sh": true},
			func(s string) string {
				return strings.ReplaceAll(s, "./deploy/redis-stack-preflight.sh", "./deploy/does-not-exist.sh")
			},
			"not a file in this repository"},
	} {
		mutated := tc.mutate(stack)
		if mutated == stack {
			t.Fatalf("%s: the mutation changed nothing, so it proves nothing", tc.name)
		}
		if problems := mountSourcesJudgedWith(tc.registered, mutated); !containsProblem(problems, tc.want) {
			t.Errorf("%s: not reported (%v)", tc.name, problems)
		}
	}
}

// ---- executing the two delivered scripts against substitute commands ----

// indexFixture is scratch state: a substitute redis-cli and a substitute image entrypoint, a
// configuration file, and the canned reply the substitute prints. Both substitutes record how they
// were invoked, so the leak boundary is measured instead of asserted from text.
type indexFixture struct {
	dir       string
	client    string
	entry     string
	conf      string
	reply     string
	argvLog   string
	envLog    string
	reached   string
	entryArgs string
}

func newIndexFixture(t *testing.T) *indexFixture {
	t.Helper()
	dir := t.TempDir()
	fixture := &indexFixture{
		dir:       dir,
		client:    filepath.Join(dir, "bin", "redis-cli"),
		entry:     filepath.Join(dir, "bin", "entrypoint.sh"),
		conf:      filepath.Join(dir, "redis-stack.conf"),
		reply:     filepath.Join(dir, "reply.txt"),
		argvLog:   filepath.Join(dir, "argv.txt"),
		envLog:    filepath.Join(dir, "env.txt"),
		reached:   filepath.Join(dir, "reached.txt"),
		entryArgs: filepath.Join(dir, "entry-args.txt"),
	}
	// The substitute prints the canned reply, exits with FAKE_RC, and records argv plus its own
	// environment. It is not a client: no socket is opened.
	client := strings.NewReplacer("@ARGV@", posixForSh(fixture.argvLog),
		"@ENV@", posixForSh(fixture.envLog),
		"@REPLY@", posixForSh(fixture.reply)).Replace(`#!/bin/sh
printf 'ARGV:%s\n' "$*" > "@ARGV@"
/bin/env > "@ENV@"
cat "@REPLY@"
exit ${FAKE_RC:-0}
`)
	if err := os.MkdirAll(filepath.Dir(fixture.client), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.client, []byte(client), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := strings.NewReplacer("@REACHED@", posixForSh(fixture.reached), "@ARGS@", posixForSh(fixture.entryArgs)).
		Replace(`#!/bin/sh
printf 'ENTRY:%s\n' "$*" > "@ARGS@"
printf 'reached\n' >> "@REACHED@"
exit 0
`)
	if err := os.WriteFile(fixture.entry, []byte(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fixture.argvLog, fixture.envLog, fixture.reply, fixture.reached, fixture.entryArgs} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

// writeConf replaces the configuration file under test with exactly these bytes.
func (f *indexFixture) writeConf(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(f.conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setReply replaces the canned server reply the substitute client prints.
func (f *indexFixture) setReply(t *testing.T, reply string) {
	t.Helper()
	if err := os.WriteFile(f.reply, []byte(reply+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *indexFixture) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// reachedCount is how many times the substitute entrypoint ran: a refusal must leave it at zero,
// which is the only way to show the check happens before Redis starts.
func (f *indexFixture) reachedCount(t *testing.T) int {
	t.Helper()
	text := strings.TrimSpace(f.read(t, f.reached))
	if text == "" {
		return 0
	}
	return len(strings.Split(text, "\n"))
}

// sh locates a POSIX shell. These judges execute the delivered scripts, so a machine without one
// is a named failure rather than a silent pass: skipping here would let the deployment seam go
// unexecuted while the suite still reads green.
func sh(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"sh", "bash"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Fatalf("no POSIX shell found on PATH: the two scripts under deploy/ are executed by these tests, so this machine cannot verify them")
	return ""
}

// posixForSh renders a path the way the MSYS shell on this machine expects, because the value is
// handed over as an environment variable and used by the shell rather than by Go.
func posixForSh(path string) string {
	slashed := filepath.ToSlash(path)
	if matched := regexp.MustCompile(`^([A-Za-z]):/`).FindStringSubmatch(slashed); matched != nil {
		return "/" + strings.ToLower(matched[1]) + slashed[2:]
	}
	return slashed
}

// runIndexScript executes one of the delivered scripts with its seams pointed at the substitutes.
// PATH and the rest of the environment are inherited so awk, sed, tr and wc are found; a missing
// one shows up as a refusal, which these tests would report rather than hide.
func runIndexScript(t *testing.T, script string, env map[string]string) (int, string) {
	t.Helper()
	command := exec.Command(sh(t), posixForSh(script))
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	command.Dir = root
	inherited := os.Environ()
	command.Env = make([]string, len(inherited))
	copy(command.Env, inherited)
	for _, key := range []string{"REDIS_STACK_CONF", "REDIS_CLI", "REDIS_STACK_ENTRYPOINT", "FAKE_RC"} {
		if value, ok := env[key]; ok {
			overridden := false
			for i, entry := range command.Env {
				if strings.HasPrefix(entry, key+"=") {
					command.Env[i] = key + "=" + value
					overridden = true
				}
			}
			if !overridden {
				command.Env = append(command.Env, key+"="+value)
			}
		}
	}
	output, err := command.CombinedOutput()
	if err == nil {
		return 0, string(output)
	}
	var status *exec.ExitError
	if as, ok := err.(*exec.ExitError); ok {
		status = as
	} else {
		t.Fatalf("could not run %s: %v", script, err)
	}
	return status.ExitCode(), string(output)
}

func scriptPath(t *testing.T, onDisk string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", onDisk))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// TestHealthcheckScriptAcceptsAndRefuses runs the whole delivered probe against substitute
// replies. Healthy means exit code zero and the counted index summary; everything else has to
// exit non-zero, and the reason is checked so a generic failure cannot stand in for the right one.
func TestHealthcheckScriptAcceptsAndRefuses(t *testing.T) {
	fixture := newIndexFixture(t)
	fixture.writeConf(t, "requirepass "+syntheticSecret+"\n")
	probe := scriptPath(t, indexProbeOnDisk)

	healthy := []struct {
		name    string
		reply   string
		indexes string
	}{
		{"empty array is a live module with no index yet", "[]", "0"},
		{"one index name", `["gopherai_knowledge_chunks_v3_idx"]`, "1"},
		{"three index names", `["gopherai_knowledge_chunks_v3_idx","gopherai_episode_v2_idx","gopherai_memory_v2_idx"]`, "3"},
		{"a name with a dot and a hyphen", `["idx-1.0"]`, "1"},
	}
	for _, tc := range healthy {
		fixture.setReply(t, tc.reply)
		code, output := runIndexScript(t, probe, map[string]string{
			"REDIS_STACK_CONF": posixForSh(fixture.conf),
			"REDIS_CLI":        posixForSh(fixture.client),
		})
		want := fmt.Sprintf("redis-stack health: ok (indexes=%s)", tc.indexes)
		if code != 0 || !strings.Contains(output, want) {
			t.Errorf("%s: rc=%d, want rc=0 and %q, saw %q", tc.name, code, want, output)
		}
	}

	unhealthy := []struct {
		name     string
		reply    string
		conf     string
		fakeRC   string
		wantTerm string
	}{
		{name: "a number element", reply: "[123]", wantTerm: "non-string element"},
		{name: "mixed element types", reply: `["a",2]`, wantTerm: "non-string element"},
		{name: "an object reply", reply: `{"ok":true}`, wantTerm: "not a JSON array"},
		{name: "plain text", reply: "not json at all", wantTerm: "not a JSON array"},
		{name: "an unclosed string", reply: `["unclosed`, wantTerm: "array"},
		{name: "a bare token", reply: "[a]", wantTerm: "non-string"},
		{name: "a truncated reply", reply: "[1", wantTerm: "array"},
		{name: "nested array", reply: `[["a"]]`, wantTerm: "non-string"},
		{name: "module missing", reply: "(error) ERR unknown command 'FT._LIST'", wantTerm: "module missing"},
		{name: "auth required, error form", reply: "(error) NOAUTH Authentication required.", wantTerm: "authentication rejected"},
		{name: "auth required, bare form", reply: "NOAUTH Authentication required.", wantTerm: "authentication rejected"},
		{name: "wrong password", reply: "(error) WRONGPASS invalid username-password pair", wantTerm: "authentication rejected"},
		{name: "the word NOAUTH inside an index name", reply: `["NOAUTH"]`, wantTerm: "authentication rejected"},
		{name: "connection refused", reply: "Could not connect to Redis at 127.0.0.1:6379: Connection refused", wantTerm: "connection failed"},
		{name: "the client itself failed", reply: "[]", fakeRC: "1", wantTerm: "probe command failed"},
		{name: "configuration without requirepass", reply: "[]", conf: "maxmemory 10mb\n", wantTerm: "exactly one"},
		{name: "two requirepass lines", reply: "[]",
			conf: "requirepass " + syntheticSecret + "\nrequirepass second-long-enough-value-0123456789\n", wantTerm: "exactly one"},
		{name: "empty requirepass value", reply: "[]", conf: "requirepass\n", wantTerm: "exactly one"},
		{name: "credential shorter than the floor", reply: "[]", conf: "requirepass too-short\n", wantTerm: "shorter than the agreed floor"},
		{name: "an unfilled placeholder", reply: "[]", conf: "requirepass REPLACE_WITH_REDIS_PASSWORD\n", wantTerm: "shorter than the agreed floor"},
		{name: "no configuration at all", reply: "[]", conf: "@absent@", wantTerm: "unreadable"},
	}
	for _, tc := range unhealthy {
		fixture.setReply(t, tc.reply)
		conf := tc.conf
		if conf == "" {
			conf = "requirepass " + syntheticSecret + "\n"
		}
		if conf == "@absent@" {
			conf = filepath.Join(fixture.conf + "-not-here")
		} else {
			fixture.writeConf(t, conf)
			conf = fixture.conf
		}
		env := map[string]string{"REDIS_STACK_CONF": posixForSh(conf), "REDIS_CLI": posixForSh(fixture.client)}
		if tc.fakeRC != "" {
			env["FAKE_RC"] = tc.fakeRC
		}
		code, output := runIndexScript(t, probe, env)
		if code == 0 {
			t.Errorf("%s: read as healthy (output %q)", tc.name, output)
			continue
		}
		if strings.Contains(output, "ok (indexes") {
			t.Errorf("%s: printed a healthy summary with rc=%d: %q", tc.name, code, output)
		}
		if !strings.Contains(output, tc.wantTerm) {
			t.Errorf("%s: refused, but not for the checked reason (%q, wanted %q)", tc.name, output, tc.wantTerm)
		}
		if strings.Contains(output, syntheticSecret) {
			t.Errorf("%s: the credential appeared in the output: %q", tc.name, output)
		}
		fixture.writeConf(t, "requirepass "+syntheticSecret+"\n")
	}
}

// TestHealthcheckLeakBoundaryIsMeasured runs the accepted path once and inspects what the
// substitute recorded: the credential must be absent from argv and from the output, and present
// only through the probe child's own environment.
func TestHealthcheckLeakBoundaryIsMeasured(t *testing.T) {
	fixture := newIndexFixture(t)
	fixture.writeConf(t, "requirepass "+syntheticSecret+"\n")
	fixture.setReply(t, `["gopherai_knowledge_chunks_v3_idx"]`)

	code, output := runIndexScript(t, scriptPath(t, indexProbeOnDisk), map[string]string{
		"REDIS_STACK_CONF": posixForSh(fixture.conf),
		"REDIS_CLI":        posixForSh(fixture.client),
	})
	if code != 0 {
		t.Fatalf("the accepted case did not pass: rc=%d %q", code, output)
	}
	argv := strings.TrimSpace(fixture.read(t, fixture.argvLog))
	if argv != "ARGV:-2 --json FT._LIST" {
		t.Errorf("the probe invoked the client as %q, want %q", argv, "ARGV:-2 --json FT._LIST")
	}
	if strings.Contains(argv, syntheticSecret) || strings.Contains(output, syntheticSecret) {
		t.Errorf("the credential reached argv or output: argv=%q output=%q", argv, output)
	}
	childEnv := fixture.read(t, fixture.envLog)
	lines := []string{}
	for _, line := range strings.Split(childEnv, "\n") {
		if strings.HasPrefix(line, "REDISCLI_AUTH=") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || lines[0] != "REDISCLI_AUTH="+syntheticSecret {
		t.Errorf("the credential channel is not exactly one REDISCLI_AUTH entry: saw %d entries", len(lines))
	}
	if !strings.Contains(output, "ok (indexes=1)") {
		t.Errorf("unexpected healthy summary: %q", output)
	}
}

// TestPreflightRefusesBeforeTheServerStarts executes the delivered preflight. The judge is not a
// string: the substitute image entrypoint records whether it was reached at all, so a refusal
// that still started Redis fails here.
func TestPreflightRefusesBeforeTheServerStarts(t *testing.T) {
	fixture := newIndexFixture(t)
	preflight := scriptPath(t, indexPreflightOnDisk)

	cases := []struct {
		name     string
		conf     string
		wantTerm string
	}{
		{"no configuration file", "@absent@", "configuration missing"},
		{"an empty file", "", "configuration empty"},
		{"no requirepass at all", "maxmemory 10mb\n", "exactly once"},
		{"two requirepass lines", "requirepass " + syntheticSecret + "\nrequirepass second-long-enough-value-0123456789\n", "exactly once"},
		{"an empty requirepass value", "requirepass\n", "exactly once"},
		{"a placeholder nobody replaced", "requirepass REPLACE_WITH_REDIS_PASSWORD_0123456789\n", "placeholder"},
		{"a quoted value", `requirepass "quoted-secret-value-0123456789abcdef"`, "quoting"},
		// A value containing spaces is read by both scripts as its first token, so it cannot be
		// two different values in the two places: what rejects it is the length floor, and that
		// is what is asserted here rather than a quoting rule this shape never reaches.
		{"a value with embedded spaces", "requirepass has embedded words here\n", "shorter than the agreed floor"},
		{"shorter than the agreed floor", "requirepass short\n", "shorter than the agreed floor"},
	}
	for _, tc := range cases {
		conf := fixture.conf
		if tc.conf == "@absent@" {
			conf = filepath.Join(fixture.dir, "absent", "redis-stack.conf")
		} else {
			fixture.writeConf(t, tc.conf)
		}
		code, output := runIndexScript(t, preflight, map[string]string{
			"REDIS_STACK_CONF":       posixForSh(conf),
			"REDIS_STACK_ENTRYPOINT": posixForSh(fixture.entry),
		})
		if code == 0 {
			t.Errorf("%s: the server would have started (output %q)", tc.name, output)
		}
		if reached := fixture.reachedCount(t); reached != 0 {
			t.Errorf("%s: refused with rc=%d but still handed over to the entrypoint %d time(s)", tc.name, code, reached)
		}
		if !strings.Contains(output, tc.wantTerm) {
			t.Errorf("%s: refused, but not for the checked reason (%q, wanted %q)", tc.name, output, tc.wantTerm)
		}
		if strings.Contains(output, syntheticSecret) {
			t.Errorf("%s: the credential appeared in the output: %q", tc.name, output)
		}
	}

	// The accepted case: it hands over exactly once, and hands over with no arguments of its
	// own, because the server is expected to read its configuration file.
	fixture.writeConf(t, "requirepass "+syntheticSecret+"\n")
	code, output := runIndexScript(t, preflight, map[string]string{
		"REDIS_STACK_CONF":       posixForSh(fixture.conf),
		"REDIS_STACK_ENTRYPOINT": posixForSh(fixture.entry),
	})
	if code != 0 {
		t.Fatalf("a conforming configuration was refused: rc=%d %q", code, output)
	}
	if reached := fixture.reachedCount(t); reached != 1 {
		t.Errorf("the entrypoint ran %d times, want exactly 1", reached)
	}
	if args := strings.TrimSpace(fixture.read(t, fixture.entryArgs)); args != "ENTRY:" {
		t.Errorf("the wrapper passed arguments to the image entrypoint: %q", args)
	}

	// The shipped example is safe to commit because the preflight refuses it: copying it without
	// replacing the placeholder must produce a refusal, not an open server. This is the one
	// end-to-end read of the committed example file.
	before := fixture.reachedCount(t)
	example, err := filepath.Abs(filepath.Join("..", "redis.integrated.conf.example"))
	if err != nil {
		t.Fatal(err)
	}
	code, output = runIndexScript(t, preflight, map[string]string{
		"REDIS_STACK_CONF":       posixForSh(example),
		"REDIS_STACK_ENTRYPOINT": posixForSh(fixture.entry),
	})
	if code == 0 || !strings.Contains(output, "placeholder") {
		t.Errorf("the committed example was not refused as a placeholder: rc=%d %q", code, output)
	}
	if after := fixture.reachedCount(t); after != before {
		t.Errorf("the example still handed over to the entrypoint, so an unreplaced placeholder would start the server")
	}
}
