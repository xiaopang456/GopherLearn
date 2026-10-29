package rag

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	declarationPattern      = regexp.MustCompile(`(?m)\b(?:package|func|type|var|const)\s+(?:\([^\n)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`)
	declarationGroupPattern = regexp.MustCompile(`(?ms)\b(?:var|const)\s*\((.*?)\)`)
	groupNamePattern        = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\b`)
	qualifiedPattern        = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*\b`)
	sqlObjectPattern        = regexp.MustCompile(`(?i)\b(?:table|from|join|into|update)\s+([A-Za-z_][A-Za-z0-9_.]*)`)
	shellVariablePattern    = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)
	identifierPattern       = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_-]*\b`)
)

// ExtractSymbols returns stable first-occurrence symbols useful for later
// keyword retrieval. It deliberately avoids language-specific AST parsing.
func ExtractSymbols(content string) []string {
	result := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(symbol string) {
		symbol = strings.TrimSpace(symbol)
		if symbol == "" {
			return
		}
		key := strings.ToLower(symbol)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		result = append(result, symbol)
	}

	for _, match := range declarationPattern.FindAllStringSubmatch(content, -1) {
		add(match[1])
	}
	for _, group := range declarationGroupPattern.FindAllStringSubmatch(content, -1) {
		for _, match := range groupNamePattern.FindAllStringSubmatch(group[1], -1) {
			add(match[1])
		}
	}
	for _, symbol := range qualifiedPattern.FindAllString(content, -1) {
		add(symbol)
	}
	for _, match := range sqlObjectPattern.FindAllStringSubmatch(content, -1) {
		add(match[1])
	}
	for _, match := range shellVariablePattern.FindAllStringSubmatch(content, -1) {
		add(match[1])
	}
	for _, symbol := range identifierPattern.FindAllString(content, -1) {
		if looksLikeTechnicalSymbol(symbol) {
			add(symbol)
		}
	}
	return result
}

func looksLikeTechnicalSymbol(value string) bool {
	if strings.ContainsAny(value, "_-") {
		return len(value) >= 3
	}
	hasLower, hasUpper := false, false
	for i, r := range value {
		if unicode.IsLower(r) {
			hasLower = true
		}
		if unicode.IsUpper(r) {
			hasUpper = true
			if i > 0 && hasLower {
				return true
			}
		}
	}
	return hasUpper && !hasLower && len(value) >= 2
}
