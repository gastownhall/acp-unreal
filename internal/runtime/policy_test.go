package runtime

import "testing"

func TestAllowlisted(t *testing.T) {
	prefixes := []string{"echo", "git status", " "}
	cases := map[string]bool{
		"echo hi":            true,
		"echo":               true,
		"  git status  ":     true,
		"git status --short": true,
		"echoo hi":           false,
		"git stash":          false,
		"echo x; rm -rf ~":   false,
		"echo $(id)":         false,
		"echo a && rm b":     false,
		"echo a | sh":        false,
		"echo a > /etc/x":    false,
		"echo `id`":          false,
		"echo a\nrm b":       false,
		"rm -rf /":           false,
	}
	for command, want := range cases {
		if got := allowlisted(command, prefixes); got != want {
			t.Errorf("allowlisted(%q) = %v, want %v", command, got, want)
		}
	}
}
