package clonetopology

import (
	"sort"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/teamsteryaml"
)

// typicalSource mirrors a typical hub's teamster.yaml shape (WP2 §2's
// live-verified values) — used to prove the always-explicit/always-drop
// rules hold against a real-looking populated config, not just a zero one.
func typicalSource() SourceConfig {
	return teamsteryaml.Config{
		Hookd:      teamsteryaml.Hookd{Mode: "systemd", Port: 9125},
		Store:      teamsteryaml.Store{Mode: "managed", DSN: "mysql://teamster:test-secret-password@127.0.0.1:3306/teamster"},
		Prometheus: teamsteryaml.Service{Port: 9090, Health: "http://source:9090/-/healthy"},
		Grafana:    teamsteryaml.Service{},
		Otelcol:    teamsteryaml.Otelcol{Mode: "managed", GRPCPort: 4317, HTTPPort: 4318, CodexHTTPPort: 4329},
		Relay: teamsteryaml.Relay{
			Mode:           "install",
			Target:         "http://replica:9125/event",
			ReplPushRemote: "user@replica",
		},
		Env:  "production",
		Tags: map[string]teamsteryaml.TagConfig{"priority": {}, "project": {}},
	}
}

func sorted(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func assertFlagsEqual(t *testing.T, got, want []string) {
	t.Helper()
	g, w := sorted(got), sorted(want)
	if len(g) != len(w) {
		t.Fatalf("flag count mismatch:\ngot:  %v\nwant: %v", g, w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("flag mismatch at %d:\ngot:  %v\nwant: %v", i, g, w)
		}
	}
}

var expectedCloneFlags = []string{
	"--hookd-mode=systemd",
	"--store-mode=install",
	"--store-engine=mysql-8.4",
	"--otelcol-mode=install",
	"--prometheus-mode=install",
	"--grafana-mode=install",
	"--env=clone",
	"--wire",
}

func TestTranslate_PlexRealConfig(t *testing.T) {
	got, err := Translate(typicalSource(), TargetSpec{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertFlagsEqual(t, got, expectedCloneFlags)
}

func TestTranslate_EmptySourceProducesIdenticalOutput(t *testing.T) {
	// Proves §3's "always explicit regardless of source" rule structurally,
	// not just by inspection.
	got, err := Translate(SourceConfig{}, TargetSpec{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertFlagsEqual(t, got, expectedCloneFlags)
}

func TestTranslate_RelayNeverEmitted(t *testing.T) {
	src := SourceConfig{
		Relay: teamsteryaml.Relay{
			Mode:           "install",
			Target:         "http://replica:9125/event",
			ReplPushRemote: "user@replica",
		},
	}
	got, err := Translate(src, TargetSpec{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, f := range got {
		if strings.HasPrefix(f, "--relay-mode") || strings.HasPrefix(f, "--relay-target") || strings.HasPrefix(f, "--repl-push-remote") {
			t.Errorf("relay flag leaked into output: %q (R1/I2 violation)", f)
		}
	}
}

func TestTranslate_BasedirUnderForbiddenPrefixRefuses(t *testing.T) {
	src := SourceConfig{Clone: teamsteryaml.Clone{ForbiddenBasedirs: []string{"/shared/nfs"}}}
	tests := []string{
		"/shared/nfs/teamster-clone",
		"/shared/nfs",
		"/shared/nfs/",
		"/shared/nfs/sub/dir",
	}
	for _, basedir := range tests {
		t.Run(basedir, func(t *testing.T) {
			_, err := Translate(src, TargetSpec{Basedir: basedir})
			if err == nil {
				t.Fatalf("expected I5 refusal for basedir %q, got nil error", basedir)
			}
			if !strings.Contains(err.Error(), "forbidden prefix") {
				t.Errorf("expected error to cite the forbidden prefix, got: %v", err)
			}
		})
	}
}

func TestTranslate_BasedirLookalikeNotRefused(t *testing.T) {
	// /shared/nfsXXX must not false-positive against the /shared/nfs prefix check.
	src := SourceConfig{Clone: teamsteryaml.Clone{ForbiddenBasedirs: []string{"/shared/nfs"}}}
	_, err := Translate(src, TargetSpec{Basedir: "/shared/nfsXXX/teamster"})
	if err != nil {
		t.Fatalf("unexpected refusal for a lookalike path: %v", err)
	}
}

func TestTranslate_NoForbiddenBasedirsConfigured_NothingRefused(t *testing.T) {
	// An empty ForbiddenBasedirs (e.g. a source with no clone section yet)
	// must not refuse any basedir — I5 is operator-configured, not implicit.
	_, err := Translate(SourceConfig{}, TargetSpec{Basedir: "/shared/nfs/teamster-clone"})
	if err != nil {
		t.Fatalf("unexpected refusal with no forbidden prefixes configured: %v", err)
	}
}

func TestTranslate_BasedirEmpty_NoFlagEmitted(t *testing.T) {
	got, err := Translate(SourceConfig{}, TargetSpec{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, f := range got {
		if strings.HasPrefix(f, "--basedir") {
			t.Errorf("--basedir should not be emitted when TargetSpec.Basedir is empty, got %q", f)
		}
	}
}

func TestTranslate_BasedirSet_FlagEmitted(t *testing.T) {
	got, err := Translate(SourceConfig{}, TargetSpec{Basedir: "/home/claude/teamster"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, f := range got {
		if f == "--basedir=/home/claude/teamster" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --basedir=/home/claude/teamster in output, got %v", got)
	}
}

func TestTranslate_CustomEnvLabel(t *testing.T) {
	got, err := Translate(SourceConfig{}, TargetSpec{EnvLabel: "custom"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, f := range got {
		if f == "--env=custom" {
			found = true
		}
		if f == "--env=clone" {
			t.Error("default env label leaked through despite an explicit EnvLabel")
		}
	}
	if !found {
		t.Errorf("expected --env=custom in output, got %v", got)
	}
}

func TestTranslate_StoreDSNNeverLeaks(t *testing.T) {
	secretDSN := "mysql://teamster:test-super-secret-password@127.0.0.1:3306/teamster"
	src := SourceConfig{Store: teamsteryaml.Store{Mode: "managed", DSN: secretDSN}}
	got, err := Translate(src, TargetSpec{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "store-dsn") {
		t.Error("--store-dsn should never be emitted (I3) — installrunner.sh auto-generates a fresh DSN")
	}
	if strings.Contains(joined, "test-super-secret-password") {
		t.Error("the source DSN's password leaked into Translate's output")
	}
}

func TestTranslate_FiveModeFlags(t *testing.T) {
	// WP2 §5c: exactly five --*-mode= flags, not four (store-engine's
	// severity correction to DESIGN.md's I4).
	got, err := Translate(typicalSource(), TargetSpec{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	count := 0
	for _, f := range got {
		if strings.Contains(f, "-mode=") {
			count++
		}
	}
	if count != 5 {
		t.Errorf("expected exactly 5 mode flags, got %d: %v", count, got)
	}
}
