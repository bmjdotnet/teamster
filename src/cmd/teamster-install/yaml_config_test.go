package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// writePriorYAML writes a teamster.yaml into basedir/etc so buildYAMLConfig's
// readExistingYAML picks it up as the prior config.
func writePriorYAML(t *testing.T, basedir string, prior teamsterYAML) {
	t.Helper()
	etcDir := filepath.Join(basedir, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	out, err := yaml.Marshal(prior)
	if err != nil {
		t.Fatalf("marshal prior: %v", err)
	}
	if err := os.WriteFile(filepath.Join(etcDir, "teamster.yaml"), out, 0o644); err != nil {
		t.Fatalf("write prior: %v", err)
	}
}

// TestBuildYAMLConfig_PreservesOperatorTags is the golden round-trip test
// (Stream A test #9, red-team S1/S2). A prior tags: block with FULLY POPULATED
// fields must survive buildYAMLConfig UNCHANGED — the installer must never
// clobber the operator's vocabulary on an upgrade install.
func TestBuildYAMLConfig_PreservesOperatorTags(t *testing.T) {
	basedir := t.TempDir()

	// A fully-populated, hand-edited operator vocabulary: every yamlTagConfig
	// field exercised, plus a custom key the installer has no built-in notion
	// of. If any field drifts or is dropped, the assertion below fails.
	writePriorYAML(t, basedir, teamsterYAML{
		Hookd: yamlHookd{Mode: "systemd", Port: 9125},
		Tags: map[string]yamlTagConfig{
			"project": {
				Category:    "context",
				Cardinality: "single",
				Values:      []string{}, // create-on-apply: explicit empty, no values
				Description: "operator-edited project description",
			},
			"priority": {
				Category:    "context",
				Cardinality: "single",
				Values:      []string{"p0", "p1", "p2", "p3", "p4"}, // operator added p4
				Description: "operator-edited priority",
			},
			"customkey": {
				Category:    "context",
				Cardinality: "multi",
				Values:      []string{"alpha", "beta"},
				Description: "a key the installer does not ship by default",
			},
		},
	})

	// The faithful round-trip invariant: buildYAMLConfig must return exactly what
	// re-reading the prior file yields. We compare against readExistingYAML (not
	// the in-memory literal) so the test asserts true preservation and is not
	// tripped by yaml.v3's nil-vs-empty-slice normalization on the disk hop.
	priorTags := readExistingYAML(basedir).Tags
	if len(priorTags) != 3 {
		t.Fatalf("prior fixture did not round-trip to disk: got %d keys", len(priorTags))
	}

	got := buildYAMLConfig(yamlParams{basedir: basedir})

	if !reflect.DeepEqual(got.Tags, priorTags) {
		t.Errorf("buildYAMLConfig clobbered operator tags:\n got  %#v\n want %#v", got.Tags, priorTags)
	}

	// Assert FULL per-field equality (not merely "non-empty survives"): every
	// field of every key must round-trip, including the custom key.
	for key, want := range priorTags {
		g, ok := got.Tags[key]
		if !ok {
			t.Errorf("tag key %q dropped", key)
			continue
		}
		if g.Category != want.Category {
			t.Errorf("tag %q category = %q; want %q", key, g.Category, want.Category)
		}
		if g.Cardinality != want.Cardinality {
			t.Errorf("tag %q cardinality = %q; want %q", key, g.Cardinality, want.Cardinality)
		}
		if !reflect.DeepEqual(g.Values, want.Values) {
			t.Errorf("tag %q values = %v; want %v", key, g.Values, want.Values)
		}
		if g.Description != want.Description {
			t.Errorf("tag %q description = %q; want %q", key, g.Description, want.Description)
		}
	}
}

// TestBuildYAMLConfig_FirstInstallInjectsDefaultVocab verifies that a fresh
// install (no prior tags:) gets the shipped defaultTagVocab() block.
func TestBuildYAMLConfig_FirstInstallInjectsDefaultVocab(t *testing.T) {
	basedir := t.TempDir() // no teamster.yaml exists

	got := buildYAMLConfig(yamlParams{basedir: basedir})

	want := defaultTagVocab()
	if !reflect.DeepEqual(got.Tags, want) {
		t.Errorf("first install did not inject default vocab:\n got  %#v\n want %#v", got.Tags, want)
	}
	// Sanity: the declared starter keys are present and single-value.
	for _, key := range []string{"project", "priority"} {
		tc, ok := got.Tags[key]
		if !ok {
			t.Fatalf("default vocab missing key %q", key)
		}
		if tc.Cardinality != "single" {
			t.Errorf("default %q cardinality = %q; want single", key, tc.Cardinality)
		}
	}
}

// TestBuildYAMLConfig_EmptyPriorTagsGetsDefault verifies that a prior config
// present but with an EMPTY tags: block (e.g. operator deleted every key, or a
// pre-tags-era yaml) is treated as first-install for vocabulary: the default
// block is injected rather than persisting an empty map.
func TestBuildYAMLConfig_EmptyPriorTagsGetsDefault(t *testing.T) {
	basedir := t.TempDir()
	writePriorYAML(t, basedir, teamsterYAML{
		Hookd: yamlHookd{Mode: "systemd", Port: 9125},
		// Tags: nil — simulates a pre-tags-era teamster.yaml.
	})

	got := buildYAMLConfig(yamlParams{basedir: basedir})

	if !reflect.DeepEqual(got.Tags, defaultTagVocab()) {
		t.Errorf("empty prior tags not backfilled with default vocab:\n got  %#v", got.Tags)
	}
}

// TestWriteReadRoundTrip is the end-to-end silent-data-loss guard (S1): it
// drives the real writeYAMLConfig (marshal → WriteFile, truncating) and then
// re-reads, proving an operator's custom tags: block survives a full
// write→read cycle. This is the cycle that an upgrade install performs.
func TestWriteReadRoundTrip(t *testing.T) {
	basedir := t.TempDir()

	writePriorYAML(t, basedir, teamsterYAML{Tags: map[string]yamlTagConfig{
		"project": {Category: "context", Cardinality: "single", Values: []string{}, Description: "p"},
		"customkey": {
			Category:    "context",
			Cardinality: "multi",
			Values:      []string{"alpha", "beta"},
			Description: "operator key",
		},
	}})

	// The disk-normalized prior is the invariant we must preserve across the
	// upgrade-install write→read cycle.
	priorTags := readExistingYAML(basedir).Tags

	// Simulate the upgrade install: re-marshal and overwrite teamster.yaml.
	writeYAMLConfig(yamlParams{basedir: basedir})

	got := readExistingYAML(basedir)
	if !reflect.DeepEqual(got.Tags, priorTags) {
		t.Errorf("write→read cycle clobbered operator tags:\n got  %#v\n want %#v", got.Tags, priorTags)
	}
}

// TestWriteYAMLConfigPerms0600 proves teamster.yaml is written owner-only (0600)
// — it carries the store DSN inline as a CLI fallback, same credential-hygiene
// class as the secrets EnvironmentFile. Also proves a re-install narrows a
// pre-existing world-readable (0644) file back to 0600.
func TestWriteYAMLConfigPerms0600(t *testing.T) {
	basedir := t.TempDir()
	dest := filepath.Join(basedir, "etc", "teamster.yaml")

	// Fresh write.
	writeYAMLConfig(yamlParams{basedir: basedir, storeDSN: fakeDSN})
	if mode := fileMode(t, dest); mode != 0o600 {
		t.Fatalf("fresh teamster.yaml perms = %o, want 600", mode)
	}

	// writePriorYAML writes at 0644; the re-install must narrow it to 0600.
	if err := os.Chmod(dest, 0o644); err != nil {
		t.Fatalf("chmod 0644: %v", err)
	}
	writeYAMLConfig(yamlParams{basedir: basedir, storeDSN: fakeDSN})
	if mode := fileMode(t, dest); mode != 0o600 {
		t.Fatalf("re-install teamster.yaml perms = %o, want 600 (must narrow, not widen)", mode)
	}
}
