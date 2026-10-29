package app

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// compose.redis.yml is the only file in this repository that names an external stateful
// service the application can talk to, so the checks below are about the shape of the
// exposure rather than about Redis itself: what an operator can reach from outside the
// host, whether a credential has to be typed into a file that gets committed, and whether
// the file quietly grew a second service.

const redisComposeFile = "compose.redis.yml"

func TestRedisComposePublishesLoopbackOnly(t *testing.T) {
	// Comments are stripped first: this file explains, in prose, the interpolation and the
	// tag it refuses to use, and a check that reads prose would fail on the explanation.
	text := withoutComments(repoText(t, redisComposeFile))

	// No interpolation at all here: a value this file needs would otherwise land in
	// `docker compose config` output, which operators paste into tickets.
	if bad := regexp.MustCompile(`\$\{[^}]*\}`).FindString(text); bad != "" {
		t.Errorf("%s interpolates %q; this service takes no credentials from the shell", redisComposeFile, bad)
	}
	if strings.Contains(strings.ToLower(text), "password:") {
		t.Error("a Redis password must never be written in a committed file")
	}
	// Loopback binding is the isolation this file relies on, because the image's own
	// entrypoint starts Redis with --protected-mode no.
	for _, forbidden := range []string{`"6379:6379"`, `"0.0.0.0:`, `":6379:"`, "- 6379:"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("%s publishes on more than the loopback interface: %s", redisComposeFile, forbidden)
		}
	}
	if !strings.Contains(text, `"127.0.0.1:6380:6379"`) {
		t.Error(`the index must be published as "127.0.0.1:6380:6379" so the host backend can reach it`)
	}
	for _, name := range regexp.MustCompile(`(?m)^\s+name: (\S+)`).FindAllStringSubmatch(text, -1) {
		if !strings.HasPrefix(name[1], "gopherlearn-") {
			t.Errorf("volume or container %q is not named after this project", name[1])
		}
	}
	if !strings.Contains(text, "name: gopherlearn-redis-11a-data") {
		t.Error("the index needs its own round-specific volume, not a shared one")
	}
}

func TestRedisComposeRunsTheModuleBearingImageOnly(t *testing.T) {
	text := withoutComments(repoText(t, redisComposeFile))

	// A moving tag would make "the version we verified" a claim about whatever the
	// registry handed out that day.
	if !strings.Contains(text, "image: redis/redis-stack-server@sha256:") {
		t.Error("redis-stack-server must be pinned to a digest: the search module's behaviour changes with its version")
	}
	// `name:tag@sha256:…` is legal compose and pointless: the digest wins. The pattern has
	// to allow the digest's own colon, so it looks for a tag colon before any '@'.
	if regexp.MustCompile(`image: [^\s@]*:\S`).MatchString(text) {
		t.Error("a digest-pinned image must not also carry a tag")
	}
	// Plain `redis` answers FT.CREATE with an unknown-command error, so the stack
	// variant is not a preference.
	if strings.Contains(text, "image: redis@") || strings.Contains(text, "image: redis:7") {
		t.Error("redis/redis-stack-server is required: the KNN query needs the RediSearch module")
	}
	if !strings.Contains(text, "FT._LIST") {
		t.Error("the healthcheck must prove the search module answered, not that a port opened")
	}
	if !strings.Contains(text, "REDIS_DATA_DIR: /data") || !strings.Contains(text, "- data:/data") {
		t.Error("the volume must be mounted where the entrypoint writes, and say so")
	}

	// Exactly one service, and it is not this file's business to start anything else.
	block := servicesBlock(t, withoutComments(text))
	services := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9_-]*):\s*$`).FindAllStringSubmatch(block, -1)
	if len(services) != 1 || services[0][1] != "redis" {
		names := make([]string, 0, len(services))
		for _, match := range services {
			names = append(names, match[1])
		}
		t.Errorf("expected exactly one service named redis, got %v", names)
	}
	for _, forbidden := range []string{"mysql", "rabbitmq", "minio", "kafka", "nats", "ollama", "tei", "backend", "frontend"} {
		if strings.Contains(strings.ToLower(block), forbidden) {
			t.Errorf("this file adds no %s service", forbidden)
		}
	}
	// The default stack must stay startable without this file.
	if strings.Contains(withoutComments(text), "depends_on") {
		t.Error("nothing may depend on the index: semantic retrieval is opt-in")
	}
}

// servicesBlock returns the indented body of the top-level `services:` mapping. sectionOf
// cannot do this: it stops at the first two-space-indented line, which is exactly where a
// service name begins, and the top-level `volumes:` key is indented the same way as a
// service, so scanning the whole file would read "data" as a second service.
func servicesBlock(t *testing.T, text string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if line != "services:" {
			continue
		}
		var body []string
		for _, next := range lines[index+1:] {
			if strings.TrimSpace(next) == "" {
				continue
			}
			if !strings.HasPrefix(next, " ") {
				break
			}
			body = append(body, next)
		}
		if len(body) == 0 {
			t.Fatal("services: block is empty")
		}
		return strings.Join(body, "\n")
	}
	t.Fatal(`no top-level "services:" line`)
	return ""
}

// TestRedisIsDeclaredOnlyInRegisteredComposeFiles is the scope rule the retrieval mode depends
// on, narrowed on 2026-10-15 with the owner's approval so the integrated stack may declare an
// index of its own.
//
// The original rule was written when one stack existed: exactly one root compose file may
// declare a redis service, because 8C/8D's acceptance story is a three-service stack needing
// nothing beyond MySQL, and a sneaked `redis` line would quietly make the vector index a
// startup dependency of the retrieval mode that does not use it. The integrated stack
// legitimately declares one - there search *is* a startup dependency and the backend waits for
// its health - so the rule becomes an allow-list of registered declarers instead of a single
// filename.
//
// Everything that was prohibited still is: no default-stack file may declare, reference or
// include the index; an unregistered third file may not appear; and the integrated stack may
// not reuse a single baseline container, volume or network name.
func TestRedisIsDeclaredOnlyInRegisteredComposeFiles(t *testing.T) {
	registered := map[string]bool{redisComposeFile: true, "compose.integrated.yml": true}
	names, err := filepath.Glob(filepath.Join("..", "..", "compose*.yml"))
	if err != nil {
		t.Fatalf("glob compose files: %v", err)
	}
	want := make([]string, 0, len(registered))
	for name := range registered {
		want = append(want, name)
	}
	sort.Strings(want)
	var declaring []string
	for _, name := range names {
		base := filepath.Base(name)
		text := withoutComments(repoText(t, base))
		if !strings.Contains(strings.ToLower(servicesBlock(t, text)), "redis") {
			continue
		}
		declaring = append(declaring, base)
		if !registered[base] {
			t.Errorf("compose file %q declares a redis service but is not a registered declarer", base)
		}
	}
	sort.Strings(declaring)
	if len(declaring) != len(want) {
		t.Fatalf("compose files declaring a redis service = %v, want exactly the registered %v", declaring, want)
	}
	// The default stack stays startable without Redis: no declaration, no dependency, no
	// reference to the overlay, and no compose `include` that would make it unavoidable.
	for _, name := range []string{"compose.app.yml", "compose.mysql.yml", "compose.provider.yml"} {
		text := repoText(t, name)
		if strings.Contains(strings.ToLower(withoutComments(text)), "redis") {
			t.Errorf("%s declares or depends on redis, so the default stack is no longer startable without it", name)
		}
		if strings.Contains(text, redisComposeFile) {
			t.Errorf("%s references %s, so semantic retrieval is no longer opt-in", name, redisComposeFile)
		}
		if regexp.MustCompile(`(?m)^\s*include:`).MatchString(text) {
			t.Errorf("%s uses compose `include:`, which would make the overlay unavoidable", name)
		}
	}
	// Resource identity is never shared: `docker compose` reuses by project name plus service
	// name, and these two stacks must not be able to write one dataset by accident.
	baseline := repoText(t, "compose.app.yml") + "\n" + repoText(t, redisComposeFile)
	integrated := repoText(t, "compose.integrated.yml")
	for _, owned := range regexp.MustCompile(`(?m)^[ \t]*(?:container_)?name:[ \t]*(\S+)[ \t]*$`).FindAllStringSubmatch(baseline, -1) {
		if strings.HasPrefix(owned[1], "gopherlearn-") && strings.Contains(integrated, owned[1]) {
			t.Errorf("the integrated stack reuses the baseline resource name %q", owned[1])
		}
	}
}
