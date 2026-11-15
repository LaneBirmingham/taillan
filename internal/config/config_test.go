package config

import (
	"strings"
	"testing"
)

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"  Lane's   server ":    "Lane's server",
		"":                      "",
		"   ":                   "",
		strings.Repeat("é", 50): strings.Repeat("é", 40),
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultNameSkipsContainerIDs(t *testing.T) {
	if !containerID.MatchString("3f4e8a9b2c1d") || containerID.MatchString("lanes-pc") {
		t.Error("container id pattern")
	}
	if n := defaultName(); n == "" || systemUsers[strings.ToLower(n)] {
		t.Errorf("defaultName() = %q", n)
	}
}
