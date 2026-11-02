package conversation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotArchiveRoundTripAndTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	s := validSnapshot()
	key := "request-123"
	s.Session.CreationRequestKey = &key
	s.Session.CreationInputHash = strings.Repeat("a", 64)
	if err := WriteSnapshot(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Session.CreationRequestKey == nil || *got.Session.CreationRequestKey != key || got.Session.CreationInputHash != s.Session.CreationInputHash {
		t.Fatal("session metadata lost")
	}
	if err := WriteSnapshot(path, s); err == nil {
		t.Fatal("overwrote snapshot")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "hello", "HELLO", 1))
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadSnapshot(path); err == nil {
		t.Fatal("accepted modified snapshot")
	}
}
