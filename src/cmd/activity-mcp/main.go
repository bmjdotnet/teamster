// Command activity-mcp is an MCP stdio server exposing reportActivity,
// setOverallIntent, and completeActivity tools. All tools are no-ops that
// return confirmation strings — the hook client extracts the actual data.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bmjdotnet/teamster/internal/logging"
	"github.com/bmjdotnet/teamster/internal/mcp/activity"
	"github.com/bmjdotnet/teamster/internal/version"
)

// rpcRequest is the minimal JSON-RPC 2.0 request shape we need to parse.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func respond(id interface{}, result interface{}) {
	out, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
	fmt.Println(string(out))
}

func respondError(id interface{}, code int, message string) {
	out, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]interface{}{"code": code, "message": message},
	})
	fmt.Println(string(out))
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("activity-mcp %s\n", version.String())
			os.Exit(0)
		}
	}

	logging.Init("activity-mcp")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		if req.ID == nil {
			continue
		}

		switch req.Method {
		case "initialize":
			respond(req.ID, map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]interface{}{"name": "activity-mcp", "version": version.Version},
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			})

		case "tools/list":
			respond(req.ID, map[string]interface{}{
				"tools": activity.ToolDefs,
			})

		case "tools/call":
			text, callErr := activity.HandleToolCall(req.Params)
			if callErr != nil {
				respondError(req.ID, callErr.Code, callErr.Message)
			} else {
				respond(req.ID, activity.TextResult(text))
			}

		default:
			respondError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}
