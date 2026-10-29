package model

import (
	"database/sql/driver"
	"reflect"
	"testing"
)

func TestMergeEpisodeSourceRefsBuildsDeterministicClosure(t *testing.T) {
	refs, err := MergeEpisodeSourceRefs(
		EpisodeSourceRefs{{EpisodeID: "b", Version: 2}},
		EpisodeSourceRefs{{EpisodeID: "a", Version: 1}, {EpisodeID: "b", Version: 2}},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := EpisodeSourceRefs{{EpisodeID: "a", Version: 1}, {EpisodeID: "b", Version: 2}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs=%+v want=%+v", refs, want)
	}
}

func TestEpisodeSourceRefsRejectUnsafeLineage(t *testing.T) {
	if _, err := MergeEpisodeSourceRefs(EpisodeSourceRefs{{EpisodeID: "a", Version: 1}, {EpisodeID: "a", Version: 2}}); err == nil {
		t.Fatal("conflicting source versions must fail")
	}
	tooMany := make(EpisodeSourceRefs, EpisodeSourceRefLimit+1)
	for i := range tooMany {
		tooMany[i] = EpisodeSourceRef{EpisodeID: string(rune('a' + i)), Version: 1}
	}
	if _, err := tooMany.Value(); err == nil {
		t.Fatal("source overflow must fail instead of truncating")
	}
	var refs EpisodeSourceRefs
	if err := refs.Scan(driver.Value(`[{"episode_id":"a","version":0}]`)); err == nil {
		t.Fatal("zero source version must fail")
	}
}
