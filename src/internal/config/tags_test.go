package config_test

import (
	"reflect"
	"testing"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/wms"
	"gopkg.in/yaml.v3"
)

// TestTagsParse verifies a `tags:` yaml block unmarshals into
// FileConfig.Tags with category/cardinality/values/description (plan test #8).
// No DB — runs unconditionally.
func TestTagsParse(t *testing.T) {
	const src = `
tags:
  project:
    category: context
    cardinality: single
    description: "which project this work belongs to"
  priority:
    category: context
    cardinality: single
    values: [p0, p1, p2, p3]
    description: "urgency band"
`
	var fc config.FileConfig
	if err := yaml.Unmarshal([]byte(src), &fc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(fc.Tags) != 2 {
		t.Fatalf("len(Tags) = %d, want 2", len(fc.Tags))
	}

	project, ok := fc.Tags["project"]
	if !ok {
		t.Fatal("missing project key")
	}
	if project.Category != "context" || project.Cardinality != "single" {
		t.Fatalf("project = %+v, want category=context cardinality=single", project)
	}
	if len(project.Values) != 0 {
		t.Fatalf("project.Values = %v, want empty (create-on-apply)", project.Values)
	}
	if project.Description != "which project this work belongs to" {
		t.Fatalf("project.Description = %q", project.Description)
	}

	priority := fc.Tags["priority"]
	if want := []string{"p0", "p1", "p2", "p3"}; !reflect.DeepEqual(priority.Values, want) {
		t.Fatalf("priority.Values = %v, want %v", priority.Values, want)
	}
}

// TestTagSpecs verifies Config.TagSpecs converts the declared vocabulary into
// []wms.TagSpec in sorted key order, preserving all fields.
func TestTagSpecs(t *testing.T) {
	cfg := config.Config{Tags: map[string]config.TagConfig{
		"priority": {Category: "context", Cardinality: "single", Values: []string{"p0", "p1"}, Description: "urgency"},
		"project":  {Category: "context", Cardinality: "single", Description: "owning project"},
	}}

	got := cfg.TagSpecs()
	want := []wms.TagSpec{
		{Key: "priority", Category: "context", Cardinality: "single", Values: []string{"p0", "p1"}, Description: "urgency"},
		{Key: "project", Category: "context", Cardinality: "single", Description: "owning project"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TagSpecs() = %+v, want %+v", got, want)
	}
}

// TestTagSpecsEmpty verifies no declared vocabulary yields nil (reconcile then
// leaves seeds untouched).
func TestTagSpecsEmpty(t *testing.T) {
	if got := (config.Config{}).TagSpecs(); got != nil {
		t.Fatalf("TagSpecs() = %v, want nil for empty Tags", got)
	}
}
