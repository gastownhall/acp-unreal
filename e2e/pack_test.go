package e2e

import (
	"slices"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

type packOption struct {
	Key          string   `toml:"key"`
	Default      string   `toml:"default"`
	FlagTemplate []string `toml:"flag_template"`
	Choices      []struct {
		Value    string   `toml:"value"`
		FlagArgs []string `toml:"flag_args"`
	} `toml:"choices"`
}

type packFile struct {
	Providers map[string]struct {
		SupportsACP   bool         `toml:"supports_acp"`
		PromptMode    string       `toml:"prompt_mode"`
		ACPArgs       []string     `toml:"acp_args"`
		OptionsSchema []packOption `toml:"options_schema"`
	} `toml:"providers"`
}

func loadPack(t *testing.T) (packFile, map[string]packOption) {
	t.Helper()
	var p packFile
	if _, err := toml.DecodeFile("../pack/pack.toml", &p); err != nil {
		t.Fatal(err)
	}
	options := map[string]packOption{}
	for _, o := range p.Providers["unreal"].OptionsSchema {
		options[o.Key] = o
	}
	return p, options
}

// gc appends a session's initial message to the launch command unless
// prompt_mode is "none"; acp-unreal takes prompts only over ACP.
func TestPackDeliversInitialMessageOverACP(t *testing.T) {
	p, _ := loadPack(t)
	unreal := p.Providers["unreal"]
	if !unreal.SupportsACP || unreal.PromptMode != "none" {
		t.Fatalf("providers.unreal supports_acp=%v prompt_mode=%q, want true and \"none\"", unreal.SupportsACP, unreal.PromptMode)
	}
}

// The allowlist choice is useless without a way to pass --allow, and ask /
// allowlist hang forever under a gc that never answers permission requests
// unless the timeout is finite by default.
func TestPackPermissionOptions(t *testing.T) {
	_, options := loadPack(t)
	allow, ok := options["allow"]
	if !ok || !slices.Equal(allow.FlagTemplate, []string{"--allow", "{value}"}) || allow.Default != "" {
		t.Fatalf("allow option = %+v, want an open option rendering --allow {value}, default unset", allow)
	}
	timeout, ok := options["permission_timeout"]
	if !ok || !slices.Equal(timeout.FlagTemplate, []string{"--permission-timeout", "{value}"}) {
		t.Fatalf("permission_timeout option = %+v", timeout)
	}
	if d, err := time.ParseDuration(timeout.Default); err != nil || d <= 0 {
		t.Fatalf("permission_timeout default %q must be a finite duration", timeout.Default)
	}
	var modes []string
	for _, c := range options["permission_mode"].Choices {
		modes = append(modes, c.Value)
	}
	if !slices.Equal(modes, []string{"auto", "ask", "allowlist"}) {
		t.Fatalf("permission_mode choices = %v", modes)
	}
}

// Every flag the pack can render is one acp-unreal accepts.
func TestPackFlagsAreAccepted(t *testing.T) {
	p, options := loadPack(t)
	args := slices.Clone(p.Providers["unreal"].ACPArgs)
	for _, o := range options {
		for _, c := range o.Choices {
			args = append(args, c.FlagArgs...)
		}
		if len(o.FlagTemplate) == 2 {
			value := "x"
			if o.Key == "permission_timeout" {
				value = "90s"
			}
			args = append(args, o.FlagTemplate[0], value)
		}
	}
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: args})
	a.initialize()
	a.newSession()
	a.stop()
}
