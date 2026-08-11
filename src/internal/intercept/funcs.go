package intercept

import (
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/bmjdotnet/teamster/internal/redact"
)

// stringifyFieldValue renders a tool_input field value for template
// interpolation: "" for nil, strings as-is, integral float64s (JSON numbers
// like PR numbers) without scientific notation, everything else via
// fmt.Sprint. Shared by the "f" template func and rule params matching so
// both use identical coercion.
func stringifyFieldValue(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// fPlaceholder is registered at template-compile time so Parse accepts
// {{f "field"}} call syntax. It is never actually invoked — every Execute
// rebinds "f" to a closure over that call's tool_input map via
// Template.Clone().Funcs(...). See registry.go's executeTemplate.
func fPlaceholder(string) string { return "" }

// staticFuncMap holds the template funcs that don't need per-execution
// context: truncate/basename/lower/upper/redact are pure, and "f" is the
// parse-time placeholder (rebound before every Execute).
func staticFuncMap() template.FuncMap {
	return template.FuncMap{
		"f":        fPlaceholder,
		"truncate": truncateFunc,
		"basename": basenameFunc,
		"lower":    strings.ToLower,
		"upper":    strings.ToUpper,
		"redact":   redact.Redact,
	}
}

// fieldAccessor returns the real "f" closure for one template execution,
// bound to toolInput.
func fieldAccessor(toolInput map[string]interface{}) func(string) string {
	return func(key string) string {
		if toolInput == nil {
			return ""
		}
		v, ok := toolInput[key]
		if !ok {
			return ""
		}
		return stringifyFieldValue(v)
	}
}

func truncateFunc(n int, s string) string {
	if n < 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func basenameFunc(path string) string {
	path = strings.TrimRight(path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}
