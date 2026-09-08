package clonedata

import "testing"

func TestVerifyRODSN(t *testing.T) {
	got, err := VerifyRODSN("mysql://appuser:apppass@localhost:3306/teamster", "sekret", "claude_telemetry")
	if err != nil {
		t.Fatal(err)
	}
	want := "mysql://clone_verify_ro:sekret@localhost:3306/claude_telemetry"
	if got != want {
		t.Errorf("VerifyRODSN = %q, want %q", got, want)
	}
}

func TestVerifyRODSN_InvalidSourceDSN(t *testing.T) {
	if _, err := VerifyRODSN("://not a url", "pw", "teamster"); err == nil {
		t.Fatal("expected an error for an invalid source DSN, got nil")
	}
}
