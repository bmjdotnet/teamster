// Command health-mcp is an MCP stdio server exposing agent health tools to
// agents. It reads JSON-RPC 2.0 requests from stdin and writes responses to
// stdout, backed by MySQL. Requires two store connections: the main store
// (for roster/session joins) and the gauge store (for agent_health_gauge).
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	gaugemysql "github.com/bmjdotnet/teamster/internal/agenthealth/gauge/mysql"
	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
	mcphealth "github.com/bmjdotnet/teamster/internal/mcp/health"
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
			fmt.Printf("health-mcp %s\n", version.String())
			os.Exit(0)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		os.Exit(1)
	}

	logger := logging.Init("health-mcp")

	s, err := store.Open(context.Background(), cfg.StoreDSN.Raw)
	if err != nil {
		logger.Error("opening store failed", "dsn", redact.Redact(cfg.StoreDSN.Raw), "error", err)
		os.Exit(1)
	}
	defer s.Close()

	drvDSN, err := toDriverDSN(cfg.StoreDSN.Raw)
	if err != nil {
		logger.Error("parse DSN for gauge DB", "error", err)
		os.Exit(1)
	}
	gaugeDB, err := sql.Open("mysql", drvDSN)
	if err != nil {
		logger.Error("open gauge DB", "error", err)
		os.Exit(1)
	}
	defer gaugeDB.Close()

	gs := gaugemysql.New(gaugeDB)

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
				"serverInfo":      map[string]interface{}{"name": "health-mcp", "version": version.Version},
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			})

		case "tools/list":
			respond(req.ID, map[string]interface{}{"tools": mcphealth.ToolDefs})

		case "tools/call":
			result, callErr := mcphealth.HandleToolCall(s, gs, nil, req.Params)
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

// toDriverDSN converts a mysql://user:pass@host:port/db URL to a
// go-sql-driver/mysql DSN string.
func toDriverDSN(raw string) (string, error) {
	if !strings.HasPrefix(raw, "mysql://") {
		return "", fmt.Errorf("DSN must start with mysql://")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	cfg := mysqldriver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Passwd = pw
		}
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Params = map[string]string{"time_zone": "'+00:00'"}
	for k, vs := range u.Query() {
		if len(vs) > 0 {
			cfg.Params[k] = vs[0]
		}
	}
	return cfg.FormatDSN(), nil
}
