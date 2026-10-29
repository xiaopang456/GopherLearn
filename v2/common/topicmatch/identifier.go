package topicmatch

import (
	"regexp"
	"strings"
)

// StrongIdentifiers pulls the code-shaped tokens out of a question.
//
// The rule lived inside the episode router until STABILIZE-031 needed it in two
// places: the Episode exact channel, which searches on them, and the semantic
// topic gate, which refuses a candidate topic that contains none of them. Two
// copies of "what counts as an identifier" would drift, and the drift would be
// invisible — the two callers would simply disagree about one question.
//
// The bar is deliberately high. A three-letter acronym is not an identifier:
// "RPC" appears in ordinary prose, and promoting it would make the negative
// evidence rule fire on questions that named nothing in particular. What
// qualifies is a digit, a separator, mixed case, or real length.
var identifierPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9._:/-]{2,}`)

func StrongIdentifiers(question string) []string {
	matches := identifierPattern.FindAllString(question, -1)
	seen := map[string]struct{}{}
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		match = strings.Trim(match, "._:/-")
		if !IsStrongIdentifier(match) {
			continue
		}
		key := strings.ToLower(match)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, match)
	}
	return result
}

// IsStrongIdentifier is the single definition of the bar.
func IsStrongIdentifier(value string) bool {
	if len(value) < 4 {
		return false
	}
	hasLower, hasUpper, hasDigit, hasSeparator := false, false, false, false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case strings.ContainsRune("._:/-", r):
			hasSeparator = true
		}
	}
	return hasDigit || hasSeparator || (hasLower && hasUpper) || len(value) >= 8
}
