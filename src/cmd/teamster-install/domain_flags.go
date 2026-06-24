package main

import (
	"fmt"
	"strings"
)

// isStaleLocalhostURL reports whether v is a localhost or 127.0.0.1 URL —
// the signal that a prior install baked in the old default that must be
// replaced with a hostname-based URL on reinstall. A real hostname or FQDN
// the operator deliberately set is preserved (returns false).
func isStaleLocalhostURL(v string) bool {
	return strings.HasPrefix(v, "http://localhost:") ||
		strings.HasPrefix(v, "http://127.0.0.1:")
}

// domainConfig carries the domain-named server flags from the CLI to the
// merge step. Empty string = flag not given.
type domainConfig struct {
	prometheusEndpoint string // --prometheus-endpoint
	otelcolEndpoint    string // --otelcol-endpoint
	grafanaEndpoint    string // --grafana-endpoint
	hookdEndpoint      string // --hookd-endpoint
}

// portConfig carries the installer-discovered free ports for each service.
// Zero means "not probed" (service mode != install).
// hookServerURL is the full http://localhost:<port>/event URL computed from
// findFreePort(9125); threaded into hookd's installURL so the domain
// dispatch (case 2) overwrites a stale TEAMSTER_HOOK_SERVER_URL on reinstall.
type portConfig struct {
	prometheus    int
	grafana       int
	otelGRPC      int
	hookServerURL string
}

// modeConfig carries the per-service mode flags to the domain dispatch.
type modeConfig struct {
	hookdMode      string // systemd | supervisor | external
	otelcolMode    string // install | external | managed | none
	prometheusMode string // install | external | managed | none
	grafanaMode    string // install | external | managed | none
}

// domainServiceSpec binds one CLI flag to its settings.json key and mode.
// `settingsKey == ""` means the service is plumbed informationally but does not
// land in settings.json (prometheus + grafana; their config flows through scrape
// configs / datasource JSON in P6, not env vars).
type domainServiceSpec struct {
	name        string // log-friendly: "otel", "prometheus", "grafana", "hookd"
	flagName    string // CLI flag (e.g. "otelcol-endpoint", "hookd-endpoint"); used in WARN messages
	flagValue   string // raw value from CLI (empty if not given)
	settingsKey string // env key in settings.json; empty if not applicable
	mode        string // service mode: install | external | managed | none
	installURL  string // local URL when mode=install; written when case=install-default
	normalize   func(string) string
}

func domainSpecs(cfg domainConfig, modes modeConfig, ports portConfig) []domainServiceSpec {
	otelInstallURL := "http://localhost:4327"
	if ports.otelGRPC != 0 {
		otelInstallURL = fmt.Sprintf("http://localhost:%d", ports.otelGRPC)
	}
	promInstallURL := "http://localhost:9190"
	if ports.prometheus != 0 {
		promInstallURL = fmt.Sprintf("http://localhost:%d", ports.prometheus)
	}
	grafanaInstallURL := "http://localhost:3100"
	if ports.grafana != 0 {
		grafanaInstallURL = fmt.Sprintf("http://localhost:%d", ports.grafana)
	}
	return []domainServiceSpec{
		{
			name:        "otel",
			flagName:    "otelcol-endpoint",
			flagValue:   cfg.otelcolEndpoint,
			settingsKey: "OTEL_EXPORTER_OTLP_ENDPOINT",
			mode:        modes.otelcolMode,
			installURL:  otelInstallURL,
			normalize:   normalizeURL,
		},
		{
			name:        "hookd",
			flagName:    "hookd-endpoint",
			flagValue:   cfg.hookdEndpoint,
			settingsKey: "TEAMSTER_HOOK_SERVER_URL",
			// hookd treats systemd/supervisor as "install" for domain dispatch:
			// both mean the hookd runs locally and the URL should be written.
			mode:       hookdDomainMode(modes.hookdMode),
			installURL: ports.hookServerURL,
			normalize:  normalizeTeamsterURL,
		},
		{
			name:        "prometheus",
			flagName:    "prometheus-endpoint",
			flagValue:   cfg.prometheusEndpoint,
			settingsKey: "", // plumb only — P6 handles scrape config
			mode:        modes.prometheusMode,
			installURL:  promInstallURL,
			normalize:   normalizeURL,
		},
		{
			name:        "grafana",
			flagName:    "grafana-endpoint",
			flagValue:   cfg.grafanaEndpoint,
			settingsKey: "", // plumb only — P6 handles datasource JSON
			mode:        modes.grafanaMode,
			installURL:  grafanaInstallURL,
			normalize:   normalizeURL,
		},
	}
}

// hookdDomainMode maps hookd mode values to the domain dispatch vocabulary.
// systemd and supervisor both mean hookd runs locally → "install".
// external means the hook client points at a remote hub → "external".
func hookdDomainMode(mode string) string {
	switch mode {
	case "systemd", "supervisor":
		return "install"
	default:
		return mode
	}
}

// normalizeURL turns bare `host:port` into `http://host:port`. Idempotent on
// already-schemed values. Empty input passes through empty.
func normalizeURL(v string) string {
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		return v
	}
	return "http://" + v
}

// normalizeTeamsterURL adds /event path on top of normalizeURL, idempotently.
// Bare host:port → http://host:port/event. URL without path → URL+/event.
// URL already ending in /event → unchanged.
func normalizeTeamsterURL(v string) string {
	if v == "" {
		return ""
	}
	v = normalizeURL(v)
	if strings.HasSuffix(v, "/event") {
		return v
	}
	// If there's already SOME path (not just `://host`), don't append.
	// Detect by counting `/` after the scheme — a scheme has exactly two,
	// then bare host has none, then path adds more.
	rest := strings.TrimPrefix(strings.TrimPrefix(v, "http://"), "https://")
	if strings.Contains(rest, "/") {
		return v // already has a path (other than just /event); trust the caller
	}
	return v + "/event"
}

// applyDomainServer encodes the 4-case decision tree:
//
//  1. Override:         flag present              → write flag-value
//  2. Install default:  flag absent, mode=install → write install URL
//  3. Invariant skip:   flag absent, managed/external, key present → skip+WARN
//  4. No-op:            flag absent, mode=none, key absent → do nothing
//
// Case 1+2 combo (flag override despite mode=install) emits a WARN, flag wins.
// Services with empty settingsKey (prometheus, grafana) are plumbed
// informationally but never touch env.
func applyDomainServer(env map[string]interface{}, spec domainServiceSpec) {
	hasFlag := spec.flagValue != ""
	isInstall := spec.mode == "install"

	// Plumb-only services (no settingsKey) just log; never write env.
	if spec.settingsKey == "" {
		switch {
		case hasFlag:
			dlog("INFO", "teamster-install.merge", "domain plumb (external)",
				"service", spec.name,
				"flag_value", spec.normalize(spec.flagValue),
				"action", "plumb-external",
			)
		case isInstall:
			dlog("INFO", "teamster-install.merge", "domain plumb (install)",
				"service", spec.name,
				"install_url", spec.installURL,
				"action", "plumb-install",
			)
		default:
			dlog("INFO", "teamster-install.merge", "domain plumb (absent)",
				"service", spec.name,
				"action", "no-op",
			)
		}
		return
	}

	existing, exists := env[spec.settingsKey]
	existingStr := stringifyEnvVal(existing, exists)

	switch {
	case hasFlag:
		final := spec.normalize(spec.flagValue)
		level := "INFO"
		warnExtra := ""
		if isInstall {
			level = "WARN"
			warnExtra = fmt.Sprintf("mode=install for %s but %s will point at %s per --%s override",
				spec.name, spec.settingsKey, final, spec.flagName)
		}
		env[spec.settingsKey] = final
		dlog(level, "teamster-install.merge", "domain server",
			"service", spec.name,
			"key", spec.settingsKey,
			"existing", existingStr,
			"flag_value", spec.flagValue,
			"final", final,
			"action", "override",
		)
		if warnExtra != "" {
			dlog("WARN", "teamster-install.merge", warnExtra,
				"service", spec.name,
				"mode", spec.mode,
			)
		}
	case isInstall:
		if spec.installURL == "" {
			// installURL is installer-computed (hookd via findFreePort); deferred
			// to caller's fallback. Treat as no-op here.
			dlog("INFO", "teamster-install.merge", "domain server",
				"service", spec.name,
				"key", spec.settingsKey,
				"existing", existingStr,
				"flag_value", "<absent>",
				"final", existingStr,
				"action", "install-deferred",
			)
			return
		}
		// Preserve a non-stale existing value: if the operator already has a
		// real hostname or FQDN set (not localhost/127.0.0.1), keep it.
		// Replace stale localhost values so reinstall heals the old default.
		if exists && !isStaleLocalhostURL(existingStr) {
			dlog("INFO", "teamster-install.merge", "domain server",
				"service", spec.name,
				"key", spec.settingsKey,
				"existing", existingStr,
				"flag_value", "<absent>",
				"final", existingStr,
				"action", "install-preserve",
			)
			return
		}
		env[spec.settingsKey] = spec.installURL
		dlog("INFO", "teamster-install.merge", "domain server",
			"service", spec.name,
			"key", spec.settingsKey,
			"existing", existingStr,
			"flag_value", "<absent>",
			"final", spec.installURL,
			"action", "install-default",
		)
	case exists:
		dlog("WARN", "teamster-install.merge", "domain server: existing value preserved (no flag, no install mode — tell the user)",
			"service", spec.name,
			"key", spec.settingsKey,
			"existing", existingStr,
			"flag_value", "<absent>",
			"final", existingStr,
			"action", "skip-invariant",
		)
	default:
		dlog("INFO", "teamster-install.merge", "domain server",
			"service", spec.name,
			"key", spec.settingsKey,
			"existing", "<absent>",
			"flag_value", "<absent>",
			"final", "<absent>",
			"action", "no-op",
		)
	}
}

// removeLegacyKey deletes a settings.json env key always — used for
// CLAUDE_HOOK_SERVER per [[pre-release-state]] (no backward compat).
func removeLegacyKey(env map[string]interface{}, key string) {
	if existing, exists := env[key]; exists {
		dlog("INFO", "teamster-install.merge", "legacy key removed",
			"key", key,
			"existing", stringifyEnvVal(existing, exists),
			"action", "delete-legacy",
		)
		delete(env, key)
	} else {
		dlog("INFO", "teamster-install.merge", "legacy key already absent",
			"key", key,
			"action", "no-op",
		)
	}
}
