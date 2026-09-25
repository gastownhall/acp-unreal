package sweep

import (
	"os"
	"strings"
	"testing"
)

func environ(entries ...string) []byte {
	return []byte(strings.Join(entries, "\x00") + "\x00")
}

func TestMatches(t *testing.T) {
	m := Marker{token: "aa"}
	gc := Marker{token: "aa", gcSession: "gc-1"}
	for _, tc := range []struct {
		name string
		m    Marker
		env  []byte
		want bool
	}{
		{"own token", m, environ("PATH=/bin", OwnersEnv+"=aa"), true},
		{"token among ancestors", m, environ(OwnersEnv + "=zz:aa:bb"), true},
		{"other agent", m, environ(OwnersEnv + "=zz:bb"), false},
		{"token prefix only", m, environ(OwnersEnv + "=aaa"), false},
		{"no marker", m, environ("PATH=/bin"), false},
		{"zombie (empty environ)", m, nil, false},
		{"gc: same session", gc, environ(OwnersEnv+"=aa", "GC_SESSION_ID=gc-1"), true},
		{"gc: identity stripped (city infrastructure)", gc, environ(OwnersEnv + "=aa"), false},
		{"gc: other session", gc, environ(OwnersEnv+"=aa", "GC_SESSION_ID=gc-2"), false},
		{"gc: session without token", gc, environ("GC_SESSION_ID=gc-1"), false},
		{"unmarked agent matches nothing", Marker{}, environ(OwnersEnv + "="), false},
	} {
		if got := tc.m.Matches(tc.env); got != tc.want {
			t.Errorf("%s: Matches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMarkAppendsToInheritedOwners(t *testing.T) {
	t.Setenv(OwnersEnv, "parent")
	t.Setenv(gcSessionEnv, "gc-7")
	m, err := Mark()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.token) != 32 || m.gcSession != "gc-7" {
		t.Fatalf("marker = %+v", m)
	}
	owners := os.Getenv(OwnersEnv)
	if owners != "parent:"+m.token {
		t.Fatalf("%s = %q, want the parent's tokens kept", OwnersEnv, owners)
	}
	if !m.Matches(environ(OwnersEnv+"="+owners, "GC_SESSION_ID=gc-7")) {
		t.Fatal("a tool's inherited environment must match")
	}
}
