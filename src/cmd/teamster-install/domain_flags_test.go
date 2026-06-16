package main

import (
	"testing"
)

// URL normalization tests.

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"localhost:9090", "http://localhost:9090"},
		{"prom.lan:9090", "http://prom.lan:9090"},
		{"http://prom.lan:9090", "http://prom.lan:9090"}, // idempotent on http://
		{"https://prom.lan", "https://prom.lan"},         // idempotent on https://
		{"127.0.0.1:4317", "http://127.0.0.1:4317"},
	}
	for _, c := range cases {
		if got := normalizeURL(c.in); got != c.want {
			t.Errorf("normalizeURL(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeTeamsterURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"localhost:9125", "http://localhost:9125/event"},
		{"hub.lan:9128", "http://hub.lan:9128/event"},
		{"http://hub.lan:9128", "http://hub.lan:9128/event"},
		{"http://hub.lan:9128/event", "http://hub.lan:9128/event"},   // already /event
		{"http://hub.lan:9128/custom", "http://hub.lan:9128/custom"}, // pre-existing path preserved
		{"https://hub.lan", "https://hub.lan/event"},
	}
	for _, c := range cases {
		if got := normalizeTeamsterURL(c.in); got != c.want {
			t.Errorf("normalizeTeamsterURL(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// Decision tree tests — one per case × service.

// --- OTEL service (settings-key-backed) ---

func TestApplyDomainServer_otel_case1_override(t *testing.T) {
	cfg := domainConfig{otelcolEndpoint: "external:4317"}
	modes := modeConfig{otelcolMode: "install"}
	env := map[string]interface{}{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://example-hub:4317"}
	for _, s := range domainSpecs(cfg, modes, portConfig{}) {
		if s.name == "otel" {
			applyDomainServer(env, s)
		}
	}
	if got, want := env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://external:4317"; got != want {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %v; want %v", got, want)
	}
}

func TestApplyDomainServer_otel_case1_override_with_install_warns(t *testing.T) {
	// Both flag AND install mode. Flag wins, WARN expected.
	cfg := domainConfig{otelcolEndpoint: "external:4317"}
	modes := modeConfig{otelcolMode: "install"}
	env := map[string]interface{}{}
	for _, s := range domainSpecs(cfg, modes, portConfig{}) {
		if s.name == "otel" {
			applyDomainServer(env, s)
		}
	}
	if got, want := env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://external:4317"; got != want {
		t.Errorf("flag should win over install mode; got %v want %v", got, want)
	}
}

func TestApplyDomainServer_otel_case2_install_default(t *testing.T) {
	cfg := domainConfig{}
	modes := modeConfig{otelcolMode: "install"}
	env := map[string]interface{}{}
	for _, s := range domainSpecs(cfg, modes, portConfig{}) {
		if s.name == "otel" {
			applyDomainServer(env, s)
		}
	}
	if got, want := env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://localhost:4327"; got != want {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %v; want install default %v", got, want)
	}
}

func TestApplyDomainServer_otel_case3_invariant_skip(t *testing.T) {
	cfg := domainConfig{}
	modes := modeConfig{otelcolMode: "managed"}
	env := map[string]interface{}{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://example-hub:4317"}
	for _, s := range domainSpecs(cfg, modes, portConfig{}) {
		if s.name == "otel" {
			applyDomainServer(env, s)
		}
	}
	if got, want := env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://example-hub:4317"; got != want {
		t.Errorf("existing value should be preserved (case 3); got %v want %v", got, want)
	}
}

func TestApplyDomainServer_otel_case4_noop(t *testing.T) {
	cfg := domainConfig{}
	modes := modeConfig{otelcolMode: "none"}
	env := map[string]interface{}{}
	for _, s := range domainSpecs(cfg, modes, portConfig{}) {
		if s.name == "otel" {
			applyDomainServer(env, s)
		}
	}
	if _, present := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; present {
		t.Error("OTEL key should remain absent (case 4)")
	}
}

// --- HOOKD service (settings-key-backed) ---

func TestApplyDomainServer_hookd_case1_override(t *testing.T) {
	cfg := domainConfig{hookdEndpoint: "hub.lan:9128"}
	modes := modeConfig{hookdMode: "external"}
	env := map[string]interface{}{"TEAMSTER_HOOK_SERVER_URL": "http://example-hub:9125/event"}
	for _, s := range domainSpecs(cfg, modes, portConfig{}) {
		if s.name == "hookd" {
			applyDomainServer(env, s)
		}
	}
	if got, want := env["TEAMSTER_HOOK_SERVER_URL"], "http://hub.lan:9128/event"; got != want {
		t.Errorf("TEAMSTER_HOOK_SERVER_URL = %v; want normalized %v", got, want)
	}
}

func TestApplyDomainServer_hookd_case2_install_writes_hookServerURL(t *testing.T) {
	cfg := domainConfig{}
	modes := modeConfig{hookdMode: "systemd"}
	ports := portConfig{hookServerURL: "http://localhost:9126/event"}
	env := map[string]interface{}{"TEAMSTER_HOOK_SERVER_URL": "http://old-host:9999/event"}
	for _, s := range domainSpecs(cfg, modes, ports) {
		if s.name == "hookd" {
			applyDomainServer(env, s)
		}
	}
	if got, want := env["TEAMSTER_HOOK_SERVER_URL"], "http://localhost:9126/event"; got != want {
		t.Errorf("TEAMSTER_HOOK_SERVER_URL = %v; want install-default %v (stale value must be overwritten)", got, want)
	}
}

func TestApplyDomainServer_hookd_case3_invariant_skip(t *testing.T) {
	cfg := domainConfig{}
	modes := modeConfig{hookdMode: "managed"}
	env := map[string]interface{}{"TEAMSTER_HOOK_SERVER_URL": "http://example-hub:9125/event"}
	for _, s := range domainSpecs(cfg, modes, portConfig{}) {
		if s.name == "hookd" {
			applyDomainServer(env, s)
		}
	}
	if got, want := env["TEAMSTER_HOOK_SERVER_URL"], "http://example-hub:9125/event"; got != want {
		t.Errorf("existing hookd URL should be preserved; got %v want %v", got, want)
	}
}

func TestApplyDomainServer_hookd_case4_noop(t *testing.T) {
	cfg := domainConfig{}
	modes := modeConfig{hookdMode: "none"}
	env := map[string]interface{}{}
	for _, s := range domainSpecs(cfg, modes, portConfig{}) {
		if s.name == "hookd" {
			applyDomainServer(env, s)
		}
	}
	if _, present := env["TEAMSTER_HOOK_SERVER_URL"]; present {
		t.Error("TEAMSTER_HOOK_SERVER_URL should remain absent (case 4)")
	}
}

// --- PROMETHEUS service (plumb-only, no settings key) ---

func TestApplyDomainServer_prometheus_plumb_no_env_writes(t *testing.T) {
	tests := []struct {
		name  string
		cfg   domainConfig
		modes modeConfig
		env   map[string]interface{}
	}{
		{"case1-flag", domainConfig{prometheusEndpoint: "prom.lan:9090"}, modeConfig{prometheusMode: "external"}, map[string]interface{}{}},
		{"case2-install", domainConfig{}, modeConfig{prometheusMode: "install"}, map[string]interface{}{}},
		{"case3-existing", domainConfig{}, modeConfig{prometheusMode: "managed"}, map[string]interface{}{"PROMETHEUS_URL": "http://example-hub:9090"}},
		{"case4-noop", domainConfig{}, modeConfig{prometheusMode: "none"}, map[string]interface{}{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := len(tc.env)
			for _, s := range domainSpecs(tc.cfg, tc.modes, portConfig{}) {
				if s.name == "prometheus" {
					applyDomainServer(tc.env, s)
				}
			}
			if len(tc.env) != before {
				t.Errorf("prometheus is plumb-only, env should not mutate; before=%d after=%d",
					before, len(tc.env))
			}
		})
	}
}

// --- GRAFANA service (plumb-only) ---

func TestApplyDomainServer_grafana_plumb_no_env_writes(t *testing.T) {
	cfg := domainConfig{grafanaEndpoint: "grafana.lan:3000"}
	modes := modeConfig{grafanaMode: "install"}
	env := map[string]interface{}{"GRAFANA_URL": "http://example-hub:3000"}
	for _, s := range domainSpecs(cfg, modes, portConfig{}) {
		if s.name == "grafana" {
			applyDomainServer(env, s)
		}
	}
	// env had GRAFANA_URL before; should still be there, unchanged.
	if got, want := env["GRAFANA_URL"], "http://example-hub:3000"; got != want {
		t.Errorf("grafana is plumb-only; GRAFANA_URL should be untouched; got %v want %v", got, want)
	}
}

// --- removeLegacyKey ---

func TestRemoveLegacyKey_present(t *testing.T) {
	env := map[string]interface{}{"CLAUDE_HOOK_SERVER": "http://example-hub:9125/event", "KEEP_ME": "x"}
	removeLegacyKey(env, "CLAUDE_HOOK_SERVER")
	if _, present := env["CLAUDE_HOOK_SERVER"]; present {
		t.Error("CLAUDE_HOOK_SERVER should have been deleted")
	}
	if env["KEEP_ME"] != "x" {
		t.Error("KEEP_ME should be untouched")
	}
}

func TestRemoveLegacyKey_absent(t *testing.T) {
	env := map[string]interface{}{"OTHER": "value"}
	removeLegacyKey(env, "CLAUDE_HOOK_SERVER")
	if len(env) != 1 || env["OTHER"] != "value" {
		t.Errorf("absent key should be no-op; env = %v", env)
	}
}

// --- Spec table integrity ---

func TestDomainSpecs_allFourServicesPresent(t *testing.T) {
	cfg := domainConfig{}
	modes := modeConfig{}
	specs := domainSpecs(cfg, modes, portConfig{})
	names := map[string]bool{}
	for _, s := range specs {
		names[s.name] = true
	}
	for _, want := range []string{"otel", "hookd", "prometheus", "grafana"} {
		if !names[want] {
			t.Errorf("domainSpecs missing service %q", want)
		}
	}
}

// --- Dynamic port parameterization ---

func TestDomainSpecs_dynamicPorts(t *testing.T) {
	cfg := domainConfig{}
	modes := modeConfig{otelcolMode: "install", prometheusMode: "install", grafanaMode: "install"}
	ports := portConfig{prometheus: 9191, grafana: 3101, otelGRPC: 4330}
	specs := domainSpecs(cfg, modes, ports)
	want := map[string]string{
		"otel":       "http://localhost:4330",
		"prometheus": "http://localhost:9191",
		"grafana":    "http://localhost:3101",
	}
	for _, s := range specs {
		if expected, ok := want[s.name]; ok {
			if s.installURL != expected {
				t.Errorf("service %q installURL = %q; want %q", s.name, s.installURL, expected)
			}
		}
	}
}

func TestDomainSpecs_zeroPorts_usesDefaults(t *testing.T) {
	cfg := domainConfig{}
	modes := modeConfig{otelcolMode: "install", prometheusMode: "install", grafanaMode: "install"}
	specs := domainSpecs(cfg, modes, portConfig{})
	want := map[string]string{
		"otel":       "http://localhost:4327",
		"prometheus": "http://localhost:9190",
		"grafana":    "http://localhost:3100",
	}
	for _, s := range specs {
		if expected, ok := want[s.name]; ok {
			if s.installURL != expected {
				t.Errorf("service %q installURL = %q; want %q", s.name, s.installURL, expected)
			}
		}
	}
}
