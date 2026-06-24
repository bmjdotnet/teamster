package web

import "testing"

func TestRenderDisplayHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no params", "plain text", "plain text"},
		{"simple param", "Created __foo__", `Created <span class="param">foo</span>`},
		{"embedded underscore", "Updated __browser_navigate__",
			`Updated <span class="param">browser_navigate</span>`},
		{"multiple params", "__id__ → __status__",
			`<span class="param">id</span> → <span class="param">status</span>`},
		{"html escape", "__<script>__ safe", `<span class="param">&lt;script&gt;</span> safe`},
		{"no match bare underscores", "a_b_c", "a_b_c"},
		{"mcp tool format", `wms(__wms_setFocus__)`,
			`wms(<span class="param">wms_setFocus</span>)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderDisplayHTML(tt.in)
			if got != tt.want {
				t.Errorf("renderDisplayHTML(%q)\n got: %s\nwant: %s", tt.in, got, tt.want)
			}
		})
	}
}
