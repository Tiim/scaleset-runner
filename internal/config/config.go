// Package config reads and validates the Controller's Config from environment
// variables (FR-019, FR-020).
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	units "github.com/docker/go-units"
)

// Environment variable names.
const (
	EnvInstanceID        = "SCALESET_INSTANCE_ID"
	EnvRepositories      = "SCALESET_REPOSITORIES"
	EnvScaleSetName      = "SCALESET_NAME"
	EnvAppClientID       = "SCALESET_GITHUB_APP_CLIENT_ID"
	EnvAppInstallationID = "SCALESET_GITHUB_APP_INSTALLATION_ID"
	EnvAppPrivateKey     = "SCALESET_GITHUB_APP_PRIVATE_KEY"
	EnvToken             = "SCALESET_GITHUB_TOKEN"
	EnvSlots             = "SCALESET_SLOTS"
	EnvRunnerCPUs        = "SCALESET_RUNNER_CPUS"
	EnvRunnerMemory      = "SCALESET_RUNNER_MEMORY"
	EnvShutdownTimeout   = "SCALESET_SHUTDOWN_TIMEOUT"
	EnvHealthThreshold   = "SCALESET_HEALTH_THRESHOLD"
	EnvReadinessTimeout  = "SCALESET_READINESS_TIMEOUT"
	EnvRefreshDays       = "SCALESET_REFRESH_INTERVAL_DAYS"
	EnvStatePath         = "SCALESET_STATE_PATH"
	EnvListenAddr        = "SCALESET_LISTEN_ADDR"
	EnvLogLevel          = "SCALESET_LOG_LEVEL"
	EnvRuntime           = "SCALESET_RUNTIME"
)

// Repo is one configured repository with its resolved Scale Set name.
type Repo struct {
	// Repository is "owner/name".
	Repository string `json:"repository"`
	// Name overrides the global Scale Set name (FR-005).
	Name string `json:"name,omitempty"`
	// Image overrides the default Runner Image (FR-027); empty means default.
	Image string `json:"image,omitempty"`
}

// URL returns the repository URL the scaleset client registers on.
func (r Repo) URL() string { return "https://github.com/" + r.Repository }

type Config struct {
	InstanceID   string
	Repos        []Repo
	ScaleSetName string

	AppClientID       string
	AppInstallationID int64
	AppPrivateKey     string
	Token             string

	Slots            int
	RunnerNanoCPUs   int64
	RunnerMemory     int64
	ShutdownTimeout  time.Duration
	HealthThreshold  time.Duration
	ReadinessTimeout time.Duration
	RefreshInterval  time.Duration
	StatePath        string
	ListenAddr       string
	LogLevel         slog.Level
	Runtime          string
}

// InstanceLabel is the label the Controller adds to every Scale Set it creates.
func (c *Config) InstanceLabel() string { return "scaleset-runner-" + c.InstanceID }

// DefaultImage is the tag of the default Runner Image this Controller builds.
func (c *Config) DefaultImage() string { return "scaleset-runner/runner:" + c.InstanceID }

// ImageFor returns the Runner Image reference a repository uses.
func (c *Config) ImageFor(r Repo) string {
	if r.Image != "" {
		return r.Image
	}
	return c.DefaultImage()
}

// UsesDefaultImage reports whether any repository uses the default image.
func (c *Config) UsesDefaultImage() bool {
	for _, r := range c.Repos {
		if r.Image == "" {
			return true
		}
	}
	return false
}

// Secrets returns the credential values that must never be logged.
func (c *Config) Secrets() []string {
	var s []string
	if c.Token != "" {
		s = append(s, c.Token)
	}
	if c.AppPrivateKey != "" {
		s = append(s, c.AppPrivateKey)
	}
	return s
}

var (
	instanceIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	repoRe       = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
	nameRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
)

// Load reads the Config through getenv. It returns every problem it finds,
// each naming the setting, so the Operator can fix them all at once (UC-001 A1).
// Error texts never contain credential values.
func Load(getenv func(string) string) (*Config, []error) {
	var errs []error
	bad := func(env, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", env, fmt.Sprintf(format, args...)))
	}
	c := &Config{}

	c.InstanceID = strings.TrimSpace(getenv(EnvInstanceID))
	switch {
	case c.InstanceID == "":
		bad(EnvInstanceID, "is required")
	case !instanceIDRe.MatchString(c.InstanceID):
		bad(EnvInstanceID, "must be 1-40 lowercase letters, digits or dashes, starting with a letter or digit")
	}

	c.ScaleSetName = strings.TrimSpace(getenv(EnvScaleSetName))
	if c.ScaleSetName != "" && !nameRe.MatchString(c.ScaleSetName) {
		bad(EnvScaleSetName, "must be 1-100 letters, digits, dots, underscores or dashes")
	}

	if raw := strings.TrimSpace(getenv(EnvRepositories)); raw == "" {
		bad(EnvRepositories, "is required")
	} else {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c.Repos); err != nil {
			bad(EnvRepositories, "must be a JSON array of {\"repository\":\"owner/name\",\"name\":\"…\",\"image\":\"…\"}: %v", err)
		} else if len(c.Repos) == 0 {
			bad(EnvRepositories, "must list at least one repository")
		}
	}
	seen := map[string]bool{}
	for i := range c.Repos {
		r := &c.Repos[i]
		r.Repository = strings.TrimSpace(r.Repository)
		r.Name = strings.TrimSpace(r.Name)
		r.Image = strings.TrimSpace(r.Image)
		where := fmt.Sprintf("entry %d (%q)", i, r.Repository)
		if !repoRe.MatchString(r.Repository) {
			bad(EnvRepositories, "%s: repository must be \"owner/name\"", where)
		}
		key := strings.ToLower(r.Repository)
		if seen[key] {
			bad(EnvRepositories, "%s: repository is listed more than once", where)
		}
		seen[key] = true
		if r.Name == "" {
			if c.ScaleSetName == "" {
				bad(EnvRepositories, "%s: has no Scale Set name and %s is not set", where, EnvScaleSetName)
			}
			r.Name = c.ScaleSetName
		} else if !nameRe.MatchString(r.Name) {
			bad(EnvRepositories, "%s: name must be 1-100 letters, digits, dots, underscores or dashes", where)
		}
		if r.Image != "" && strings.ContainsAny(r.Image, " \t\n") {
			bad(EnvRepositories, "%s: image must be an image reference", where)
		}
	}

	// Credentials: exactly one of GitHub App or PAT (UC-001 BR-004).
	c.AppClientID = strings.TrimSpace(getenv(EnvAppClientID))
	installation := strings.TrimSpace(getenv(EnvAppInstallationID))
	c.AppPrivateKey = normalizeKey(getenv(EnvAppPrivateKey))
	c.Token = strings.TrimSpace(getenv(EnvToken))
	hasApp := c.AppClientID != "" || installation != "" || c.AppPrivateKey != ""
	switch {
	case hasApp && c.Token != "":
		bad(EnvToken, "must not be set together with GitHub App credentials; configure exactly one")
	case !hasApp && c.Token == "":
		bad(EnvToken, "either %s or the GitHub App credentials (%s, %s, %s) are required",
			EnvToken, EnvAppClientID, EnvAppInstallationID, EnvAppPrivateKey)
	case hasApp:
		if c.AppClientID == "" {
			bad(EnvAppClientID, "is required for GitHub App authentication")
		}
		if id, err := strconv.ParseInt(installation, 10, 64); err != nil || id <= 0 {
			bad(EnvAppInstallationID, "must be a positive integer")
		} else {
			c.AppInstallationID = id
		}
		if !strings.Contains(c.AppPrivateKey, "PRIVATE KEY-----") {
			bad(EnvAppPrivateKey, "must be a PEM-encoded private key")
		}
	}

	c.Slots = intSetting(getenv, EnvSlots, 1, 1, 32, bad)

	cpus := strings.TrimSpace(getenv(EnvRunnerCPUs))
	if cpus == "" {
		cpus = "2"
	}
	if f, err := strconv.ParseFloat(cpus, 64); err != nil || f <= 0 || f > 1024 {
		bad(EnvRunnerCPUs, "must be a positive number of CPUs, e.g. 2 or 1.5")
	} else {
		c.RunnerNanoCPUs = int64(f * 1e9)
	}

	mem := strings.TrimSpace(getenv(EnvRunnerMemory))
	if mem == "" {
		mem = "4g"
	}
	if b, err := units.RAMInBytes(mem); err != nil || b < 256*units.MiB {
		bad(EnvRunnerMemory, "must be a memory size of at least 256m, e.g. 4g")
	} else {
		c.RunnerMemory = b
	}

	c.ShutdownTimeout = durationSetting(getenv, EnvShutdownTimeout, 5*time.Minute, bad)
	c.HealthThreshold = durationSetting(getenv, EnvHealthThreshold, 120*time.Second, bad)
	c.ReadinessTimeout = durationSetting(getenv, EnvReadinessTimeout, 120*time.Second, bad)
	c.RefreshInterval = time.Duration(intSetting(getenv, EnvRefreshDays, 7, 1, 48, bad)) * 24 * time.Hour

	c.StatePath = orDefault(getenv(EnvStatePath), "/var/lib/scaleset-runner/state.db")
	c.ListenAddr = orDefault(getenv(EnvListenAddr), ":9090")
	c.Runtime = orDefault(getenv(EnvRuntime), "sysbox-runc")

	switch strings.ToLower(orDefault(getenv(EnvLogLevel), "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn", "warning":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		bad(EnvLogLevel, "must be debug, info, warn or error")
	}

	return c, errs
}

// normalizeKey accepts a PEM key with real newlines or with literal "\n",
// which is how a key often ends up in a one-line environment variable.
func normalizeKey(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "\n") {
		s = strings.ReplaceAll(s, `\n`, "\n")
	}
	return s
}

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v == "" {
		return def
	}
	return v
}

func intSetting(getenv func(string) string, env string, def, lo, hi int, bad func(string, string, ...any)) int {
	raw := strings.TrimSpace(getenv(env))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		bad(env, "must be an integer from %d to %d", lo, hi)
		return def
	}
	return n
}

func durationSetting(getenv func(string) string, env string, def time.Duration, bad func(string, string, ...any)) time.Duration {
	raw := strings.TrimSpace(getenv(env))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		bad(env, "must be a positive duration, e.g. 90s or 5m")
		return def
	}
	return d
}
