package clone

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseVersionOutput(t *testing.T) {
	tests := []struct {
		name                               string
		in                                 string
		wantCommit, wantVersion, wantBuild string
		wantErr                            bool
	}{
		{
			name:        "well-formed",
			in:          "teamster v0.2.6 (c52f51c, 2026-08-14T02:37:57Z)\n",
			wantCommit:  "c52f51c",
			wantVersion: "v0.2.6",
			wantBuild:   "2026-08-14T02:37:57Z",
		},
		{
			name:        "hookd binary",
			in:          "hookd v0.2.6 (c52f51c, 2026-08-14T02:37:57Z)",
			wantCommit:  "c52f51c",
			wantVersion: "v0.2.6",
			wantBuild:   "2026-08-14T02:37:57Z",
		},
		{
			name:    "malformed",
			in:      "not a version line",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			commit, version, build, err := ParseVersionOutput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if commit != tc.wantCommit || version != tc.wantVersion || build != tc.wantBuild {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)", commit, version, build, tc.wantCommit, tc.wantVersion, tc.wantBuild)
			}
		})
	}
}

func TestValidateCommit(t *testing.T) {
	for _, bad := range []string{"", "none"} {
		if err := validateCommit(bad); !errors.Is(err, ErrUnstamped) {
			t.Errorf("validateCommit(%q) = %v, want ErrUnstamped", bad, err)
		}
	}
	if err := validateCommit("c52f51c"); err != nil {
		t.Errorf("validateCommit(good) = %v, want nil", err)
	}
}

func TestQueryLocalBuild(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		if name != "teamster" || len(args) != 1 || args[0] != "--version" {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return "teamster v0.2.6 (c52f51c, 2026-08-14T02:37:57Z)\n", nil
	}
	bd, err := QueryLocalBuild(context.Background(), run, "teamster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bd.Commit != "c52f51c" || bd.Version != "v0.2.6" {
		t.Errorf("got %+v", bd)
	}
}

func TestQueryLocalBuild_Unstamped(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		return "teamster dev (none, unknown)\n", nil
	}
	_, err := QueryLocalBuild(context.Background(), run, "teamster")
	if !errors.Is(err, ErrUnstamped) {
		t.Fatalf("got %v, want ErrUnstamped", err)
	}
}

func TestQueryRemoteHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"status":"ok","version":"v0.2.6","commit":"c52f51c"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	bd, err := QueryRemoteHealth(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bd.Commit != "c52f51c" || bd.Version != "v0.2.6" {
		t.Errorf("got %+v", bd)
	}
}

func TestQueryRemoteHealth_Unstamped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","version":"dev","commit":"none"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	_, err := QueryRemoteHealth(context.Background(), srv.Client(), srv.URL)
	if !errors.Is(err, ErrUnstamped) {
		t.Fatalf("got %v, want ErrUnstamped", err)
	}
}

func TestHookdBaseFor(t *testing.T) {
	tests := map[string]string{
		"chunk":                       "http://chunk:9125",
		"http://chunk:9125":           "http://chunk:9125",
		"https://chunk.internal:9125": "https://chunk.internal:9125",
	}
	for in, want := range tests {
		if got := hookdBaseFor(in); got != want {
			t.Errorf("hookdBaseFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsLocalSource(t *testing.T) {
	tests := []struct {
		source, host string
		want         bool
	}{
		{"", "source", true},
		{"source", "source", true},
		{"localhost", "source", true},
		{"127.0.0.1", "source", true},
		{"http://source:9125", "source", true},
		{"chunk", "source", false},
		{"http://chunk:9125", "source", false},
	}
	for _, tc := range tests {
		if got := isLocalSource(tc.source, tc.host); got != tc.want {
			t.Errorf("isLocalSource(%q, %q) = %v, want %v", tc.source, tc.host, got, tc.want)
		}
	}
}

func TestResolveBuild_PrefersLocalWhenSourceUnset(t *testing.T) {
	calledLocal := false
	run := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		calledLocal = true
		return "teamster v0.2.6 (c52f51c, 2026-08-14T02:37:57Z)\n", nil
	}
	_, err := ResolveBuild(context.Background(), SourceQueryOptions{
		Source:   "",
		Hostname: "source",
		Run:      run,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !calledLocal {
		t.Error("expected local CLI channel to be used when --source is unset (R9)")
	}
}

func TestResolveBuild_RemoteWhenSourceIsDifferentHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","version":"v0.2.6","commit":"c52f51c"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	// httptest always binds loopback, which isLocalSource correctly treats
	// as always-local (real-world 127.0.0.1 genuinely does mean "this
	// host") — so a bare loopback URL can't stand in for "a different
	// host" here. Instead use a fake non-loopback hostname ("chunk") and a
	// custom Transport that redirects the actual dial to the test server,
	// exercising hookdBaseFor's bare-hostname-defaults-to-:9125 behavior
	// for real at the same time.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, srv.Listener.Addr().String())
			},
		},
	}

	run := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		t.Fatal("local channel should not be used when --source names a different host")
		return "", nil
	}
	bd, err := ResolveBuild(context.Background(), SourceQueryOptions{
		Source:     "chunk",
		Hostname:   "source",
		Run:        run,
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bd.Commit != "c52f51c" {
		t.Errorf("got %+v", bd)
	}
}
