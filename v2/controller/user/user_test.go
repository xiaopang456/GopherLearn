package user

import (
	"encoding/json"
	"reflect"
	"testing"
)

// BUG-AGENT-001 shipped `json:password` without quotes. reflect.StructTag.Get
// could not parse it, so `go vet ./...` failed for the whole module from
// AGENT-001 onwards. encoding/json silently fell back to a case-insensitive
// field-name match, which is why login kept working and the defect survived.
//
// These tests lock both halves down: the tag must be machine-readable, and the
// wire contract must stay exactly what the frontend already sends.
func TestRequestJSONTagsAreParsable(t *testing.T) {
	cases := []struct {
		name     string
		typ      reflect.Type
		expected map[string]string
	}{
		{
			name: "LoginRequest",
			typ:  reflect.TypeOf(LoginRequest{}),
			expected: map[string]string{
				"Username": "username",
				"Password": "password",
			},
		},
		{
			name: "RegisterRequest",
			typ:  reflect.TypeOf(RegisterRequest{}),
			expected: map[string]string{
				"Email":    "email",
				"Captcha":  "captcha",
				"Password": "password",
			},
		},
		{
			name:     "CaptchaRequest",
			typ:      reflect.TypeOf(CaptchaRequest{}),
			expected: map[string]string{"Email": "email"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for field, want := range tc.expected {
				structField, ok := tc.typ.FieldByName(field)
				if !ok {
					t.Fatalf("field %s is missing from %s", field, tc.name)
				}
				// StructTag.Get returns "" both for a missing tag and for a
				// malformed one, which is exactly how the original defect
				// stayed invisible to everything except go vet.
				if got := structField.Tag.Get("json"); got != want {
					t.Errorf("field %s json tag = %q, want %q", field, got, want)
				}
			}
		})
	}
}

func TestLoginRequestBindsLowercaseJSON(t *testing.T) {
	req := new(LoginRequest)
	if err := json.Unmarshal([]byte(`{"username":"agentdemo","password":"secret"}`), req); err != nil {
		t.Fatalf("unmarshal login request: %v", err)
	}
	if req.Username != "agentdemo" {
		t.Errorf("Username = %q, want %q", req.Username, "agentdemo")
	}
	if req.Password != "secret" {
		t.Errorf("Password = %q, want %q", req.Password, "secret")
	}
}

func TestRegisterRequestBindsLowercaseJSON(t *testing.T) {
	req := new(RegisterRequest)
	body := `{"email":"user@example.com","captcha":"123456","password":"secret"}`
	if err := json.Unmarshal([]byte(body), req); err != nil {
		t.Fatalf("unmarshal register request: %v", err)
	}
	if req.Email != "user@example.com" || req.Captcha != "123456" || req.Password != "secret" {
		t.Errorf("register request bound as %+v", req)
	}
}

func TestRegisterResponseIncludesGeneratedUsername(t *testing.T) {
	encoded, err := json.Marshal(RegisterResponse{Token: "token", Username: "12345678901"})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["username"] != "12345678901" || payload["token"] != "token" {
		t.Fatalf("response payload=%v", payload)
	}
}
