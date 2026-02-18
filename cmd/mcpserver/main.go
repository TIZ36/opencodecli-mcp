package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	defaultAddr       = ":9876"
	defaultTarget     = "opencode-cli"
	defaultTimeoutSec = 120
	defaultModel      = "github-copilot/gpt-5.2-codex" // Default model - Codex 5.2
)

// Available models cache
var (
	availableModels     []string
	availableModelsOnce sync.Once
	modelCacheMu        sync.RWMutex
	modelCacheTime      time.Time
	modelCacheTTL       = 5 * time.Minute
)

type serverConfig struct {
	Addr           string
	Target         string
	DefaultTimeout time.Duration
	DefaultModel   string
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      any             `json:"id"`
	Cwd     string          `json:"cwd,omitempty"`
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

type toolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type toolCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type execArgs struct {
	Args  []string `json:"args"`
	Cwd   string   `json:"cwd,omitempty"`
	Stdin string   `json:"stdin,omitempty"`
}

type execResponse struct {
	OK       bool   `json:"ok"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
}

type jsonResponseWriter struct {
	w io.Writer
}

func (j jsonResponseWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	trimmed := strings.TrimSpace(string(p))
	if trimmed == "" {
		return len(p), nil
	}
	_, err := fmt.Fprintln(j.w, trimmed)
	return len(p), err
}

// Tool names
const (
	toolExec        = "opencode_exec"
	toolRun         = "opencode_run"
	toolModels      = "opencode_models"
	toolSessionList = "opencode_session_list"
	toolAgentList   = "opencode_agent_list"
)

func main() {
	cfg := serverConfig{
		Addr:           getenv("MCP_ADDR", defaultAddr),
		Target:         getenv("MCP_TARGET", defaultTarget),
		DefaultTimeout: time.Duration(getenvInt("MCP_TIMEOUT_SEC", defaultTimeoutSec)) * time.Second,
		DefaultModel:   getenv("MCP_DEFAULT_MODEL", defaultModel),
	}

	log.Printf("=== opencode-mcp server starting ===")
	log.Printf("  MCP_ADDR:        %s", cfg.Addr)
	log.Printf("  MCP_TARGET:      %s", cfg.Target)
	log.Printf("  MCP_TIMEOUT_SEC: %d", int(cfg.DefaultTimeout.Seconds()))
	log.Printf("  MCP_DEFAULT_MODEL: %s", cfg.DefaultModel)
	log.Printf("  Endpoints:       POST /mcp (MCP), GET /health, POST /exec, POST /exec/stream")
	log.Printf("================================")

	// Pre-fetch available models in background
	go func() {
		fetchAvailableModels(cfg.Target)
	}()

	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Session store for MCP
	sessions := &sessionStore{sessions: make(map[string]*session)}

	// MCP endpoint - handles standard MCP protocol methods (Streamable HTTP)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		// Handle OPTIONS for endpoint discovery
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.Header().Set("Accept", "application/json")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req mcpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeMCPError(w, nil, -32700, "invalid JSON")
			return
		}
		if req.Method == "" {
			writeMCPError(w, req.ID, -32600, "missing method")
			return
		}

		log.Printf("[MCP] request method=%s id=%v", req.Method, req.ID)

		// Handle session
		sessionID := r.Header.Get("Mcp-Session-Id")
		var sess *session

		switch req.Method {
		case "initialize":
			// Create new session
			sess = sessions.create()
			sessionID = sess.id
			w.Header().Set("Mcp-Session-Id", sessionID)
			log.Printf("[MCP] initialize -> session=%s", sessionID)
			handleInitialize(w, req)
			return
		case "notifications/initialized":
			// Client notification, just acknowledge
			log.Printf("[MCP] notifications/initialized ack")
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			// Validate session for non-init requests
			if sessionID != "" {
				sess = sessions.get(sessionID)
			}
			// Allow requests without session for flexibility
		}

		if sess != nil {
			w.Header().Set("Mcp-Session-Id", sess.id)
		}

		switch req.Method {
		case "tools/list":
			log.Printf("[MCP] tools/list -> returning tool list")
			handleToolsList(w, req)
		case "tools/call":
			// Always use SSE for real-time streaming of opencode output
			handleToolsCallSSE(w, r.Context(), cfg, req)
		default:
			writeMCPError(w, req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
		}
	})

	// Direct exec endpoint (non-MCP, for convenience)
	mux.HandleFunc("/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req execArgs
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if len(req.Args) == 0 {
			http.Error(w, "missing args", http.StatusBadRequest)
			return
		}
		if err := validateCwd(req.Cwd); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), cfg.DefaultTimeout)
		defer cancel()

		stdout, stderr, exitCode, err := runCommand(ctx, cfg.Target, req.Args, req.Stdin, req.Cwd)
		resp := execResponse{
			OK:       err == nil,
			Stdout:   stdout,
			Stderr:   stderr,
			ExitCode: exitCode,
		}
		if err != nil {
			resp.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	// Stream exec endpoint
	mux.HandleFunc("/exec/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req execArgs
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if len(req.Args) == 0 {
			http.Error(w, "missing args", http.StatusBadRequest)
			return
		}
		if err := validateCwd(req.Cwd); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), cfg.DefaultTimeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, cfg.Target, req.Args...)
		cmd.Stdin = strings.NewReader(req.Stdin)
		if req.Cwd != "" {
			cmd.Dir = req.Cwd
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := cmd.Start(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		go func() {
			if err := copyStream(stderr, jsonResponseWriter{w: os.Stderr}); err != nil {
				log.Printf("stderr stream error: %v", err)
			}
		}()

		if err := streamLines(stdout, w, flusher); err != nil {
			log.Printf("stdout stream error: %v", err)
		}

		_ = cmd.Wait()
	})

	srv := &http.Server{
		Addr:         cfg.Addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
	}

	log.Printf("mcpserver listening on %s (ready)", cfg.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func handleInitialize(w http.ResponseWriter, req mcpRequest) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "opencode-mcp",
				"version": "0.1.0",
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleToolsList(w http.ResponseWriter, req mcpRequest) {
	tools := []mcpTool{
		{
			Name:        toolExec,
			Description: "Run any opencode-cli command with custom arguments. Use this for advanced operations.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"args": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Command arguments (e.g., ['run', '--model', 'gpt-4', 'Hello'])",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Working directory for the command",
					},
					"stdin": map[string]any{
						"type":        "string",
						"description": "Standard input to pass to the command",
					},
				},
				"required": []string{"args"},
			},
		},
		{
			Name:        toolRun,
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
					"session": map[string]any{
						"type":        "string",
						"description": "Session ID to continue a previous conversation",
					},
					"continue": map[string]any{
						"type":        "boolean",
						"description": "Continue the last session",
					},
					"files": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "File paths to attach to the message for context (relative to cwd or absolute)",
					},
				},
				"required": []string{"message"},
			},
		},
		{
			Name:        toolModels,
			Description: "List all available AI models",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        toolSessionList,
			Description: "List all saved sessions",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        toolAgentList,
			Description: "List all available agents",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}

	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: toolsListResult{
			Tools: tools,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleToolsCall(w http.ResponseWriter, ctx context.Context, cfg serverConfig, req mcpRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeMCPError(w, req.ID, -32602, "invalid params")
		return
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.DefaultTimeout)
	defer cancel()

	var stdout, stderr string
	var exitCode int
	var err error

	switch params.Name {
	case toolExec:
		var args execArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			writeMCPError(w, req.ID, -32602, "invalid arguments")
			return
		}
		if len(args.Args) == 0 {
			writeMCPError(w, req.ID, -32602, "missing args")
			return
		}
		if args.Cwd == "" {
			args.Cwd = req.Cwd
		}
		if err := validateCwd(args.Cwd); err != nil {
			writeMCPError(w, req.ID, -32602, err.Error())
			return
		}
		stdout, stderr, exitCode, err = runCommand(ctx, cfg.Target, args.Args, args.Stdin, args.Cwd)

	case toolRun:
		var runArgs struct {
			Message  string   `json:"message"`
			Cwd      string   `json:"cwd"`
			Model    string   `json:"model"`
			Session  string   `json:"session"`
			Continue bool     `json:"continue"`
			Files    []string `json:"files"`
		}
		if err := json.Unmarshal(params.Arguments, &runArgs); err != nil {
			writeMCPError(w, req.ID, -32602, "invalid arguments")
			return
		}
		if runArgs.Message == "" {
			writeMCPError(w, req.ID, -32602, "missing message")
			return
		}
		cwd := runArgs.Cwd
		if cwd == "" {
			cwd = req.Cwd
		}
		if err := validateCwd(cwd); err != nil {
			writeMCPError(w, req.ID, -32602, err.Error())
			return
		}

		// Use default model if not specified
		model := runArgs.Model
		if model == "" {
			model = getDefaultModel(cfg)
			if model != "" {
				log.Printf("Using default model: %s", model)
			}
		}

		cmdArgs := []string{"run", "--format", "json"}
		if model != "" {
			cmdArgs = append(cmdArgs, "--model", model)
		}
		if runArgs.Session != "" {
			cmdArgs = append(cmdArgs, "--session", runArgs.Session)
		}
		if runArgs.Continue {
			cmdArgs = append(cmdArgs, "--continue")
		}
		for _, file := range runArgs.Files {
			cmdArgs = append(cmdArgs, "--file", file)
		}
		cmdArgs = append(cmdArgs, runArgs.Message)
		stdout, stderr, exitCode, err = runCommand(ctx, cfg.Target, cmdArgs, "", cwd)

	case toolModels:
		stdout, stderr, exitCode, err = runCommand(ctx, cfg.Target, []string{"models"}, "", "")

	case toolSessionList:
		stdout, stderr, exitCode, err = runCommand(ctx, cfg.Target, []string{"session", "list"}, "", "")

	case toolAgentList:
		stdout, stderr, exitCode, err = runCommand(ctx, cfg.Target, []string{"agent", "list"}, "", "")

	default:
		writeMCPError(w, req.ID, -32602, fmt.Sprintf("unknown tool: %s", params.Name))
		return
	}

	// Build result
	resultText := stdout

	// For toolRun, parse the JSON event stream to extract readable text
	if params.Name == toolRun && stdout != "" {
		parsed := parseJSONEventStream(stdout)
		if parsed != "" {
			resultText = parsed
		}
	}

	if stderr != "" {
		resultText += "\n[stderr]\n" + stderr
	}
	if err != nil {
		resultText += fmt.Sprintf("\n[exit code: %d]", exitCode)
	}

	result := toolCallResult{
		Content: []toolContent{{Type: "text", Text: resultText}},
		IsError: err != nil && exitCode != 0,
	}

	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func runCommand(ctx context.Context, target string, args []string, stdin, cwd string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, target, args...)
	cmd.Stdin = strings.NewReader(stdin)
	if cwd != "" {
		cmd.Dir = cwd
	}
	stdout, err := cmd.Output()
	if err == nil {
		return string(stdout), "", 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(stdout), string(exitErr.Stderr), exitErr.ExitCode(), fmt.Errorf("command failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
	}
	return "", "", -1, err

}

func writeMCPError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &mcpError{
			Code:    code,
			Message: message,
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func streamLines(r io.Reader, w io.Writer, flusher http.Flusher) error {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := strings.TrimSpace(string(buf[:n]))
			if chunk != "" {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func copyStream(r io.Reader, w io.Writer) error {
	_, err := io.Copy(w, r)
	return err
}

func validateCwd(cwd string) error {
	if cwd == "" {
		return nil
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("invalid cwd: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("invalid cwd: not a directory")
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var out int
		_, err := fmt.Sscanf(v, "%d", &out)
		if err == nil {
			return out
		}
	}
	return def
}

// Session management for MCP
type session struct {
	id        string
	createdAt time.Time
}

type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*session
}

func (s *sessionStore) create() *session {
	id := generateSessionID()
	sess := &session{
		id:        id,
		createdAt: time.Now(),
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return sess
}

func (s *sessionStore) get(id string) *session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

func generateSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fetchAvailableModels fetches and caches the list of available models
func fetchAvailableModels(target string) []string {
	modelCacheMu.RLock()
	if len(availableModels) > 0 && time.Since(modelCacheTime) < modelCacheTTL {
		models := availableModels
		modelCacheMu.RUnlock()
		return models
	}
	modelCacheMu.RUnlock()

	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()

	// Double-check after acquiring write lock
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
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "Available") {
			// Extract model ID (first column or whole line)
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

// getDefaultModel returns the best available model, or empty string to let opencode use its default.
// When fetchAvailableModels fails (e.g., wrong opencode binary), we return "" to avoid ProviderModelNotFoundError.
func getDefaultModel(cfg serverConfig) string {
	models := fetchAvailableModels(cfg.Target)

	// Preferred models in order (provider/model format per opencode.ai docs)
	preferredModels := []string{
		"github-copilot/gpt-5.2-codex",
		"github-copilot/gpt-5.1-codex",
		"opencode/gpt-5.2-codex",
		"opencode/gpt-5.1-codex",
		"github-copilot/gpt-4o",
		"github-copilot/claude-sonnet-4.5",
	}

	for _, preferred := range preferredModels {
		for _, available := range models {
			if available == preferred {
				log.Printf("Selected preferred model: %s", available)
				return available
			}
		}
	}

	for _, preferred := range preferredModels {
		for _, available := range models {
			if strings.Contains(available, preferred) {
				log.Printf("Selected partial match model: %s", available)
				return available
			}
		}
	}

	for _, available := range models {
		if strings.HasPrefix(available, "github-copilot/") || strings.HasPrefix(available, "opencode/") {
			log.Printf("Selected first available model: %s", available)
			return available
		}
	}

	if len(models) > 0 {
		log.Printf("Selected first available model: %s", models[0])
		return models[0]
	}

	// Don't use hardcoded fallback - let opencode use its own default to avoid ProviderModelNotFoundError
	log.Printf("No models from 'opencode models', omitting --model (opencode will use its default)")
	return ""
}

// sendProgress sends MCP notifications/progress for real-time client display
func sendProgress(w io.Writer, flusher http.Flusher, id any, progress int, message string) {
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params": map[string]any{
			"progressToken": id,
			"progress":      progress,
			"message":       message,
		},
	}
	b, _ := json.Marshal(notif)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	if flusher != nil {
		flusher.Flush()
	}
}

// truncateForLog returns s truncated to maxLen with "..." if longer
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// SSE streaming for tools/call
func handleToolsCallSSE(w http.ResponseWriter, ctx context.Context, cfg serverConfig, req mcpRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		log.Printf("[tools/call] invalid params: %v", err)
		writeMCPError(w, req.ID, -32602, "invalid params")
		return
	}

	log.Printf("[tools/call] tool=%s id=%v", params.Name, req.ID)

	// Build command args based on tool
	var cmdArgs []string
	var cwd string
	var stdin string

	switch params.Name {
	case toolExec:
		var args execArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			writeMCPError(w, req.ID, -32602, "invalid arguments")
			return
		}
		if len(args.Args) == 0 {
			writeMCPError(w, req.ID, -32602, "missing args")
			return
		}
		cmdArgs = args.Args
		cwd = args.Cwd
		stdin = args.Stdin
		log.Printf("[tools/call] exec args=%v cwd=%q", args.Args, cwd)

	case toolRun:
		var runArgs struct {
			Message  string   `json:"message"`
			Cwd      string   `json:"cwd"`
			Model    string   `json:"model"`
			Session  string   `json:"session"`
			Continue bool     `json:"continue"`
			Files    []string `json:"files"`
		}
		if err := json.Unmarshal(params.Arguments, &runArgs); err != nil {
			writeMCPError(w, req.ID, -32602, "invalid arguments")
			return
		}
		if runArgs.Message == "" {
			writeMCPError(w, req.ID, -32602, "missing message")
			return
		}

		// Use default model if not specified
		model := runArgs.Model
		if model == "" {
			model = getDefaultModel(cfg)
			if model != "" {
				log.Printf("SSE: Using default model: %s", model)
			}
		}

		cmdArgs = []string{"run", "--format", "json"}
		if model != "" {
			cmdArgs = append(cmdArgs, "--model", model)
		}
		if runArgs.Session != "" {
			cmdArgs = append(cmdArgs, "--session", runArgs.Session)
		}
		if runArgs.Continue {
			cmdArgs = append(cmdArgs, "--continue")
		}
		for _, file := range runArgs.Files {
			cmdArgs = append(cmdArgs, "--file", file)
		}
		cmdArgs = append(cmdArgs, runArgs.Message)
		cwd = runArgs.Cwd
		log.Printf("[tools/call] run message=%s model=%s cwd=%q session=%s files=%v",
			truncateForLog(runArgs.Message, 80), model, cwd, runArgs.Session, runArgs.Files)

	case toolModels:
		cmdArgs = []string{"models"}
		log.Printf("[tools/call] models")

	case toolSessionList:
		cmdArgs = []string{"session", "list"}
		log.Printf("[tools/call] session list")

	case toolAgentList:
		cmdArgs = []string{"agent", "list"}
		log.Printf("[tools/call] agent list")

	default:
		writeMCPError(w, req.ID, -32602, fmt.Sprintf("unknown tool: %s", params.Name))
		return
	}

	if cwd == "" {
		cwd = req.Cwd
	}
	if err := validateCwd(cwd); err != nil {
		writeMCPError(w, req.ID, -32602, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.DefaultTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cfg.Target, cmdArgs...)
	cmd.Stdin = strings.NewReader(stdin)
	if cwd != "" {
		cmd.Dir = cwd
	}

	log.Printf("[tools/call] exec: %s %s (cwd=%q)", cfg.Target, strings.Join(cmdArgs, " "), cwd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeMCPError(w, req.ID, -32000, err.Error())
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		writeMCPError(w, req.ID, -32000, err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		writeMCPError(w, req.ID, -32000, err.Error())
		return
	}

	// SSE response - disable buffering for real-time streaming
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: disable proxy buffering
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeMCPError(w, req.ID, -32000, "streaming unsupported")
		return
	}

	// Collect stderr in background
	var stderrBuf strings.Builder
	go func() {
		_, _ = io.Copy(&stderrBuf, stderrPipe)
	}()

	// Collect text and tool outputs for final response
	var textCollector strings.Builder
	var toolOutputs []string
	var eventCount int
	eventTypeCounts := make(map[string]int)

	// Stream stdout line by line for better JSON event handling
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer for large JSON lines
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// For opencode_run with --format json, parse and extract useful info
		if params.Name == toolRun {
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err == nil {
				eventType, _ := event["type"].(string)
				eventData := extractEventData(event)
				eventTypeCounts[eventType]++
				eventCount++

				// Log every event with step details for observability
				switch eventType {
				case "text":
					if text, ok := eventData.(string); ok {
						log.Printf("[stream] event#%d type=text len=%d", eventCount, len(text))
						log.Printf("[stream]   content: %s", truncateForLog(text, 300))
					}
				case "tool_use":
					if m, ok := eventData.(map[string]any); ok {
						toolName, _ := m["tool"].(string)
						status, _ := m["status"].(string)
						inputPreview := ""
						if input, ok := m["input"].(map[string]any); ok {
							inputJSON, _ := json.Marshal(input)
							inputPreview = truncateForLog(string(inputJSON), 200)
						}
						outputPreview := ""
						switch out := m["output"].(type) {
						case string:
							outputPreview = truncateForLog(out, 300)
						default:
							if out != nil {
								b, _ := json.Marshal(out)
								outputPreview = truncateForLog(string(b), 300)
							}
						}
						log.Printf("[stream] event#%d type=tool_use tool=%s status=%s", eventCount, toolName, status)
						if inputPreview != "" {
							log.Printf("[stream]   input:  %s", inputPreview)
						}
						if outputPreview != "" {
							log.Printf("[stream]   output: %s", outputPreview)
						}
					}
				case "step_start":
					if part, ok := event["part"].(map[string]any); ok {
						reason, _ := part["reason"].(string)
						snapshot, _ := part["snapshot"].(string)
						partType, _ := part["type"].(string)
						log.Printf("[stream] event#%d type=step_start reason=%q partType=%s snapshot=%s",
							eventCount, reason, partType, truncateForLog(snapshot, 12))
					} else {
						log.Printf("[stream] event#%d type=step_start", eventCount)
					}
				case "step_finish":
					if part, ok := event["part"].(map[string]any); ok {
						reason, _ := part["reason"].(string)
						snapshot, _ := part["snapshot"].(string)
						cost, _ := part["cost"].(float64)
						tokens, _ := part["tokens"].(map[string]any)
						log.Printf("[stream] event#%d type=step_finish reason=%q cost=$%.4f", eventCount, reason, cost)
						if tokens != nil {
							in, _ := tokens["input"].(float64)
							out, _ := tokens["output"].(float64)
							log.Printf("[stream]   tokens: input=%.0f output=%.0f snapshot=%s", in, out, truncateForLog(snapshot, 12))
						}
					} else {
						log.Printf("[stream] event#%d type=step_finish", eventCount)
					}
				default:
					log.Printf("[stream] event#%d type=%s", eventCount, eventType)
				}

				// Collect text and tool outputs for final response
				if eventType == "text" {
					if text, ok := eventData.(string); ok {
						textCollector.WriteString(text)
						// Send progress with accumulated text for real-time display
						sendProgress(w, flusher, req.ID, eventCount, textCollector.String())
					}
				} else if eventType == "tool_use" {
					if m, ok := eventData.(map[string]any); ok {
						toolName, _ := m["tool"].(string)
						status, _ := m["status"].(string)
						if status == "completed" {
							if toolName != "" {
								if output, ok := m["output"].(string); ok && output != "" {
									toolOutputs = append(toolOutputs, fmt.Sprintf("[Tool: %s]\n%s", toolName, output))
								}
							}
							// Progress: tool completed (user sees activity)
							sendProgress(w, flusher, req.ID, eventCount, fmt.Sprintf("Tool %s completed", toolName))
						}
					}
				} else if eventType == "step_start" || eventType == "step_finish" {
					// Progress: step update (user sees activity)
					if m, ok := eventData.(map[string]any); ok {
						reason, _ := m["reason"].(string)
						msg := eventType
						if reason != "" {
							msg = fmt.Sprintf("%s: %s", eventType, reason)
						}
						sendProgress(w, flusher, req.ID, eventCount, msg)
					}
				}

				// Stream event to client
				notification := map[string]any{
					"jsonrpc": "2.0",
					"method":  "notifications/message",
					"params": map[string]any{
						"type": eventType,
						"data": eventData,
					},
				}
				eventJSON, _ := json.Marshal(notification)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", eventJSON)
				flusher.Flush()
				continue
			}
		}

		// Generic: send raw line (for models, session list, exec, or non-JSON toolRun output)
		eventCount++
		log.Printf("[stream] raw#%d len=%d preview=%s", eventCount, len(line), truncateForLog(line, 150))
		textCollector.WriteString(line)
		textCollector.WriteString("\n")
		notification := map[string]any{
			"jsonrpc": "2.0",
			"method":  "notifications/progress",
			"params": map[string]any{
				"data": line,
			},
		}
		eventJSON, _ := json.Marshal(notification)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", eventJSON)
		flusher.Flush()
	}

	exitCode := 0
	waitErr := cmd.Wait()
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}

	// Build final result: text + tool outputs (for completeness)
	resultText := textCollector.String()
	if len(toolOutputs) > 0 {
		if resultText != "" {
			resultText += "\n\n--- Tool Outputs ---\n"
		} else {
			resultText = "--- Tool Outputs ---\n"
		}
		resultText += strings.Join(toolOutputs, "\n\n")
	}
	stderrStr := stderrBuf.String()
	if stderrStr != "" {
		if resultText != "" {
			resultText += "\n\n"
		}
		resultText += "[stderr]\n" + stderrStr
	}
	if exitCode != 0 {
		resultText += fmt.Sprintf("\n[exit code: %d]", exitCode)
	}

	// Log completion summary
	if len(eventTypeCounts) > 0 {
		counts := make([]string, 0, len(eventTypeCounts))
		for k, v := range eventTypeCounts {
			counts = append(counts, fmt.Sprintf("%s=%d", k, v))
		}
		log.Printf("[tools/call] done tool=%s events=%d counts=%v resultLen=%d exitCode=%d stderrLen=%d",
			params.Name, eventCount, counts, len(resultText), exitCode, len(stderrStr))
	} else {
		log.Printf("[tools/call] done tool=%s lines=%d resultLen=%d exitCode=%d stderrLen=%d",
			params.Name, eventCount, len(resultText), exitCode, len(stderrStr))
	}
	log.Printf("[tools/call] result preview: %s", truncateForLog(resultText, 200))

	result := toolCallResult{
		Content: []toolContent{{Type: "text", Text: resultText}},
		IsError: exitCode != 0,
	}

	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
	respJSON, _ := json.Marshal(resp)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", respJSON)
	flusher.Flush()
}

// extractEventData extracts readable content from opencode-cli JSON events
func extractEventData(event map[string]any) any {
	eventType, _ := event["type"].(string)
	part, ok := event["part"].(map[string]any)
	if !ok {
		return event
	}

	switch eventType {
	case "text":
		if text, ok := part["text"].(string); ok {
			return text
		}
	case "tool_use":
		toolName, _ := part["tool"].(string)
		if state, ok := part["state"].(map[string]any); ok {
			status, _ := state["status"].(string)
			result := map[string]any{
				"tool":   toolName,
				"status": status,
			}
			if input, ok := state["input"].(map[string]any); ok {
				result["input"] = input
			}
			if output, ok := state["output"]; ok {
				result["output"] = output
			}
			if errMsg, ok := state["error"].(string); ok && errMsg != "" {
				result["error"] = errMsg
			}
			return result
		}
		return map[string]any{"tool": toolName, "status": "unknown"}
	case "step_start":
		reason, _ := part["reason"].(string)
		return map[string]any{"type": "step_start", "reason": reason}
	case "step_finish":
		reason, _ := part["reason"].(string)
		return map[string]any{"type": "step_finish", "reason": reason}
	}

	return event
}

// parseJSONEventStream parses opencode-cli JSON event stream and extracts readable text.
// Preserves step_start, step_finish, tool_use (all states: in_progress, completed, error).
func parseJSONEventStream(jsonLines string) string {
	var parts []string

	lines := strings.Split(jsonLines, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		part, ok := event["part"].(map[string]any)
		if !ok {
			continue
		}

		switch eventType {
		case "text":
			if text, ok := part["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		case "step_start":
			if reason, ok := part["reason"].(string); ok && reason != "" {
				parts = append(parts, fmt.Sprintf("\n[Step started: %s]\n", reason))
			}
		case "step_finish":
			if reason, ok := part["reason"].(string); ok && reason != "" {
				parts = append(parts, fmt.Sprintf("[Step finished: %s]\n", reason))
			}
		case "tool_use":
			toolName, _ := part["tool"].(string)
			if state, ok := part["state"].(map[string]any); ok {
				status, _ := state["status"].(string)
				switch status {
				case "in_progress":
					parts = append(parts, fmt.Sprintf("[Tool: %s] running...\n", toolName))
				case "completed":
					if output, ok := state["output"].(string); ok && output != "" {
						parts = append(parts, fmt.Sprintf("[Tool: %s]\n%s\n", toolName, output))
					}
				case "error":
					errMsg, _ := state["error"].(string)
					if errMsg == "" {
						errMsg = "unknown error"
					}
					parts = append(parts, fmt.Sprintf("[Tool: %s] error: %s\n", toolName, errMsg))
				}
			}
		}
	}

	return strings.Join(parts, "")
}
