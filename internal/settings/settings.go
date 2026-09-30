// Package settings implements the env-var override convention: any setting
// may be pinned via a MAMRATIO_SETTING_<KEY> environment variable, which
// then takes precedence over the persisted value and is reported to the UI
// as "env managed" so it can be rendered read-only.
package settings

import (
	"os"
	"strconv"
	"strings"

	"github.com/miista/mam-ratio/internal/store"
)

const envPrefix = "MAMRATIO_SETTING_"

// Field describes one overridable setting.
type Field struct {
	Key            string
	EnvVar         string
	EnvOverridable bool
}

// definitions is the full list of top-level settings that participate in
// the env-override mechanism. Nested settings (search filters, download
// client) are not individually env-overridable in this first pass — only
// mam_id (the highest-value secret) and the run-cadence knobs are.
var definitions = []Field{
	{Key: "mam_id", EnvOverridable: true},
	{Key: "reserve", EnvOverridable: true},
	{Key: "next_run_delay_minutes", EnvOverridable: true},
	{Key: "max_add_per_run", EnvOverridable: true},
}

func init() {
	for i := range definitions {
		definitions[i].EnvVar = envPrefix + strings.ToUpper(definitions[i].Key)
	}
}

// Definitions returns the full list of setting field definitions.
func Definitions() []Field {
	return definitions
}

// EnvManaged reports whether key is currently overridden by an environment
// variable, and returns the raw string value if so.
func EnvManaged(key string) (value string, envVar string, managed bool) {
	for _, f := range definitions {
		if f.Key != key || !f.EnvOverridable {
			continue
		}
		if v, ok := os.LookupEnv(f.EnvVar); ok {
			return v, f.EnvVar, true
		}
		return "", f.EnvVar, false
	}
	return "", "", false
}

// Resolved is the effective settings view: persisted values with any
// env-var overrides applied, plus per-field metadata for the UI.
type Resolved struct {
	Settings store.Settings
	Managed  map[string]string // key -> env var name, only present when env-managed
}

// Resolve merges persisted settings with any active env overrides.
func Resolve(persisted store.Settings) Resolved {
	r := Resolved{Settings: persisted, Managed: map[string]string{}}

	apply := func(key string, assign func(string)) {
		if v, envVar, ok := EnvManaged(key); ok {
			assign(v)
			r.Managed[key] = envVar
		}
	}

	apply("mam_id", func(v string) { r.Settings.MamID = v })
	apply("reserve", func(v string) { r.Settings.Reserve = parseInt(v, r.Settings.Reserve) })
	apply("next_run_delay_minutes", func(v string) { r.Settings.NextRunDelayMinutes = parseInt(v, r.Settings.NextRunDelayMinutes) })
	apply("max_add_per_run", func(v string) { r.Settings.MaxAddPerRun = parseInt(v, r.Settings.MaxAddPerRun) })

	return r
}

// IsManaged reports whether the given key is currently env-managed in this
// resolved view.
func (r Resolved) IsManaged(key string) bool {
	_, ok := r.Managed[key]
	return ok
}

func parseInt(v string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return n
}

// AuthDisabled reports whether MAMRATIO_AUTH_DISABLED is set truthily.
func AuthDisabled() bool {
	v, ok := os.LookupEnv("MAMRATIO_AUTH_DISABLED")
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && b
}

// MaskSecret returns a masked representation of a secret value, showing
// only the last 4 characters, e.g. "••••••1234". Empty values are returned
// empty.
func MaskSecret(value string) string {
	if value == "" {
		return ""
	}
	const visible = 4
	if len(value) <= visible {
		return strings.Repeat("•", len(value))
	}
	return strings.Repeat("•", len(value)-visible) + value[len(value)-visible:]
}
