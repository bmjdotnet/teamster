package display

import (
	"strings"
	"testing"
)

func TestRenderDisplayParamHighlight(t *testing.T) {
	tagColor := [3]int{100, 200, 255}

	tests := []struct {
		name      string
		input     string
		wantInner string // the text that should appear without __ delimiters
	}{
		{"simple", "__file__", "file"},
		{"underscore_in_name", "__browser_navigate__", "browser_navigate"},
		{"multiple_underscores", "__a_b_c__", "a_b_c"},
		{"mcp_tool_display", "chrome-devtools(__read_page__)", "read_page"},
		{"mixed", "Created __entity_id__: __title__", "entity_id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := RenderDisplay(tt.input, tagColor, "salt")
			if strings.Contains(result, "__"+tt.wantInner+"__") {
				t.Errorf("literal __%s__ survived rendering (not highlighted)", tt.wantInner)
			}
			if !strings.Contains(result, tt.wantInner) {
				t.Errorf("inner text %q missing from rendered output", tt.wantInner)
			}
		})
	}
}
