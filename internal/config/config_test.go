package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func valid() map[string]string {
	return map[string]string{
		EnvInstanceID:   "prod",
		EnvScaleSetName: "orpheus",
		EnvRepositories: `[{"repository":"tim/a"},{"repository":"tim/b","name":"big","image":"ghcr.io/tim/runner:1"}]`,
		EnvToken:        "ghp_secretsecretsecret",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, errs := Load(env(valid()))
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.Slots != 1 || c.RunnerNanoCPUs != 2e9 || c.RunnerMemory != 4<<30 {
		t.Fatalf("bad limits: %+v", c)
	}
	if c.ShutdownTimeout != 5*time.Minute || c.HealthThreshold != 120*time.Second ||
		c.ReadinessTimeout != 120*time.Second || c.RefreshInterval != 7*24*time.Hour {
		t.Fatalf("bad durations: %+v", c)
	}
	if c.Repos[0].Name != "orpheus" || c.Repos[1].Name != "big" {
		t.Fatalf("bad names: %+v", c.Repos)
	}
	if c.ImageFor(c.Repos[0]) != "scaleset-runner/runner:prod" || c.ImageFor(c.Repos[1]) != "ghcr.io/tim/runner:1" {
		t.Fatalf("bad images")
	}
	if c.InstanceLabel() != "scaleset-runner-prod" {
		t.Fatalf("bad label %q", c.InstanceLabel())
	}
}

func TestLoadNamesEveryProblem(t *testing.T) {
	m := map[string]string{
		EnvInstanceID:   "Bad ID",
		EnvRepositories: `[{"repository":"tim/a"},{"repository":"TIM/A"},{"repository":"nope"}]`,
		EnvToken:        "ghp_x",
		EnvAppClientID:  "Iv1.123",
		EnvSlots:        "33",
		EnvLogLevel:     "loud",
		EnvRunnerMemory: "1m",
	}
	_, errs := Load(env(m))
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	for _, want := range []string{EnvInstanceID, "listed more than once", "\"owner/name\"", "no Scale Set name",
		EnvToken, EnvSlots, EnvLogLevel, EnvRunnerMemory} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "ghp_x") {
		t.Errorf("credential leaked into error text")
	}
}

func TestAppCredentials(t *testing.T) {
	m := valid()
	delete(m, EnvToken)
	m[EnvAppClientID] = "Iv1.abc"
	m[EnvAppInstallationID] = "42"
	m[EnvAppPrivateKey] = `-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----`
	c, errs := Load(env(m))
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.AppInstallationID != 42 || !strings.Contains(c.AppPrivateKey, "\nabc\n") {
		t.Fatalf("bad app config: %+v", c)
	}

	delete(m, EnvAppClientID)
	if _, errs := Load(env(m)); len(errs) != 1 || !strings.Contains(errs[0].Error(), EnvAppClientID) {
		t.Fatalf("expected missing client id error, got %v", errs)
	}
}
