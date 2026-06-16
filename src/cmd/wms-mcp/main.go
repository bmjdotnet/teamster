// Command wms-mcp is an MCP stdio server exposing work management operations
// to agents. It reads JSON-RPC 2.0 requests from stdin and writes responses to
// stdout, backed by MySQL.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
	mcpwms "github.com/bmjdotnet/teamster/internal/mcp/wms"
	"github.com/bmjdotnet/teamster/internal/redact"
	storemysql "github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/version"
	"github.com/bmjdotnet/teamster/internal/wms"
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
			fmt.Printf("wms-mcp %s\n", version.String())
			os.Exit(0)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "component", "wms-mcp", "error", err)
		os.Exit(1)
	}

	logger := logging.Init("wms-mcp")

	if cfg.StoreDSN.Driver != config.StoreDriverMySQL {
		logger.Error("TEAMSTER_STORE_DSN must be a mysql:// URL")
		os.Exit(1)
	}
	s, err := storemysql.New(cfg.StoreDSN.Primary,
		storemysql.WithRequireTagsOnDone(cfg.RequireTagsOnDone))
	if err != nil {
		logger.Error("opening store failed", "dsn", redact.Redact(cfg.StoreDSN.Primary), "error", err)
		os.Exit(1)
	}
	defer s.Close() //nolint:errcheck
	var store wms.Store = s

	// Reconcile the declared tag vocabulary (teamster.yaml `tags:`) into the
	// seed vocabulary before serving. Non-fatal: a bad vocab line must not stop
	// the MCP — log the error and proceed with whatever seeds the migrations
	// already established (honor no-silent-failures: log, don't swallow).
	if specs := cfg.TagSpecs(); len(specs) > 0 {
		if err := store.ReconcileVocabulary(context.Background(), specs); err != nil {
			logger.Error("reconciling tag vocabulary failed", "tag_keys", len(specs), "error", err)
		} else {
			logger.Info("reconciled tag vocabulary", "tag_keys", len(specs))
		}
	}

	eng := wms.NewEngine(store, nil)

	eng.AddObserver(wms.NewJournalObserver(store))

	if hookURL := os.Getenv("TEAMSTER_HOOK_SERVER_URL"); hookURL != "" {
		eng.AddObserver(wms.NewHookObserver(hookURL, cfg.Host))
	}

	sr := wms.NewJSONLSignalReader()
	classifier := wms.NewRuleClassifier(store, sr, cfg.LogFile)
	mcpwms.ActiveClassifier = classifier
	mcpwms.CreatorUser = cfg.User
	eng.AddObserver(wms.NewClassifierObserver(classifier))

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
				"serverInfo":      map[string]interface{}{"name": "wms-mcp", "version": version.Version},
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			})

		case "tools/list":
			respond(req.ID, map[string]interface{}{"tools": mcpwms.ToolDefs})

		case "tools/call":
			result, callErr := mcpwms.HandleToolCall(store, eng, req.Params)
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
