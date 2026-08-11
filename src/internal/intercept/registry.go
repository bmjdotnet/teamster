package intercept

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"
)

// TagDef is a compiled tag definition: a 4-char display label with a fixed
// RGB color.
type TagDef struct {
	Label       string
	Color       [3]int
	Description string
}

// MatchSpec is the compiled form of a match block. Method is an exact match
// against the tool name remainder after the enclosing namespace's Prefix is
// stripped (rule level, the primary per-tool matcher — this avoids suffix
// collisions like "untagEntity" matching a "tagEntity" rule); Suffix/Exact/
// Regex match the full tool name; Params is an AND-match against tool_input
// field values, usable alongside any positional matcher at any level.
type MatchSpec struct {
	Prefix string
	Method string
	Suffix string
	Exact  string
	Params map[string]string

	regex *regexp.Regexp
}

// matches reports whether toolName (with methodRemainder = toolName with the
// enclosing namespace's Prefix stripped) and toolInput satisfy spec. A spec
// with no positional field set matches every tool name — letting a rule
// select purely on Params.
func (spec MatchSpec) matches(toolName, methodRemainder string, toolInput map[string]interface{}) bool {
	if spec.Prefix != "" && !strings.HasPrefix(toolName, spec.Prefix) {
		return false
	}
	if spec.Method != "" && methodRemainder != spec.Method {
		return false
	}
	if spec.Suffix != "" && !strings.HasSuffix(toolName, spec.Suffix) {
		return false
	}
	if spec.Exact != "" && toolName != spec.Exact {
		return false
	}
	if spec.regex != nil && !spec.regex.MatchString(toolName) {
		return false
	}
	for k, wanted := range spec.Params {
		v, ok := toolInput[k]
		if !ok {
			// Absent field never matches (absent != empty string).
			return false
		}
		if stringifyFieldValue(v) != wanted {
			return false
		}
	}
	return true
}

// Rule is one compiled per-tool-name matcher within a Namespace.
type Rule struct {
	Match    MatchSpec
	Tag      string
	Display  *template.Template
	Fields   map[string]*template.Template
	Suppress bool

	// matchCount is a pointer (not a value) so Rule stays copyable — it's
	// built as a local value and appended into Namespace.Rules.
	matchCount *atomic.Uint64
}

// Namespace is a compiled top-level interceptor: a Prefix (or other) match
// gating an ordered set of Rules, plus an optional direct Tag/Display/
// Fields/Suppress outcome used when no Rule matches — or when there are no
// Rules at all, e.g. mcp__health__'s blanket suppress.
type Namespace struct {
	Match    MatchSpec
	Tag      string
	Display  *template.Template
	Fields   map[string]*template.Template
	Suppress bool
	Rules    []Rule

	// noRuleMatchCount is a pointer for the same reason as Rule.matchCount.
	noRuleMatchCount *atomic.Uint64
}

// Registry holds the fully compiled interceptor config, ready for
// concurrent use by Match.
type Registry struct {
	Tags       map[string]TagDef
	Namespaces []Namespace

	mu              sync.Mutex
	unmatchedLogged map[string]bool
	lastErrLog      map[string]time.Time
}

// Result is what a matched interceptor produces for one PreToolUse event.
type Result struct {
	Tag      string
	Display  string
	Fields   map[string]string
	Suppress bool
}

// Match finds the first namespace matching toolName, then the first rule
// within it matching toolName/toolInput, and renders that rule's templates.
// Returns nil if no namespace matches at all. A namespace match with no rule
// match falls back to the namespace's own Tag/Display/Suppress, or TOOL + a
// generic "server(__method__)" display if the namespace defines neither
// (design doc M5). A runtime template-execution error is logged (throttled)
// and treated as no match, so the caller's existing built-in-tool fallback
// applies — events are never dropped because of a bad template.
func (r *Registry) Match(toolName string, toolInput map[string]interface{}) *Result {
	for i := range r.Namespaces {
		ns := &r.Namespaces[i]
		if !ns.Match.matches(toolName, "", toolInput) {
			continue
		}
		methodRemainder := strings.TrimPrefix(toolName, ns.Match.Prefix)
		for j := range ns.Rules {
			rule := &ns.Rules[j]
			if !rule.Match.matches(toolName, methodRemainder, toolInput) {
				continue
			}
			rule.matchCount.Add(1)
			result, err := renderRule(rule, ns, toolInput)
			if err != nil {
				r.logTemplateError(toolName, err)
				return nil
			}
			return result
		}
		ns.noRuleMatchCount.Add(1)
		result, err := fallbackForNamespace(ns, toolName, toolInput)
		if err != nil {
			r.logTemplateError(toolName, err)
			return nil
		}
		return result
	}
	if strings.HasPrefix(toolName, "mcp__") {
		r.logUnmatchedOnce(toolName)
	}
	return nil
}

func renderRule(rule *Rule, ns *Namespace, toolInput map[string]interface{}) (*Result, error) {
	if rule.Suppress {
		return &Result{Suppress: true}, nil
	}
	tag := rule.Tag
	if tag == "" {
		tag = ns.Tag
	}
	result := &Result{Tag: tag}
	if rule.Display != nil {
		display, err := executeTemplate(rule.Display, toolInput)
		if err != nil {
			return nil, err
		}
		result.Display = display
	}
	fields, err := renderFields(rule.Fields, toolInput)
	if err != nil {
		return nil, err
	}
	result.Fields = fields
	return result, nil
}

func fallbackForNamespace(ns *Namespace, toolName string, toolInput map[string]interface{}) (*Result, error) {
	if ns.Suppress {
		return &Result{Suppress: true}, nil
	}
	tag := ns.Tag
	if tag == "" {
		tag = "TOOL"
	}
	if ns.Display != nil {
		display, err := executeTemplate(ns.Display, toolInput)
		if err != nil {
			return nil, err
		}
		fields, err := renderFields(ns.Fields, toolInput)
		if err != nil {
			return nil, err
		}
		return &Result{Tag: tag, Display: display, Fields: fields}, nil
	}
	return &Result{Tag: tag, Display: genericDisplay(toolName)}, nil
}

// genericDisplay reproduces the built-in "server(__method__)" fallback used
// throughout hook.go/enrich.go for MCP tools with no specific handler, so a
// namespace-only match (no rules, no explicit display) renders identically
// to the pre-interceptor behavior.
func genericDisplay(toolName string) string {
	parts := strings.SplitN(toolName, "__", 3)
	if len(parts) == 3 {
		return parts[1] + "(__" + parts[2] + "__)"
	}
	return strings.ToLower(toolName)
}

func renderFields(fields map[string]*template.Template, toolInput map[string]interface{}) (map[string]string, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(fields))
	for k, tmpl := range fields {
		v, err := executeTemplate(tmpl, toolInput)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// executeTemplate runs tmpl against toolInput. The compiled template's "f"
// func is a parse-time placeholder (funcs.go) with no access to this call's
// toolInput, so every execution clones the template and rebinds "f" to a
// closure over toolInput — Clone is what makes this safe under concurrent
// Match() calls against one shared, otherwise-immutable Registry. Post-
// processing collapses "____" (a literal __ pair around an absent field
// that itself rendered as "") down to nothing (design doc I9).
func executeTemplate(tmpl *template.Template, toolInput map[string]interface{}) (string, error) {
	clone, err := tmpl.Clone()
	if err != nil {
		return "", fmt.Errorf("clone template: %w", err)
	}
	clone = clone.Funcs(template.FuncMap{"f": fieldAccessor(toolInput)})
	var buf strings.Builder
	if err := clone.Execute(&buf, toolInput); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return strings.ReplaceAll(buf.String(), "____", ""), nil
}

// logTemplateError logs a template execution failure, throttled to once per
// tool name per minute (design doc §2.7).
func (r *Registry) logTemplateError(toolName string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastErrLog[toolName]; ok && time.Since(last) < time.Minute {
		return
	}
	r.lastErrLog[toolName] = time.Now()
	slog.Error("intercept: template execution failed, falling back to generic display", "tool", toolName, "error", err)
}

// logUnmatchedOnce logs an mcp__* tool name with no matching namespace, once
// ever per tool name (design doc §2.8).
func (r *Registry) logUnmatchedOnce(toolName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unmatchedLogged[toolName] {
		return
	}
	r.unmatchedLogged[toolName] = true
	slog.Warn("intercept: no interceptor namespace matched MCP tool", "tool", toolName)
}

// buildRegistry validates cfg and compiles it into a Registry. Hard
// violations (bad tag reference, malformed regex, control flow in a
// template, an invalid 4-char label, TOOL missing, RGB out of range) are
// returned as an error — the caller (config.go) decides whether that means
// falling back to embedded defaults. Shadowed-rule findings are non-fatal:
// logged as warnings, the config still loads.
func buildRegistry(cfg *ConfigFile) (*Registry, error) {
	if err := validateTags(cfg.Tags); err != nil {
		return nil, err
	}

	tags := make(map[string]TagDef, len(cfg.Tags))
	for label, tc := range cfg.Tags {
		tags[label] = TagDef{Label: label, Color: tc.Color, Description: tc.Description}
	}

	reg := &Registry{
		Tags:            tags,
		unmatchedLogged: make(map[string]bool),
		lastErrLog:      make(map[string]time.Time),
	}

	for i, ic := range cfg.Interceptors {
		nsLabel := interceptorLabel(i, ic)

		if err := validateTagRef(cfg.Tags, ic.Tag, "namespace "+nsLabel); err != nil {
			return nil, err
		}
		matchSpec, err := compileMatchSpec(ic.Match)
		if err != nil {
			return nil, fmt.Errorf("namespace %s: %w", nsLabel, err)
		}

		ns := Namespace{
			Match:            matchSpec,
			Tag:              ic.Tag,
			Suppress:         ic.Suppress,
			noRuleMatchCount: new(atomic.Uint64),
		}
		if ic.Display != "" {
			tmpl, err := compileTemplate(nsLabel+"/display", ic.Display)
			if err != nil {
				return nil, err
			}
			ns.Display = tmpl
		}
		if len(ic.Fields) > 0 {
			ns.Fields = make(map[string]*template.Template, len(ic.Fields))
			for k, v := range ic.Fields {
				tmpl, err := compileTemplate(nsLabel+"/fields/"+k, v)
				if err != nil {
					return nil, err
				}
				ns.Fields[k] = tmpl
			}
		}

		for _, w := range detectShadowedRules(nsLabel, ic.Rules) {
			slog.Warn(w)
		}

		for j, rc := range ic.Rules {
			ruleLabel := fmt.Sprintf("%s/rule[%d method=%q]", nsLabel, j, rc.Match.Method)

			if err := validateTagRef(cfg.Tags, rc.Tag, ruleLabel); err != nil {
				return nil, err
			}
			rMatch, err := compileMatchSpec(rc.Match)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ruleLabel, err)
			}

			rule := Rule{
				Match:      rMatch,
				Tag:        rc.Tag,
				Suppress:   rc.Suppress,
				matchCount: new(atomic.Uint64),
			}
			if rc.Display != "" {
				tmpl, err := compileTemplate(ruleLabel+"/display", rc.Display)
				if err != nil {
					return nil, err
				}
				rule.Display = tmpl
			}
			if len(rc.Fields) > 0 {
				rule.Fields = make(map[string]*template.Template, len(rc.Fields))
				for k, v := range rc.Fields {
					tmpl, err := compileTemplate(ruleLabel+"/fields/"+k, v)
					if err != nil {
						return nil, err
					}
					rule.Fields[k] = tmpl
				}
			}
			ns.Rules = append(ns.Rules, rule)
		}

		reg.Namespaces = append(reg.Namespaces, ns)
	}

	return reg, nil
}

// interceptorLabel names a namespace for error/warning messages.
func interceptorLabel(i int, ic InterceptorConfig) string {
	if ic.Match.Prefix != "" {
		return ic.Match.Prefix
	}
	return fmt.Sprintf("interceptors[%d]", i)
}
