package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Stdio MCP server that wraps opencode-cli directly
// This is designed for use with Claude Desktop, Cursor, and other MCP clients
// that use stdio transport

const (
	defaultTimeout = 300 * time.Second
	defaultModel   = "github-copilot/gpt-5.2-codex" // Codex 5.2 model
)

// Model cache
var (
	availableModels []string
	modelCacheMu    sync.RWMutex
	modelCacheTime  time.Time
	modelCacheTTL   = 5 * time.Minute
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      any             `json:"id"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type toolCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

var target = getenv("MCP_TARGET", "opencode-cli")

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lshortfile)

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Exit(0)
	}()

	log.Printf("opencode-mcp stdio server started, target=%s", target)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024) // 10MB buffer

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var req mcpRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			writeError(nil, -32700, "invalid JSON")
			continue
		}

		log.Printf("Request: method=%s id=%v", req.Method, req.ID)
		handleRequest(req)
	}

	if err := scanner.Err(); err != nil {
		log.Printf("stdin error: %v", err)
	}
}

func handleRequest(req mcpRequest) {
	switch req.Method {
	case "initialize":
		writeResponse(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "opencode-mcp",
				"version": "0.1.0",
			},
		})

	case "notifications/initialized":
		// No response needed for notifications

	case "tools/list":
		writeResponse(req.ID, map[string]any{
			"tools": getTools(),
		})

	case "tools/call":
		handleToolsCall(req)

	default:
		writeError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func getTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "opencode_run",
			Description: "Run AI code assistant with a message. This is the main tool for code editing, analysis, and generation.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message": map[string]any{
						"type":        "string",
						"description": "The message/prompt to send to the AI assistant",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Project directory to work in",
					},
					"model": map[string]any{
						"type":        "string",
						"description": "Model to use (e.g., 'github-copilot/claude-sonnet-4')",
					},
					"files": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "File paths to attach to the message for context",
					},
				},
				"required": []string{"message"},
			},
		},
		{
			Name:        "opencode_models",
			Description: "List all available AI models",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "opencode_exec",
			Description: "Run any opencode-cli command with custom arguments",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"args": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Command arguments",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Working directory",
					},
				},
				"required": []string{"args"},
			},
		},
	}
}

func handleToolsCall(req mcpRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeError(req.ID, -32602, "invalid params")
		return
	}

	var cmdArgs []string
	var cwd string

	switch params.Name {
	case "opencode_run":
		var args struct {
			Message string   `json:"message"`
			Cwd     string   `json:"cwd"`
			Model   string   `json:"model"`
			Files   []string `json:"files"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			writeError(req.ID, -32602, "invalid arguments")
			return
		}
		if args.Message == "" {
			writeError(req.ID, -32602, "missing message")
			return
		}

		// Use default model if not specified
		model := args.Model
		if model == "" {
			model = getDefaultModel()
			log.Printf("Using default model: %s", model)
		}

		cmdArgs = []string{"run", "--format", "json", "--model", model}
		for _, f := range args.Files {
			cmdArgs = append(cmdArgs, "--file", f)
		}
		cmdArgs = append(cmdArgs, args.Message)
		cwd = args.Cwd

	case "opencode_models":
		cmdArgs = []string{"models"}

	case "opencode_exec":
		var args struct {
			Args []string `json:"args"`
			Cwd  string   `json:"cwd"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			writeError(req.ID, -32602, "invalid arguments")
			return
		}
		if len(args.Args) == 0 {
			writeError(req.ID, -32602, "missing args")
			return
		}
		cmdArgs = args.Args
		cwd = args.Cwd

	default:
		writeError(req.ID, -32602, fmt.Sprintf("unknown tool: %s", params.Name))
		return
	}

	// Execute command
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, target, cmdArgs...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeError(req.ID, -32000, err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		writeError(req.ID, -32000, err.Error())
		return
	}

	// For opencode_run, stream progress notifications and collect text
	var textCollector strings.Builder

	if params.Name == "opencode_run" {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				continue
			}

			eventType, _ := event["type"].(string)

			// Extract text content
			if eventType == "text" {
				if part, ok := event["part"].(map[string]any); ok {
					if text, ok := part["text"].(string); ok {
						textCollector.WriteString(text)

						// Send progress notification
						writeNotification("notifications/progress", map[string]any{
							"progressToken": req.ID,
							"progress":      textCollector.Len(),
							"message":       text,
						})
					}
				}
			}
		}
	} else {
		// For other tools, just read all output
		output, _ := io.ReadAll(stdout)
		textCollector.Write(output)
	}

	cmd.Wait()

	// Send final result
	result := toolCallResult{
		Content: []toolContent{{Type: "text", Text: textCollector.String()}},
		IsError: false,
	}
	writeResponse(req.ID, result)
}

func writeResponse(id any, result any) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	fmt.Println(string(data))
	log.Printf("Response: id=%v len=%d", id, len(data))
}

func writeError(id any, code int, message string) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpError{Code: code, Message: message},
	}
	data, _ := json.Marshal(resp)
	fmt.Println(string(data))
	log.Printf("Error: id=%v code=%d msg=%s", id, code, message)
}

func writeNotification(method string, params any) {
	notification := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	data, _ := json.Marshal(notification)
	fmt.Println(string(data))
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// fetchAvailableModels fetches and caches available models
func fetchAvailableModels() []string {
	modelCacheMu.RLock()
	if len(availableModels) > 0 && time.Since(modelCacheTime) < modelCacheTTL {
		models := availableModels
		modelCacheMu.RUnlock()
		return models
	}
	modelCacheMu.RUnlock()

	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()

	if len(availableModels) > 0 && time.Since(modelCacheTime) < modelCacheTTL {
		return availableModels
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, target, "models")
	output, err := cmd.Output()
	if err != nil {
		log.Printf("Failed to fetch models: %v", err)
		return nil
	}

	var models []string
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				models = append(models, parts[0])
			}
		}
	}

	if len(models) > 0 {
		availableModels = models
		modelCacheTime = time.Now()
		log.Printf("Cached %d available models", len(models))
	}

	return models
}

// getDefaultModel returns the best available model
func getDefaultModel() string {
	models := fetchAvailableModels()

	// Preferred models (tested to work with --format json)
	preferredModels := []string{
		"github-copilot/gpt-5.2-codex",
		"github-copilot/gpt-5.1-codex",
		"github-copilot/gpt-4o",
		"github-copilot/gpt-4.1",
	}

	for _, preferred := range preferredModels {
		for _, available := range models {
			if available == preferred {
				return available
			}
		}
	}

	// Return first github-copilot model
	for _, available := range models {
		if strings.HasPrefix(available, "github-copilot/") {
			return available
		}
	}

	if len(models) > 0 {
		return models[0]
	}

	return defaultModel
}
