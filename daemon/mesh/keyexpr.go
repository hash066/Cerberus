package mesh

import "strings"

// MatchKey reports whether a Zenoh-style key expression matches a concrete key.
//
// Keys are '/'-delimited chunks (e.g. "cerberus/site-a/telemetry/<peer>").
// The expression supports two wildcards, mirroring Zenoh key-expr semantics:
//
//	*   matches exactly one chunk
//	**  matches zero or more chunks
//
// A plain expression with no wildcards matches only the identical key.
func MatchKey(expr, key string) bool {
	return matchChunks(splitKey(expr), splitKey(key))
}

func splitKey(s string) []string {
	s = strings.Trim(s, "/")
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

// matchChunks is a standard wildcard matcher with backtracking for "**".
func matchChunks(pat, key []string) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case "**":
			// "**" at the end matches the remainder (including empty).
			if len(pat) == 1 {
				return true
			}
			// Try to consume zero or more key chunks, then match the rest.
			for i := 0; i <= len(key); i++ {
				if matchChunks(pat[1:], key[i:]) {
					return true
				}
			}
			return false
		case "*":
			if len(key) == 0 {
				return false
			}
			pat, key = pat[1:], key[1:]
		default:
			if len(key) == 0 || pat[0] != key[0] {
				return false
			}
			pat, key = pat[1:], key[1:]
		}
	}
	return len(key) == 0
}
