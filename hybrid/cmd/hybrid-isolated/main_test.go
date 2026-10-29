package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIRefusesMissingConfigurationWithNonzeroExit(t *testing.T) {
	var output bytes.Buffer
	if code := runCLI(context.Background(), nil, &output); code == 0 || output.Len() == 0 {
		t.Fatal("startup refusal did not fail CLI")
	}
}

func TestCLIInvalidConfigurationDoesNotPrintValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.toml")
	if err := os.WriteFile(path, []byte("INVALID_PRIVATE_VALUE ["), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := runCLI(context.Background(), []string{"-config", path}, &output); code == 0 || bytes.Contains(output.Bytes(), []byte("INVALID_PRIVATE_VALUE")) {
		t.Fatal("invalid config exit/output contract")
	}
}
