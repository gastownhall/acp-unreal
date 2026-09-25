package credenv

import (
	"slices"
	"testing"
)

func TestIsCredentialName(t *testing.T) {
	for name, want := range map[string]bool{
		"OPENAI_API_KEY":         true,
		"OLLAMA_API_KEY2":        true,
		"openrouter_apikey":      true,
		"GITHUB_TOKEN":           true,
		"AWS_SECRET_ACCESS_KEY":  true,
		"AWS_ACCESS_KEY_ID":      true,
		"DB_PASSWORD":            true,
		"MY_SECRET":              true,
		"TOKEN":                  true,
		"PATH":                   false,
		"HOME":                   false,
		"KEYBOARD_LAYOUT":        false,
		"TOKENIZERS_PARALLELISM": false,
		"MONKEY":                 false,
		"API_URL":                false,
		"SSH_AUTH_SOCK":          false,
	} {
		if got := IsCredentialName(name); got != want {
			t.Errorf("IsCredentialName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestScrub(t *testing.T) {
	env := []string{
		"PATH=/bin",
		"ACP_UNREAL_API_KEY=k1",
		"OPENAI_API_KEY=k2",
		"OLLAMA_API_KEY2=k3",
		"GC_INSTANCE_TOKEN=gc",
		"BEADS_HOLDER_TOKEN=beads",
		"GITHUB_TOKEN=gh",
		"CUSTOM_CRED=c",
		"KEEP_ME_TOKEN=keep",
		"GC_PROVIDER_KEY=explicit",
		"NOVALUE",
	}
	p := Policy{
		Explicit: []string{"ACP_UNREAL_API_KEY", "CUSTOM_CRED", "GC_PROVIDER_KEY"},
		Keep:     []string{"KEEP_ME_TOKEN"},
	}
	kept, removed := p.Scrub(env)
	wantKept := []string{"PATH=/bin", "GC_INSTANCE_TOKEN=gc", "BEADS_HOLDER_TOKEN=beads", "KEEP_ME_TOKEN=keep", "NOVALUE"}
	if !slices.Equal(kept, wantKept) {
		t.Errorf("kept = %q, want %q", kept, wantKept)
	}
	wantRemoved := []string{"ACP_UNREAL_API_KEY", "OPENAI_API_KEY", "OLLAMA_API_KEY2", "GITHUB_TOKEN", "CUSTOM_CRED", "GC_PROVIDER_KEY"}
	if !slices.Equal(removed, wantRemoved) {
		t.Errorf("removed = %q, want %q", removed, wantRemoved)
	}
}

func TestDefaultExplicitNames(t *testing.T) {
	p := NewPolicy("MY_KEY_VAR", []string{"EXTRA"}, nil)
	for _, name := range []string{"MY_KEY_VAR", "EXTRA", "UNREAL_HARNESS_LLM_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "OLLAMA_API_KEY"} {
		if !slices.Contains(p.Explicit, name) {
			t.Errorf("explicit list lacks %s: %q", name, p.Explicit)
		}
	}
}
