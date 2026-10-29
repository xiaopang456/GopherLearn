package rag

import (
	"reflect"
	"testing"
)

func TestSymbolExtractionAcrossTechnicalNotes(t *testing.T) {
	t.Parallel()

	content := `package service

type UserService struct{}

func (s *UserService) GetUser() {
	var userID int64
	_ = gorm.DB
}

const (
	StatusReady = "ready"
)

CREATE TABLE user_sessions (id BIGINT);
SELECT * FROM knowledge_documents;
echo $OPENAI_API_KEY --max-retries
`
	got := ExtractSymbols(content)
	wantPrefix := []string{
		"service", "UserService", "GetUser", "userID",
	}
	if len(got) < len(wantPrefix) || !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("ExtractSymbols() = %v, want prefix %v", got, wantPrefix)
	}
	assertContainsSymbol(t, got, "StatusReady")
	assertContainsSymbol(t, got, "gorm.DB")
	assertContainsSymbol(t, got, "user_sessions")
	assertContainsSymbol(t, got, "knowledge_documents")
	assertContainsSymbol(t, got, "OPENAI_API_KEY")
	assertContainsSymbol(t, got, "max-retries")
}

func TestSymbolExtractionDeduplicatesCaseInsensitively(t *testing.T) {
	t.Parallel()

	got := ExtractSymbols("func GetUser() {}\nGetUser()\ngetuser()")
	count := 0
	for _, symbol := range got {
		if symbol == "GetUser" || symbol == "getuser" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("case-insensitive duplicate count = %d, symbols %v", count, got)
	}
}

func TestTokenEstimateHandlesASCIIAndChineseDeterministically(t *testing.T) {
	t.Parallel()

	if got := EstimateTokens("abcdefgh"); got != 2 {
		t.Errorf("EstimateTokens(ASCII) = %d, want 2", got)
	}
	if got := EstimateTokens("并发安全"); got != 4 {
		t.Errorf("EstimateTokens(CJK) = %d, want 4", got)
	}
	if first, second := EstimateTokens("GetUser(ctx)\n用户"), EstimateTokens("GetUser(ctx)\n用户"); first != second {
		t.Errorf("token estimate is unstable: %d != %d", first, second)
	}
}

func assertContainsSymbol(t *testing.T, symbols []string, want string) {
	t.Helper()
	for _, symbol := range symbols {
		if symbol == want {
			return
		}
	}
	t.Errorf("symbols %v do not contain %q", symbols, want)
}
