package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u:s3cret@h/db":              "postgres://u:xxxxx@h/db",
		"postgres://u@h/db?password=s3cret&x=1": "postgres://u@h/db?password=xxxxx&x=1",
		"/data/mnemo.db":                        "/data/mnemo.db",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

// Usage output prints flag defaults, so secrets from the environment must not be defaults.
func TestUsageHidesEnvSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	cmd := exec.Command("go", "run", ".", "hub", "--no-such-flag")
	cmd.Env = append(cmd.Environ(), "MNEMO_DB=postgres://u:s3cret@h/db", "MNEMO_TOKEN=s3cret", "MNEMO_OIDC_CLIENT_SECRET=s3cret")
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "s3cret") || !strings.Contains(string(out), "-db") {
		t.Errorf("usage output:\n%s", out)
	}
}
