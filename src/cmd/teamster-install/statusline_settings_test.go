package main

import "testing"

const testStatuslineBin = "/opt/teamster/lib/scripts/teamster-statusline.sh"

func TestApplyStatusLine_NoExisting_WritesTeamsterScript(t *testing.T) {
	settings := map[string]interface{}{}
	env := map[string]interface{}{}

	applyStatusLine(settings, env, "statusLine", testStatuslineBin, "TEAMSTER_STATUSLINE_CHAIN")

	sl, _ := settings["statusLine"].(map[string]interface{})
	if sl["command"] != testStatuslineBin {
		t.Fatalf("command = %v, want %s", sl["command"], testStatuslineBin)
	}
	if _, chained := env["TEAMSTER_STATUSLINE_CHAIN"]; chained {
		t.Fatal("no prior command existed — chain var should not be set")
	}
}

func TestApplyStatusLine_ExistingForeignCommand_ChainsIt(t *testing.T) {
	settings := map[string]interface{}{
		"statusLine": map[string]interface{}{
			"type":            "command",
			"command":         "/opt/other-tool/statusline.sh",
			"refreshInterval": float64(10),
		},
	}
	env := map[string]interface{}{}

	applyStatusLine(settings, env, "statusLine", testStatuslineBin, "TEAMSTER_STATUSLINE_CHAIN")

	if env["TEAMSTER_STATUSLINE_CHAIN"] != "/opt/other-tool/statusline.sh" {
		t.Fatalf("chain var = %v, want the operator's original command", env["TEAMSTER_STATUSLINE_CHAIN"])
	}
	sl, _ := settings["statusLine"].(map[string]interface{})
	if sl["command"] != testStatuslineBin {
		t.Fatalf("command = %v, want %s (must be replaced, not left as the original)", sl["command"], testStatuslineBin)
	}
}

func TestApplyStatusLine_AlreadyTeamsterScript_Idempotent(t *testing.T) {
	settings := map[string]interface{}{
		"statusLine": map[string]interface{}{
			"type":            "command",
			"command":         testStatuslineBin,
			"refreshInterval": float64(10),
		},
	}
	env := map[string]interface{}{}

	applyStatusLine(settings, env, "statusLine", testStatuslineBin, "TEAMSTER_STATUSLINE_CHAIN")

	if _, chained := env["TEAMSTER_STATUSLINE_CHAIN"]; chained {
		t.Fatal("re-running over our own script must not manufacture a chain var pointing at itself")
	}
	sl, _ := settings["statusLine"].(map[string]interface{})
	if sl["command"] != testStatuslineBin {
		t.Fatalf("command = %v, want %s", sl["command"], testStatuslineBin)
	}
}

func TestApplyStatusLine_MainAndSubagentUseDistinctChainVars(t *testing.T) {
	settings := map[string]interface{}{
		"statusLine": map[string]interface{}{
			"command": "/opt/other-tool/statusline.sh",
		},
		"subagentStatusLine": map[string]interface{}{
			"command": "/some/other/subagent-line.sh",
		},
	}
	env := map[string]interface{}{}

	applyStatusLine(settings, env, "statusLine", testStatuslineBin, "TEAMSTER_STATUSLINE_CHAIN")
	applyStatusLine(settings, env, "subagentStatusLine", testStatuslineBin, "TEAMSTER_SUBAGENT_STATUSLINE_CHAIN")

	if env["TEAMSTER_STATUSLINE_CHAIN"] != "/opt/other-tool/statusline.sh" {
		t.Fatalf("main chain var = %v, want /opt/other-tool/statusline.sh", env["TEAMSTER_STATUSLINE_CHAIN"])
	}
	if env["TEAMSTER_SUBAGENT_STATUSLINE_CHAIN"] != "/some/other/subagent-line.sh" {
		t.Fatalf("subagent chain var = %v, want /some/other/subagent-line.sh", env["TEAMSTER_SUBAGENT_STATUSLINE_CHAIN"])
	}

	sl, _ := settings["statusLine"].(map[string]interface{})
	subSl, _ := settings["subagentStatusLine"].(map[string]interface{})
	if sl["command"] != testStatuslineBin || subSl["command"] != testStatuslineBin {
		t.Fatalf("both slots must point at the teamster script: statusLine=%v subagentStatusLine=%v", sl["command"], subSl["command"])
	}
}

func TestIsTeamsterStatusline(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{testStatuslineBin, true},
		{"/some/other/basedir/lib/scripts/teamster-statusline.sh", true},
		{"/opt/other-tool/statusline.sh", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isTeamsterStatusline(tc.cmd, testStatuslineBin); got != tc.want {
			t.Errorf("isTeamsterStatusline(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}
