// Command roster-mcp is an MCP stdio server exposing agent roster tools to
// agents. It reads JSON-RPC 2.0 requests from stdin and writes responses to
// stdout, backed by MySQL.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
	mcproster "github.com/bmjdotnet/teamster/internal/mcp/roster"
	"github.com/bmjdotnet/teamster/internal/redact"
	"github.com/bmjdotnet/teamster/internal/store"
	_ "github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/version"
)

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
			fmt.Printf("roster-mcp %s\n", version.String())
			os.Exit(0)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		os.Exit(1)
	}

	logger := logging.Init("roster-mcp")

	s, err := store.Open(context.Background(), cfg.StoreDSN.Raw)
	if err != nil {
		logger.Error("opening store failed", "dsn", redact.Redact(cfg.StoreDSN.Raw), "error", err)
		os.Exit(1)
	}
	defer s.Close()

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
				"serverInfo":      map[string]interface{}{"name": "roster-mcp", "version": version.Version},
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			})

		case "tools/list":
			respond(req.ID, map[string]interface{}{"tools": mcproster.ToolDefs})

		case "tools/call":
			result, callErr := mcproster.HandleToolCall(s, req.Params)
			if callErr != nil {
				respondError(req.ID, callErr.Code, callErr.Message)
			} else {
				respond(req.ID, result)
			}

		default:
			respondError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}
