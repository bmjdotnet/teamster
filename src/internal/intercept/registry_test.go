package intercept

import (
	"os"
	"strings"
	"testing"
)

// fixtureYAML is a small config exercising every match/render code path
// without pulling in the full shipped config.
const fixtureYAML = `
tags:
  TASK:
    color: [240, 110, 170]
    description: "Work management"
  CHRM:
    color: [80, 180, 220]
    description: "Browser automation"
  TOOL:
    color: [160, 160, 160]
    description: "Generic tool (fallback)"

interceptors:
  - match: { prefix: "mcp__wms__wms_" }
    rules:
      - match: { method: "createOutcome" }
        tag: TASK
        display: 'Created outcome __{{f "id"}}__: __{{f "title"}}__'
      - match: { method: "setFocus" }
        fields:
          _focus: '{{f "entityType"}} {{f "entityID"}}: {{f "focus"}}'
      - match: { method: "getWorkUnit" }
        tag: TASK
        display: 'Querying workunit __{{f "id"}}__'

  - match: { prefix: "mcp__chrome__" }
    tag: CHRM
    rules:
      - match: { method: "computer", params: { action: "click" } }
        display: 'Clicking __{{f "coordinate"}}__'
      - match: { method: "computer", params: { action: "screenshot" } }
        display: 'Screenshot'
      - match: { method: "computer" }
        display: 'Browser action: __{{f "action"}}__'

  - match: { prefix: "mcp__numeric__" }
    rules:
      - match: { method: "setStatus", params: { code: "200" } }
        tag: TASK
        display: 'OK'
      - match: { method: "setStatus" }
        tag: TASK
        display: 'Other'

  - match: { prefix: "mcp__activity__" }
    rules:
      - match: { method: "setMode" }
        suppress: true

  - match: { prefix: "mcp__health__" }
    suppress: true
`

func mustBuildRegistry(t *testing.T, yamlSrc string) *Registry {
	t.Helper()
	reg, err := buildFromYAML([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("buildFromYAML: %v", err)
	}
	return reg
}

func TestMatchByMethod(t *testing.T) {
	reg := mustBuildRegistry(t, fixtureYAML)
	result := reg.Match("mcp__wms__wms_getWorkUnit", map[string]interface{}{"id": "wu-1"})
	if result == nil {
		t.Fatal("expected a match, got nil")
	}
	if result.Tag != "TASK" {
		t.Errorf("tag = %q, want TASK", result.Tag)
	}
	if want := "Querying workunit __wu-1__"; result.Display != want {
		t.Errorf("display = %q, want %q", result.Display, want)
	}
}

func TestNamespacePrefixMatching(t *testing.T) {
	reg := mustBuildRegistry(t, fixtureYAML)
	// "createOutcome" is only a rule under the wms namespace's rules; called
	// under the chrome namespace's prefix it must not leak the wms rule's
	// tag/display, it must fall to chrome's own namespace fallback instead.
	r := reg.Match("mcp__chrome__createOutcome", nil)
	if r == nil {
		t.Fatal("expected chrome's namespace-level fallback, got nil")
	}
	if r.Tag != "CHRM" {
		t.Errorf("tag = %q, want CHRM — the wms rule must not apply across the namespace boundary", r.Tag)
	}
	if strings.Contains(r.Display, "Created outcome") {
		t.Errorf("display = %q leaked the wms createOutcome rule into the chrome namespace", r.Display)
	}
}

func TestParamsMatchingStringNumericAbsent(t *testing.T) {
	reg := mustBuildRegistry(t, fixtureYAML)

	t.Run("string param match", func(t *testing.T) {
		r := reg.Match("mcp__chrome__computer", map[string]interface{}{"action": "click", "coordinate": "10,20"})
		if r == nil || r.Display != "Clicking __10,20__" {
			t.Fatalf("got %+v, want Clicking __10,20__", r)
		}
	})

	t.Run("numeric param match", func(t *testing.T) {
		r := reg.Match("mcp__numeric__setStatus", map[string]interface{}{"code": float64(200)})
		if r == nil || r.Display != "OK" {
			t.Fatalf("got %+v, want display OK", r)
		}
	})

	t.Run("numeric param mismatch falls through to generic rule", func(t *testing.T) {
		r := reg.Match("mcp__numeric__setStatus", map[string]interface{}{"code": float64(404)})
		if r == nil || r.Display != "Other" {
			t.Fatalf("got %+v, want display Other", r)
		}
	})

	t.Run("absent param field never matches", func(t *testing.T) {
		r := reg.Match("mcp__numeric__setStatus", map[string]interface{}{})
		if r == nil || r.Display != "Other" {
			t.Fatalf("absent params field should fall through to the params-less rule, got %+v", r)
		}
	})
}

func TestFirstMatchWinsPriority(t *testing.T) {
	reg := mustBuildRegistry(t, fixtureYAML)

	click := reg.Match("mcp__chrome__computer", map[string]interface{}{"action": "click"})
	// coordinate absent -> "Clicking __" + "" + "__" = "Clicking ____",
	// collapsed to "Clicking " (trailing space, all four underscores gone).
	if click == nil || click.Display != "Clicking " {
		t.Fatalf("got %+v, want display \"Clicking \"", click)
	}
	shot := reg.Match("mcp__chrome__computer", map[string]interface{}{"action": "screenshot"})
	if shot == nil || shot.Display != "Screenshot" {
		t.Fatalf("got %+v, want Screenshot", shot)
	}
	generic := reg.Match("mcp__chrome__computer", map[string]interface{}{"action": "drag"})
	if generic == nil || generic.Display != "Browser action: __drag__" {
		t.Fatalf("got %+v, want the generic fallback rule", generic)
	}
}

func TestNamespaceFallbackNoRuleMatch(t *testing.T) {
	reg := mustBuildRegistry(t, fixtureYAML)
	r := reg.Match("mcp__chrome__unknown_method", nil)
	if r == nil {
		t.Fatal("expected namespace-level fallback, got nil")
	}
	if r.Tag != "CHRM" {
		t.Errorf("tag = %q, want CHRM (namespace default)", r.Tag)
	}
	if want := "chrome(__unknown_method__)"; r.Display != want {
		t.Errorf("display = %q, want %q", r.Display, want)
	}
}

func TestNoNamespaceMatchReturnsNil(t *testing.T) {
	reg := mustBuildRegistry(t, fixtureYAML)
	if r := reg.Match("mcp__totally_unconfigured__method", nil); r != nil {
		t.Errorf("expected nil, got %+v", r)
	}
	if r := reg.Match("Read", map[string]interface{}{"file_path": "x"}); r != nil {
		t.Errorf("expected nil for a non-MCP built-in tool name, got %+v", r)
	}
}

func TestFieldAccessor(t *testing.T) {
	f := fieldAccessor(map[string]interface{}{
		"str":   "hello",
		"num":   float64(42),
		"frac":  float64(3.5),
		"other": true,
	})
	if got := f("missing"); got != "" {
		t.Errorf("absent field: got %q, want empty string", got)
	}
	if got := f("str"); got != "hello" {
		t.Errorf("string field: got %q, want hello", got)
	}
	if got := f("num"); got != "42" {
		t.Errorf("integral float64 field: got %q, want 42 (no scientific notation)", got)
	}
	if got := f("frac"); got != "3.5" {
		t.Errorf("fractional float64 field: got %q, want 3.5", got)
	}
	if got := f("other"); got != "true" {
		t.Errorf("bool field: got %q, want true (fmt.Sprint fallback)", got)
	}
	if got := fieldAccessor(nil)("x"); got != "" {
		t.Errorf("nil toolInput: got %q, want empty string", got)
	}
}

func TestTruncateFuncRuneSafety(t *testing.T) {
	// Byte-slicing a multi-byte UTF-8 string (s[:n]) can split a rune in
	// half, producing invalid UTF-8. truncateFunc must count runes, not
	// bytes.
	if got := truncateFunc(3, "日本語テスト"); got != "日本語" {
		t.Errorf("truncateFunc(3, multi-byte) = %q, want %q", got, "日本語")
	}
	if got := truncateFunc(10, "日本語テスト"); got != "日本語テスト" {
		t.Errorf("truncateFunc(n >= rune count) = %q, want the untruncated string", got)
	}
	if got := truncateFunc(0, "日本語"); got != "" {
		t.Errorf("truncateFunc(0, ...) = %q, want empty string", got)
	}
	if got := truncateFunc(-1, "日本語"); got != "日本語" {
		t.Errorf("truncateFunc(-1, ...) = %q, want the untruncated string (negative n is a no-op)", got)
	}
	if got := truncateFunc(5, "hello world"); got != "hello" {
		t.Errorf("truncateFunc(5, ascii) = %q, want %q", got, "hello")
	}
}

func TestSuppressBehavior(t *testing.T) {
	reg := mustBuildRegistry(t, fixtureYAML)

	t.Run("rule-level suppress", func(t *testing.T) {
		r := reg.Match("mcp__activity__setMode", map[string]interface{}{"mode": "solo"})
		if r == nil || !r.Suppress {
			t.Fatalf("got %+v, want Suppress: true", r)
		}
	})

	t.Run("namespace-level suppress with no rules", func(t *testing.T) {
		r := reg.Match("mcp__health__health_listAgents", nil)
		if r == nil || !r.Suppress {
			t.Fatalf("got %+v, want Suppress: true", r)
		}
	})
}

func TestFieldsMapRendering(t *testing.T) {
	reg := mustBuildRegistry(t, fixtureYAML)
	r := reg.Match("mcp__wms__wms_setFocus", map[string]interface{}{
		"entityType": "workunit",
		"entityID":   "wu-42",
		"focus":      "build the thing",
	})
	if r == nil {
		t.Fatal("expected a match")
	}
	if r.Tag != "" {
		t.Errorf("tag = %q, want empty (setFocus carries no tag, only fields)", r.Tag)
	}
	if r.Display != "" {
		t.Errorf("display = %q, want empty", r.Display)
	}
	want := "workunit wu-42: build the thing"
	if got := r.Fields["_focus"]; got != want {
		t.Errorf("_focus = %q, want %q", got, want)
	}
}

func TestPostProcessingCollapsesEmptyParamMarkers(t *testing.T) {
	reg := mustBuildRegistry(t, fixtureYAML)
	r := reg.Match("mcp__wms__wms_createOutcome", map[string]interface{}{"id": "my-outcome"})
	if r == nil {
		t.Fatal("expected a match")
	}
	want := "Created outcome __my-outcome__: "
	if r.Display != want {
		t.Errorf("display = %q, want %q (absent title's ____ collapsed)", r.Display, want)
	}
}

// --- Validation ---

func TestValidateTagsRejectsBadTagReference(t *testing.T) {
	const badYAML = `
tags:
  TOOL:
    color: [160, 160, 160]
    description: "fallback"
interceptors:
  - match: { prefix: "mcp__x__" }
    rules:
      - match: { method: "y" }
        tag: NOPE
        display: "z"
`
	_, err := buildFromYAML([]byte(badYAML))
	if err == nil {
		t.Fatal("expected an error for an undefined tag reference")
	}
	if !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("error %q should mention the bad tag name", err)
	}
}

func TestValidateTagsRequiresFourCharLabel(t *testing.T) {
	err := validateTags(map[string]TagConfig{
		"TOOL": {Color: [3]int{1, 1, 1}},
		"ACT":  {Color: [3]int{1, 1, 1}}, // 3 chars, missing its leading space
	})
	if err == nil {
		t.Fatal("expected an error for a 3-character tag label")
	}
}

func TestValidateTagsRequiresTool(t *testing.T) {
	err := validateTags(map[string]TagConfig{
		"TASK": {Color: [3]int{1, 1, 1}},
	})
	if err == nil {
		t.Fatal("expected an error when TOOL tag is missing")
	}
}

func TestValidateTagsRejectsOutOfRangeColor(t *testing.T) {
	err := validateTags(map[string]TagConfig{
		"TOOL": {Color: [3]int{1, 1, 1}},
		"TASK": {Color: [3]int{300, 0, 0}},
	})
	if err == nil {
		t.Fatal("expected an error for an out-of-range RGB component")
	}
}

func TestCompileTemplateRejectsControlFlow(t *testing.T) {
	cases := []string{
		`{{if eq (f "action") "click"}}Clicking{{else}}Other{{end}}`,
		`{{range .items}}{{.}}{{end}}`,
		`{{with .x}}{{.}}{{end}}`,
	}
	for _, tmplText := range cases {
		if _, err := compileTemplate("t", tmplText); err == nil {
			t.Errorf("template %q: expected control-flow rejection, got no error", tmplText)
		}
	}
}

func TestCompileTemplateAllowsInterpolationOnly(t *testing.T) {
	if _, err := compileTemplate("t", `Hello __{{f "name"}}__`); err != nil {
		t.Fatalf("plain interpolation template should compile: %v", err)
	}
}

func TestDetectShadowedRules(t *testing.T) {
	t.Run("method-only rule before method+params rule warns", func(t *testing.T) {
		rules := []RuleConfig{
			{Match: MatchConfig{Method: "computer"}},
			{Match: MatchConfig{Method: "computer", Params: map[string]string{"action": "click"}}},
		}
		warnings := detectShadowedRules("mcp__x__", rules)
		if len(warnings) != 1 {
			t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
		}
	})

	t.Run("params-specific rule before generic rule does not warn", func(t *testing.T) {
		rules := []RuleConfig{
			{Match: MatchConfig{Method: "computer", Params: map[string]string{"action": "click"}}},
			{Match: MatchConfig{Method: "computer"}},
		}
		if warnings := detectShadowedRules("mcp__x__", rules); len(warnings) != 0 {
			t.Errorf("expected no warnings, got %v", warnings)
		}
	})

	t.Run("different methods never shadow", func(t *testing.T) {
		rules := []RuleConfig{
			{Match: MatchConfig{Method: "a"}},
			{Match: MatchConfig{Method: "b", Params: map[string]string{"x": "y"}}},
		}
		if warnings := detectShadowedRules("mcp__x__", rules); len(warnings) != 0 {
			t.Errorf("expected no warnings, got %v", warnings)
		}
	})
}

// --- Embedded default config ---

func TestEmbeddedDefaultIsValid(t *testing.T) {
	reg, err := LoadDefault()
	if err != nil {
		t.Fatalf("embedded default config must always parse and validate: %v", err)
	}
	if _, ok := reg.Tags["TOOL"]; !ok {
		t.Error("embedded default config is missing the mandatory TOOL tag")
	}
	if len(reg.Namespaces) == 0 {
		t.Error("embedded default config has no interceptor namespaces")
	}
}

// TestEmbeddedConfigMatchesSkelSource is a sync tripwire: go:embed can't
// reach skel/etc/interceptors.yaml directly (patterns can't ascend out of
// the source file's directory), so a checked-in copy lives alongside
// config.go. This test fails loudly if someone edits one copy without the
// other.
func TestEmbeddedConfigMatchesSkelSource(t *testing.T) {
	skelPath := "../../../skel/etc/interceptors.yaml"
	skelData, err := os.ReadFile(skelPath)
	if err != nil {
		t.Fatalf("read %s: %v", skelPath, err)
	}
	if string(skelData) != string(embeddedDefaultYAML) {
		t.Errorf("internal/intercept/interceptors.yaml (go:embed source) has drifted from %s — copy the shipped file over the embedded one", skelPath)
	}
}
