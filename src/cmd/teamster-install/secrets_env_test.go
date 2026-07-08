package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDSN is a NON-real credential used to prove the password never lands inline
// in a unit. Never put a live secret in a test fixture.
const (
	fakeDSN      = "mysql://teamster:FAKEPASSWORD@127.0.0.1:3306/teamster"
	fakePassword = "FAKEPASSWORD"
)

// TestRenderSecretsEnvFile proves the EnvironmentFile content is systemd-format
// (KEY=value, no quotes, no `export`) and carries the DSN, and that an empty DSN
// renders nothing.
func TestRenderSecretsEnvFile(t *testing.T) {
	got := renderSecretsEnvFile(fakeDSN)
	want := "TEAMSTER_STORE_DSN=" + fakeDSN + "\n"
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if strings.Contains(got, "\"") || strings.Contains(got, "export ") {
		t.Fatalf("EnvironmentFile must be KEY=value with no quotes/export, got %q", got)
	}
	if renderSecretsEnvFile("") != "" {
		t.Fatalf("empty DSN must render empty content")
	}
}

// TestWriteSecretsEnvFile proves the file is created at 0600 with the DSN, that a
// re-install narrows a pre-existing wider file back to 0600 (idempotent, never
// widens), and that an empty DSN writes nothing.
func TestWriteSecretsEnvFile(t *testing.T) {
	base := t.TempDir()
	path := secretsEnvPath(base)

	if err := writeSecretsEnvFile(base, fakeDSN); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(data), "TEAMSTER_STORE_DSN="+fakeDSN) {
		t.Fatalf("file missing DSN, got %q", string(data))
	}
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("perms = %o, want 600", mode)
	}

	// Simulate a pre-existing world-readable file from an older install; the
	// re-write must narrow it back to 0600.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := writeSecretsEnvFile(base, fakeDSN); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("re-install perms = %o, want 600 (must not widen)", mode)
	}

	// Empty DSN: no secrets to write, leave nothing behind.
	base2 := t.TempDir()
	if err := writeSecretsEnvFile(base2, ""); err != nil {
		t.Fatalf("empty write: %v", err)
	}
	if _, err := os.Stat(secretsEnvPath(base2)); !os.IsNotExist(err) {
		t.Fatalf("empty DSN must not create a secrets file (stat err=%v)", err)
	}
}

// TestDSNEnvLineNeverInlinesSecret proves the line every DSN-bearing unit
// receives is an EnvironmentFile= reference and never an inline
// Environment="...DSN..." — the line text must not contain the DSN or password.
func TestDSNEnvLineNeverInlinesSecret(t *testing.T) {
	secretsPath := secretsEnvPath("/opt/teamster")
	line := dsnEnvLine(secretsPath)

	if !strings.HasPrefix(line, "EnvironmentFile=") {
		t.Fatalf("unit DSN line must be EnvironmentFile=, got %q", line)
	}
	if !strings.Contains(line, secretsPath) {
		t.Fatalf("EnvironmentFile= must point at the secrets file, got %q", line)
	}
	if strings.Contains(line, "TEAMSTER_STORE_DSN=") {
		t.Fatalf("unit line must not inline the DSN env, got %q", line)
	}
	if strings.Contains(line, fakeDSN) || strings.Contains(line, fakePassword) {
		t.Fatalf("unit line must not contain the credential, got %q", line)
	}
}

// TestMaterializedUnitHasNoInlineDSN renders the five DSN-bearing service
// templates the way the installer does — substituting __BASEDIR__/__USER__ and
// appending the DSN env line — and asserts (a) no [Service] text contains the
// DSN/password inline, and (b) each unit references the secrets EnvironmentFile.
func TestMaterializedUnitHasNoInlineDSN(t *testing.T) {
	base := t.TempDir()
	secretsPath := secretsEnvPath(base)

	// The five real shipped templates that receive the DSN. We read them from
	// skel/etc/ (not stand-in strings) and apply the same materialization the
	// installer does — so a future edit that re-introduces an inline DSN in any
	// real template fails this test.
	repoRoot := repoRootFromTest(t)
	templates := []string{
		"teamster-hookd.service.tmpl",
		"teamster-rollup.service.tmpl",
		"teamster-classify.service.tmpl",
		"teamster-sweep.service.tmpl",
		"teamster-codex-scraper.service.tmpl",
	}
	for _, fname := range templates {
		data, err := os.ReadFile(filepath.Join(repoRoot, "skel", "etc", fname))
		if err != nil {
			t.Fatalf("read template %s: %v", fname, err)
		}
		m := strings.ReplaceAll(string(data), "__BASEDIR__", base)
		m = strings.ReplaceAll(m, "__USER__", "claude")
		// Mirror the installer's DSN injection: insert before [Install] when
		// present (hookd), else append to [Service] (rollup/classify/sweep).
		line := dsnEnvLine(secretsPath)
		if idx := strings.Index(m, "\n[Install]"); idx >= 0 {
			m = m[:idx] + "\n" + line + m[idx:]
		} else {
			m = strings.TrimRight(m, "\n") + "\n" + line
		}

		if strings.Contains(m, fakeDSN) || strings.Contains(m, fakePassword) {
			t.Fatalf("%s: unit text contains the credential inline:\n%s", fname, m)
		}
		if strings.Contains(m, "Environment=\"TEAMSTER_STORE_DSN=") ||
			strings.Contains(m, "Environment=TEAMSTER_STORE_DSN=") {
			t.Fatalf("%s: unit inlines TEAMSTER_STORE_DSN:\n%s", fname, m)
		}
		if strings.Contains(m, "__STORE_DSN__") {
			t.Fatalf("%s: unit carries unsubstituted __STORE_DSN__ placeholder:\n%s", fname, m)
		}
		if !strings.Contains(m, "EnvironmentFile="+secretsPath) {
			t.Fatalf("%s: unit missing EnvironmentFile= reference:\n%s", fname, m)
		}
	}
}

// repoRootFromTest resolves the repo root from this test's cwd
// (src/cmd/teamster-install) so tests can read shipped skel/ assets.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

func fileMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi.Mode().Perm()
}

// TestSweepTemplateHasNoInlineDSN guards the shipped sweep template: it must not
// carry an inline DSN or a stale __STORE_DSN__ placeholder (the installer appends
// the EnvironmentFile= line, like the other DSN-bearing units).
func TestSweepTemplateHasNoInlineDSN(t *testing.T) {
	tmpl := filepath.Join(repoRootFromTest(t), "skel", "etc", "teamster-sweep.service.tmpl")
	data, err := os.ReadFile(tmpl)
	if err != nil {
		t.Skipf("sweep template not found at %s: %v", tmpl, err)
	}
	s := string(data)
	if strings.Contains(s, "TEAMSTER_STORE_DSN") {
		t.Fatalf("sweep template must not reference TEAMSTER_STORE_DSN inline:\n%s", s)
	}
	if strings.Contains(s, "__STORE_DSN__") {
		t.Fatalf("sweep template must not carry the stale __STORE_DSN__ placeholder:\n%s", s)
	}
}
