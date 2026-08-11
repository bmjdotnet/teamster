package intercept

import (
	"fmt"
	"regexp"
	"text/template"
	"text/template/parse"
)

// validateTags enforces the tag-definition invariants (design doc §1.1):
// every label is exactly 4 characters (space-padding is significant, e.g.
// " ACT" — this catches an unquoted edit silently dropping the leading
// space), every RGB component is 0-255, and the TOOL fallback tag exists.
func validateTags(tags map[string]TagConfig) error {
	if _, ok := tags["TOOL"]; !ok {
		return fmt.Errorf("intercept: tags: TOOL tag is mandatory (fallback tag)")
	}
	for label, def := range tags {
		if len(label) != 4 {
			return fmt.Errorf("intercept: tags: label %q must be exactly 4 characters, got %d", label, len(label))
		}
		for _, c := range def.Color {
			if c < 0 || c > 255 {
				return fmt.Errorf("intercept: tags: %q color component %d out of range 0-255", label, c)
			}
		}
	}
	return nil
}

// validateTagRef checks that a rule/namespace's tag: reference (when set)
// exists in the tags map.
func validateTagRef(tags map[string]TagConfig, ref, context string) error {
	if ref == "" {
		return nil
	}
	if _, ok := tags[ref]; !ok {
		return fmt.Errorf("intercept: %s: tag %q is not defined in the tags map", context, ref)
	}
	return nil
}

// compileMatchSpec compiles a MatchConfig into a MatchSpec, pre-compiling
// the regex (if any) so per-event matching never parses a regex.
func compileMatchSpec(mc MatchConfig) (MatchSpec, error) {
	spec := MatchSpec{
		Prefix: mc.Prefix,
		Method: mc.Method,
		Suffix: mc.Suffix,
		Exact:  mc.Exact,
		Params: mc.Params,
	}
	if mc.Regex != "" {
		re, err := regexp.Compile(mc.Regex)
		if err != nil {
			return MatchSpec{}, fmt.Errorf("intercept: invalid regex %q: %w", mc.Regex, err)
		}
		spec.regex = re
	}
	return spec, nil
}

// compileTemplate parses a display/field template and rejects any use of
// control flow ({{if}}, {{range}}, {{with}}, and by extension {{else}} —
// operator ruling: interpolation-only templates, see design doc §2.2). name
// is used only for error messages and template.Template's internal name.
func compileTemplate(name, text string) (*template.Template, error) {
	tmpl, err := template.New(name).Funcs(staticFuncMap()).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("intercept: %s: parse template %q: %w", name, text, err)
	}
	if tmpl.Tree != nil && containsControlFlow(tmpl.Tree.Root) {
		return nil, fmt.Errorf("intercept: %s: template %q uses control flow ({{if}}/{{range}}/{{with}}) — interpolation only, model this as multiple rules with params matching instead", name, text)
	}
	return tmpl, nil
}

// containsControlFlow reports whether any node in list is an if/range/with
// block. A control-flow node anywhere in the template is always directly
// reachable from the root list (nesting only happens inside a branch's own
// List/ElseList, so the outermost occurrence is always a top-level Node) —
// a single-level scan is sufficient.
func containsControlFlow(list *parse.ListNode) bool {
	if list == nil {
		return false
	}
	for _, n := range list.Nodes {
		switch n.(type) {
		case *parse.IfNode, *parse.RangeNode, *parse.WithNode:
			return true
		}
	}
	return false
}

// detectShadowedRules warns (non-fatal) when an earlier rule in the same
// namespace unconditionally matches a method that a later, more specific
// (params-qualified) rule for the same method can never reach. This is the
// C2/shadowing case from the design doc: a method-only rule preceding a
// method+params rule with the same method makes the latter unreachable.
func detectShadowedRules(namespaceLabel string, rules []RuleConfig) []string {
	var warnings []string
	for i := 0; i < len(rules); i++ {
		for j := i + 1; j < len(rules); j++ {
			if rulesSubsume(rules[i].Match, rules[j].Match) {
				warnings = append(warnings, fmt.Sprintf(
					"intercept: namespace %s: rule %d (method=%q) shadows rule %d (method=%q, params=%v) — rule %d is unreachable, reorder so the more specific rule comes first",
					namespaceLabel, i, rules[i].Match.Method, j, rules[j].Match.Method, rules[j].Match.Params, j))
			}
		}
	}
	return warnings
}

// rulesSubsume reports whether earlier unconditionally matches every event
// later would match, given both target the same method.
func rulesSubsume(earlier, later MatchConfig) bool {
	if earlier.Method == "" || earlier.Method != later.Method {
		return false
	}
	if len(earlier.Params) == 0 {
		// earlier matches on method alone — it will catch everything later
		// (with or without params) matches for that method.
		return true
	}
	return paramsSubset(earlier.Params, later.Params)
}

// paramsSubset reports whether every key/value in a is also present with an
// equal value in b (so a's condition is implied by b's, meaning a rule
// requiring only "a" would already have matched anything matching "b").
func paramsSubset(a, b map[string]string) bool {
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
