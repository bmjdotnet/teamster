package clone

import (
	"context"
	"errors"
	"testing"
)

func TestProbeTarget_OK(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		if len(args) != 2 || args[0] != "printenv" || args[1] != "HOME" {
			t.Fatalf("unexpected command: %v", args)
		}
		return "/home/deploy\n", nil
	}
	info, err := ProbeTarget(context.Background(), sshRun, "user@chunk")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Home != "/home/deploy" {
		t.Errorf("Home = %q, want %q", info.Home, "/home/deploy")
	}
}

func TestProbeTarget_SSHFailure(t *testing.T) {
	wantErr := errors.New("connection refused")
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return "", wantErr
	}
	if _, err := ProbeTarget(context.Background(), sshRun, "user@chunk"); !errors.Is(err, wantErr) {
		t.Fatalf("got %v, want wrapped %v", err, wantErr)
	}
}

func TestProbeTarget_EmptyOutputRejected(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return "\n", nil
	}
	if _, err := ProbeTarget(context.Background(), sshRun, "user@chunk"); err == nil {
		t.Fatal("expected error for empty $HOME output")
	}
}

func TestProbeTarget_RelativeOutputRejected(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return "not-a-path\n", nil
	}
	if _, err := ProbeTarget(context.Background(), sshRun, "user@chunk"); err == nil {
		t.Fatal("expected error for non-absolute $HOME output")
	}
}
