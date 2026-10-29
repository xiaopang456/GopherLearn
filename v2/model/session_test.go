package model

import "testing"

func TestParseSessionModeCompatibility(t *testing.T) {
	tests := map[string]SessionMode{
		"1":          SessionModeNormal,
		"normal":     SessionModeNormal,
		"2":          SessionModeRAG,
		"RAG":        SessionModeRAG,
		"3":          SessionModeMCPLegacy,
		"mcp":        SessionModeMCPLegacy,
		"4":          SessionModeOllama,
		"5":          SessionModeAgent,
		" agent ":    SessionModeAgent,
		"mcp_legacy": SessionModeMCPLegacy,
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			got, err := ParseSessionMode(input)
			if err != nil || got != want {
				t.Fatalf("ParseSessionMode(%q) = (%q, %v), want %q", input, got, err, want)
			}
		})
	}
	if _, err := ParseSessionMode("unknown"); err == nil {
		t.Fatal("expected unsupported mode error")
	}
}

func TestSessionModeModelType(t *testing.T) {
	tests := map[SessionMode]string{
		SessionModeNormal:    "1",
		SessionModeRAG:       "2",
		SessionModeMCPLegacy: "3",
		SessionModeOllama:    "4",
		SessionModeAgent:     "5",
	}
	for mode, want := range tests {
		got, err := mode.ModelType()
		if err != nil || got != want {
			t.Fatalf("ModelType(%q) = (%q, %v), want %q", mode, got, err, want)
		}
	}
}

func TestSessionValidateForCreate(t *testing.T) {
	valid := Session{ID: "session", UserID: 1, UserName: "alice", Mode: SessionModeRAG}
	if err := valid.ValidateForCreate(); err != nil {
		t.Fatal(err)
	}
	tests := []Session{
		{UserID: 1, UserName: "alice", Mode: SessionModeRAG},
		{ID: "session", UserName: "alice", Mode: SessionModeRAG},
		{ID: "session", UserID: 1, Mode: SessionModeRAG},
		{ID: "session", UserID: 1, UserName: "alice", Mode: "invalid"},
	}
	for i := range tests {
		if err := tests[i].ValidateForCreate(); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
}
