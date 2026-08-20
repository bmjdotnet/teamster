package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const testSettingsPath = "/home/chunk/.claude/settings.json"

func TestPreflightExistingRemote_NoSettingsFile(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return "", errors.New("cat: No such file or directory")
	}
	if err := preflightExistingRemote(context.Background(), sshRun, "user@chunk", testSettingsPath); err != nil {
		t.Fatalf("expected nil (absent settings.json is the common fresh-target case), got %v", err)
	}
}

func TestPreflightExistingRemote_NoHookURL(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return `{"env": {}}`, nil
	}
	if err := preflightExistingRemote(context.Background(), sshRun, "user@chunk", testSettingsPath); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestPreflightExistingRemote_StaleLocalhostAllowed(t *testing.T) {
	for _, url := range []string{"http://localhost:9125/event", "http://127.0.0.1:9125/event"} {
		sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
			return `{"env": {"TEAMSTER_HOOK_SERVER_URL": "` + url + `"}}`, nil
		}
		if err := preflightExistingRemote(context.Background(), sshRun, "user@chunk", testSettingsPath); err != nil {
			t.Errorf("stale-localhost URL %q should not refuse (installer heals it), got %v", url, err)
		}
	}
}

func TestPreflightExistingRemote_RealRemoteRefuses(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return `{"env": {"TEAMSTER_HOOK_SERVER_URL": "http://source:9125/event"}}`, nil
	}
	err := preflightExistingRemote(context.Background(), sshRun, "user@chunk", testSettingsPath)
	if !errors.Is(err, ErrExistingRemoteInstall) {
		t.Fatalf("got %v, want ErrExistingRemoteInstall", err)
	}
	if !strings.Contains(err.Error(), "http://source:9125/event") {
		t.Errorf("refusal should name the existing URL, got: %v", err)
	}
}

func TestPreflightExistingRemote_MalformedJSONIgnored(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return `not json at all`, nil
	}
	if err := preflightExistingRemote(context.Background(), sshRun, "user@chunk", testSettingsPath); err != nil {
		t.Fatalf("expected nil for unparseable content, got %v", err)
	}
}

func TestInvokeInstaller_PassesArgsAsPositionalParams(t *testing.T) {
	var gotArgs []string
	sshScript := func(ctx context.Context, target, script string, args ...string) (string, error) {
		gotArgs = args
		return "install ok", nil
	}
	out, err := invokeInstaller(context.Background(), sshScript, "user@chunk", "~/clone-src/abc123", "abc123", "v1.0.0-dirty", []string{"--hookd-mode=systemd", "--wire"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "install ok" {
		t.Errorf("got output %q", out)
	}
	want := []string{"~/clone-src/abc123", "abc123", "v1.0.0-dirty", "--hookd-mode=systemd", "--wire"}
	if len(gotArgs) != len(want) {
		t.Fatalf("got args %v, want %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want[i])
		}
	}
}

func TestInvokeInstaller_FailureSurfacesTail(t *testing.T) {
	longOutput := strings.Repeat("noise line\n", 100) + "the actual error\n"
	sshScript := func(ctx context.Context, target, script string, args ...string) (string, error) {
		return longOutput, errors.New("exit status 1")
	}
	_, err := invokeInstaller(context.Background(), sshScript, "user@chunk", "dir", "short", "ver", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "the actual error") {
		t.Errorf("expected failure tail to include the last line, got: %v", err)
	}
	if strings.Count(err.Error(), "noise line") >= 100 {
		t.Error("expected output to be truncated to a tail, not dumped in full")
	}
}

func TestMaskDisposableTimers(t *testing.T) {
	var gotArgs []string
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		gotArgs = args
		return "", nil
	}
	if err := maskDisposableTimers(context.Background(), sshRun, "user@chunk"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"systemctl", "mask", "teamster-sweep.timer", "teamster-backup.timer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected command to contain %q, got %q", want, joined)
		}
	}
	// Only sweep/backup are masked — rollup/classify/health-collector are the
	// clone's own ongoing self-observability and must not be silenced.
	for _, mustNotContain := range []string{"rollup", "classify", "health-collector"} {
		if strings.Contains(joined, mustNotContain) {
			t.Errorf("mask command should not touch %q", mustNotContain)
		}
	}
}

func TestStartTarget(t *testing.T) {
	var gotArgs []string
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		gotArgs = args
		return "", nil
	}
	if err := startTarget(context.Background(), sshRun, "user@chunk", "~/teamster/bin/teamster"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "~/teamster/bin/teamster" || gotArgs[1] != "start" {
		t.Errorf("got args %v, want [~/teamster/bin/teamster start]", gotArgs)
	}
}

func TestTailLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 5, ""},
		{"under limit", "a\nb\nc", 5, "a\nb\nc"},
		{"over limit", "a\nb\nc\nd\ne", 3, "c\nd\ne"},
		{"trailing newline stripped", "a\nb\n", 5, "a\nb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tailLines(tc.in, tc.n); got != tc.want {
				t.Errorf("tailLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}
