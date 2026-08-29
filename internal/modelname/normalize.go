// Package modelname normalizes Ollama model names so that the same model is
// counted once.
//
// Ollama treats "llama3" and "llama3:latest" as the same model, but the proxy
// records whatever string the client sent — so one client's shorthand and
// another's explicit tag split every aggregate (and every cost row) in two.
// Normalizing at record time keeps per-model statistics comparable.
package modelname

import (
	"fmt"
	"strings"
)

// Unknown is the placeholder recorded when a request names no model.
const Unknown = "unknown"

// defaultTag is the tag Ollama implies when a name carries none.
const defaultTag = "latest"

// ParseAliases parses a comma-separated "alias=target" list, as accepted by the
// -model-aliases flag. An empty string yields a nil map.
func ParseAliases(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		alias, target, ok := strings.Cut(pair, "=")
		alias, target = strings.TrimSpace(alias), strings.TrimSpace(target)
		if !ok || alias == "" || target == "" {
			return nil, fmt.Errorf("invalid model alias %q: want alias=target", pair)
		}
		out[alias] = target
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Normalize resolves model through aliases (one hop) and appends the implicit
// ":latest" tag when the name carries none. Empty names and Unknown are returned
// untouched, as are names whose final path segment is already tagged.
func Normalize(model string, aliases map[string]string) string {
	m := strings.TrimSpace(model)
	if m == "" || m == Unknown {
		return m
	}
	if target, ok := aliases[m]; ok {
		m = strings.TrimSpace(target)
	}
	// A registry host may carry a port ("registry:5000/ns/model"), so only the
	// last path segment decides whether a tag is present.
	base := m[strings.LastIndex(m, "/")+1:]
	if strings.Contains(base, ":") {
		return m
	}
	return m + ":" + defaultTag
}
