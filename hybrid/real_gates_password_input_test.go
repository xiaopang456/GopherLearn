//go:build v2_real_gates

package hybrid

import (
	"GopherAI/entry"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestI0PasswordStdinOnlyChangesThePassword(t *testing.T) {
	s := testSettings(t)
	s.Config.MysqlPassword = ""
	before := *s.Config
	value := strings.Repeat("a", 64)
	if err := applyI0PasswordInput(context.Background(), s, io.NopCloser(strings.NewReader(value+"\n"))); err != nil {
		t.Fatal(err)
	}
	if s.Config.MysqlPassword != value {
		t.Fatal("test password not assigned")
	}
	s.Config.MysqlPassword = ""
	if !reflect.DeepEqual(*s.Config, before) {
		t.Fatal("non-password configuration changed")
	}
}

func TestI0PasswordStdinRejectsMalformedOrExistingPassword(t *testing.T) {
	for _, text := range []string{"", strings.Repeat("a", 63) + "\n", strings.Repeat("a", 65) + "\n", strings.Repeat("A", 64) + "\n", strings.Repeat("a", 64), strings.Repeat("a", 64) + "\nextra", strings.Repeat("a", 64) + "\r\n"} {
		s := testSettings(t)
		s.Config.MysqlPassword = ""
		if err := applyI0PasswordInput(context.Background(), s, io.NopCloser(strings.NewReader(text))); err == nil {
			t.Fatal("invalid input accepted")
		}
		if s.Config.MysqlPassword != "" {
			t.Fatal("invalid input changed config")
		}
	}
	s := testSettings(t)
	s.Config.MysqlPassword = "existing"
	if err := applyI0PasswordInput(context.Background(), s, io.NopCloser(strings.NewReader(strings.Repeat("a", 64)+"\n"))); err == nil {
		t.Fatal("conflicting password accepted")
	}
	if s.Config.MysqlPassword != "existing" {
		t.Fatal("existing config changed")
	}
	if err := applyI0PasswordInput(context.Background(), (*entry.Settings)(nil), nil); err == nil {
		t.Fatal("nil accepted")
	}
}

func TestI0PasswordStdinTimeoutClosesInput(t *testing.T) {
	s := testSettings(t)
	s.Config.MysqlPassword = ""
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := applyI0PasswordInput(ctx, s, r); err == nil {
		t.Fatal("timeout accepted")
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("input not closed on timeout")
	}
	if s.Config.MysqlPassword != "" {
		t.Fatal("timeout changed config")
	}
}
