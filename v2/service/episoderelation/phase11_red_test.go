package episoderelation

import (
	"os"
	"strings"
	"testing"
)

// STABILIZE-065 Phase 1.1 red lights.
//
// Phase 1 built a component and called it done. These state the ten things
// that were missing between "a package exists" and "production uses it", and
// each one is a behavioural claim wherever a behaviour exists to check. Where
// the claim is "this code has no production caller at all", the only honest
// check is over the source, and those three are marked as such rather than
// dressed up as behaviour.

// readRepoFile reads one file of the repository as text.
func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	blob, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return string(blob)
}

// Red 1: there is no production CandidateSource. The interface exists and
// every implementation of it is a test double, which means the finder has
// never read a row from a database.
//
// Source-level by necessity: the defect is the absence of an implementation,
// and an absent type has no behaviour to assert.
func TestRedNoProductionCandidateSource(t *testing.T) {
	source := readRepoFile(t, "dao/episoderelation/candidates.go")
	if !strings.Contains(source, "func (r *CandidateRepository) ActiveAssertions") {
		t.Error("RED: no production CandidateSource reads the learner's episodes")
	}
}

// Red 2: nothing in production calls the generator. The worker projects, the
// indexer indexes, and the relation pass happens only in tests.
func TestRedNoProductionRelationSeam(t *testing.T) {
	worker := readRepoFile(t, "service/episode/worker.go")
	if !strings.Contains(worker, "RelationWriter") {
		t.Error("RED: the episode worker has no relation seam")
	}
	main := readRepoFile(t, "main.go")
	if !strings.Contains(main, "episoderelation") {
		t.Error("RED: main.go assembles no relation writer, source, store or classifier")
	}
	if !strings.Contains(main, "EpisodeRelationMode") {
		t.Error("RED: there is no configuration selecting off, shadow or apply")
	}
}

// Red 3: there is no strict-JSON classifier over the project's chat model.
func TestRedNoStrictJSONClassifierAdapter(t *testing.T) {
	source := readRepoFile(t, "service/episoderelation/chat_classifier.go")
	for _, required := range []string{"DisallowUnknownFields", "io.EOF"} {
		if !strings.Contains(source, required) {
			t.Errorf("RED: the chat classifier does not enforce %s", required)
		}
	}
}
