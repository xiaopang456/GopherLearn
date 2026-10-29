package model

import (
	"reflect"
	"testing"
)

func TestMessageSourcesValueAndScanRoundTrip(t *testing.T) {
	original := MessageSources{{Citation: 1, DocID: "doc", FileName: "redis.md", TitlePath: []string{"Redis", "TTL"}, Snippet: "EX 设置 TTL"}}
	value, err := original.Value()
	if err != nil {
		t.Fatal(err)
	}
	var decoded MessageSources
	if err := decoded.Scan(value); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("decoded = %+v, want %+v", decoded, original)
	}
	var empty MessageSources
	if err := empty.Scan(nil); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("NULL sources = %#v, err=%v", empty, err)
	}
}

func TestMessageSourcesRejectsMalformedJSON(t *testing.T) {
	var sources MessageSources
	if err := sources.Scan([]byte(`{"doc_id":`)); err == nil {
		t.Fatal("malformed sources were accepted")
	}
}
