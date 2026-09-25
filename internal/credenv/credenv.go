// Package credenv decides which environment variables are credentials that
// the model's tools must not inherit.
//
// A variable is scrubbed when it is named explicitly (the --api-key-env
// variable, the well-known provider key names, --scrub-env), or when its
// name has a credential component (API_KEY, APIKEY, ACCESS_KEY, SECRET,
// PASSWORD, TOKEN; trailing digits ignored, so OLLAMA_API_KEY2 matches).
// GC_* and BEADS_* are kept (tools need GC_INSTANCE_TOKEN and
// BEADS_HOLDER_TOKEN), and so is anything in the keep list. An explicit
// name always wins.
package credenv

import (
	"slices"
	"strings"
)

// WellKnown are provider key variables scrubbed even when not configured.
var WellKnown = []string{"UNREAL_HARNESS_LLM_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "OLLAMA_API_KEY"}

// KeptPrefixes are exempt from the name pattern.
var KeptPrefixes = []string{"GC_", "BEADS_"}

// Policy is a scrub decision rule.
type Policy struct {
	// Explicit names are always scrubbed.
	Explicit []string
	// Keep names are exempt from the name pattern.
	Keep []string
}

// NewPolicy builds the policy for an API key variable plus extra explicit
// and keep lists.
func NewPolicy(apiKeyEnv string, scrub, keep []string) Policy {
	explicit := []string{}
	if apiKeyEnv != "" {
		explicit = append(explicit, apiKeyEnv)
	}
	explicit = append(explicit, WellKnown...)
	explicit = append(explicit, scrub...)
	return Policy{Explicit: explicit, Keep: slices.Clone(keep)}
}

// Scrubbed reports whether the variable name must be removed.
func (p Policy) Scrubbed(name string) bool {
	if slices.Contains(p.Explicit, name) {
		return true
	}
	if slices.Contains(p.Keep, name) {
		return false
	}
	for _, prefix := range KeptPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return IsCredentialName(name)
}

// Scrub splits environ ("NAME=value" entries) into the kept entries and the
// removed names, both in input order.
func (p Policy) Scrub(environ []string) (kept, removed []string) {
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if name != "" && p.Scrubbed(name) {
			removed = append(removed, name)
			continue
		}
		kept = append(kept, entry)
	}
	return kept, removed
}

// IsCredentialName reports whether an underscore-separated component of
// name (case-insensitive, trailing digits ignored) marks a credential.
func IsCredentialName(name string) bool {
	parts := strings.Split(strings.ToUpper(name), "_")
	for i := range parts {
		parts[i] = strings.TrimRight(parts[i], "0123456789")
	}
	for i, part := range parts {
		switch part {
		case "APIKEY", "SECRET", "SECRETS", "PASSWORD", "PASSWD", "TOKEN", "TOKENS":
			return true
		case "KEY":
			if i > 0 && (parts[i-1] == "API" || parts[i-1] == "ACCESS") {
				return true
			}
		}
	}
	return false
}
