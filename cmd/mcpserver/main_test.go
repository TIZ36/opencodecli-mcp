package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Test helpers
func createTestServer(t *testing.T) (*http.Server, *http.ServeMux, serverConfig) {
	t.Helper()
	cfg := serverConfig{
		Addr:           ":0",
		Target:         "echo", // Use echo as a mock command
		DefaultTimeout: 5 * time.Second,
	}
	mux := http.NewServeMux()
	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: mux,
	}
	return srv, mux, cfg
}

func doMCPRequest(t *testing.T, handler http.HandlerFunc, method string, id any, params any) mcpResponse {
	t.Helper()
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"id":      id,
	}
	if params != nil {
		paramsJSON, _ := json.Marshal(params)
		reqBody["params"] = json.RawMessage(paramsJSON)
	}

	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var resp mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v, body: %s", err, rec.Body.String())
	}
	return resp
}

// Test validateCwd
func TestValidateCwd(t *testing.T) {
	tests := []struct {
		name    string
		cwd     string
		wantErr bool
	}{
		{
			name:    "empty cwd is valid",
			cwd:     "",
			wantErr: false,
		},
		{
			name:    "valid directory",
			cwd:     os.TempDir(),
			wantErr: false,
		},
		{
			name:    "non-existent directory",
			cwd:     "/nonexistent/path/that/does/not/exist",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCwd(tt.cwd)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateCwd(%q) error = %v, wantErr %v", tt.cwd, err, tt.wantErr)
			}
		})
	}

	// Test with a file (not a directory)
	t.Run("file instead of directory", func(t *testing.T) {
		tmpFile, err := os.CreateTemp("", "test")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpFile.Name())
		tmpFile.Close()

		err = validateCwd(tmpFile.Name())
		if err == nil {
			t.Error("validateCwd with file should return error")
		}
	})
}

// Test getenv
func TestGetenv(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		def    string
		setEnv bool
		envVal string
		want   string
	}{
		{
			name:   "returns default when not set",
			key:    "TEST_GETENV_UNSET",
			def:    "default",
			setEnv: false,
			want:   "default",
		},
		{
			name:   "returns env value when set",
			key:    "TEST_GETENV_SET",
			def:    "default",
			setEnv: true,
			envVal: "custom",
			want:   "custom",
		},
		{
			name:   "returns default for empty env",
			key:    "TEST_GETENV_EMPTY",
			def:    "default",
			setEnv: true,
			envVal: "",
			want:   "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				os.Setenv(tt.key, tt.envVal)
				defer os.Unsetenv(tt.key)
			}
			if got := getenv(tt.key, tt.def); got != tt.want {
				t.Errorf("getenv(%q, %q) = %q, want %q", tt.key, tt.def, got, tt.want)
			}
		})
	}
}

// Test getenvInt
func TestGetenvInt(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		def    int
		setEnv bool
		envVal string
		want   int
	}{
		{
			name:   "returns default when not set",
			key:    "TEST_GETENV_INT_UNSET",
			def:    42,
			setEnv: false,
			want:   42,
		},
		{
			name:   "returns parsed int when set",
			key:    "TEST_GETENV_INT_SET",
			def:    42,
			setEnv: true,
			envVal: "100",
			want:   100,
		},
		{
			name:   "returns default for invalid int",
			key:    "TEST_GETENV_INT_INVALID",
			def:    42,
			setEnv: true,
			envVal: "not_a_number",
			want:   42,
		},
		{
			name:   "returns default for empty env",
			key:    "TEST_GETENV_INT_EMPTY",
			def:    42,
			setEnv: true,
			envVal: "",
			want:   42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				os.Setenv(tt.key, tt.envVal)
				defer os.Unsetenv(tt.key)
			}
			if got := getenvInt(tt.key, tt.def); got != tt.want {
				t.Errorf("getenvInt(%q, %d) = %d, want %d", tt.key, tt.def, got, tt.want)
			}
		})
	}
}

// Test generateSessionID
func TestGenerateSessionID(t *testing.T) {
	id1 := generateSessionID()
	id2 := generateSessionID()

	// Check length (16 bytes = 32 hex chars)
	if len(id1) != 32 {
		t.Errorf("session ID length = %d, want 32", len(id1))
	}

	// Check uniqueness
	if id1 == id2 {
		t.Error("generated session IDs should be unique")
	}

	// Check hex format
	for _, c := range id1 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("session ID contains non-hex character: %c", c)
		}
	}
}

// Test sessionStore
func TestSessionStore(t *testing.T) {
	store := &sessionStore{sessions: make(map[string]*session)}

	// Create session
	sess1 := store.create()
	if sess1 == nil {
		t.Fatal("create() returned nil")
	}
	if sess1.id == "" {
		t.Error("session ID is empty")
	}

	// Get existing session
	retrieved := store.get(sess1.id)
	if retrieved != sess1 {
		t.Error("get() didn't return the same session")
	}

	// Get non-existent session
	nonExistent := store.get("nonexistent")
	if nonExistent != nil {
		t.Error("get() should return nil for non-existent session")
	}

	// Create multiple sessions
	sess2 := store.create()
	if sess2.id == sess1.id {
		t.Error("session IDs should be unique")
	}
}

// Test extractEventData
func TestExtractEventData(t *testing.T) {
	tests := []struct {
		name  string
		event map[string]any
		check func(t *testing.T, result any)
	}{
		{
			name: "text event",
			event: map[string]any{
				"type": "text",
				"part": map[string]any{
					"text": "Hello, world!",
				},
			},
			check: func(t *testing.T, result any) {
				if result != "Hello, world!" {
					t.Errorf("expected 'Hello, world!', got %v", result)
				}
			},
		},
		{
			name: "tool_use event",
			event: map[string]any{
				"type": "tool_use",
				"part": map[string]any{
					"tool": "read_file",
					"state": map[string]any{
						"status": "completed",
						"input":  map[string]any{"path": "/tmp/test.txt"},
						"output": "file contents",
					},
				},
			},
			check: func(t *testing.T, result any) {
				m, ok := result.(map[string]any)
				if !ok {
					t.Fatalf("expected map, got %T", result)
				}
				if m["tool"] != "read_file" {
					t.Errorf("expected tool 'read_file', got %v", m["tool"])
				}
				if m["status"] != "completed" {
					t.Errorf("expected status 'completed', got %v", m["status"])
				}
			},
		},
		{
			name: "step_start event",
			event: map[string]any{
				"type": "step_start",
				"part": map[string]any{
					"reason": "user_request",
				},
			},
			check: func(t *testing.T, result any) {
				m, ok := result.(map[string]any)
				if !ok {
					t.Fatalf("expected map, got %T", result)
				}
				if m["type"] != "step_start" {
					t.Errorf("expected type 'step_start', got %v", m["type"])
				}
			},
		},
		{
			name: "event without part",
			event: map[string]any{
				"type": "unknown",
				"data": "something",
			},
			check: func(t *testing.T, result any) {
				m, ok := result.(map[string]any)
				if !ok {
					t.Fatalf("expected map, got %T", result)
				}
				if m["type"] != "unknown" {
					t.Errorf("expected original event to be returned")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractEventData(tt.event)
			tt.check(t, result)
		})
	}
}

// Test health endpoint
func TestHealthEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status code = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

// Test MCP OPTIONS endpoint
func TestMCPOptionsEndpoint(t *testing.T) {
	sessions := &sessionStore{sessions: make(map[string]*session)}
	handler := createMCPHandler(sessions, serverConfig{})

	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status code = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if allow := rec.Header().Get("Allow"); allow != "POST, OPTIONS" {
		t.Errorf("Allow header = %q, want %q", allow, "POST, OPTIONS")
	}
}

// Test MCP initialize
func TestMCPInitialize(t *testing.T) {
	sessions := &sessionStore{sessions: make(map[string]*session)}
	handler := createMCPHandler(sessions, serverConfig{})

	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"id":      1,
		"params":  map[string]any{},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status code = %d, want %d", rec.Code, http.StatusOK)
	}

	// Check session ID header
	sessionID := rec.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Error("Mcp-Session-Id header not set")
	}

	var resp mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("result is not a map")
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want %v", result["protocolVersion"], "2024-11-05")
	}
}

// Test MCP tools/list
func TestMCPToolsList(t *testing.T) {
	sessions := &sessionStore{sessions: make(map[string]*session)}
	handler := createMCPHandler(sessions, serverConfig{})

	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/list",
		"id":      1,
		"params":  map[string]any{},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status code = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("result is not a map")
	}

	toolsRaw, ok := result["tools"].([]any)
	if !ok {
		t.Fatal("tools is not an array")
	}

	// Check expected tools
	expectedTools := map[string]bool{
		toolExec:        false,
		toolRun:         false,
		toolModels:      false,
		toolSessionList: false,
		toolAgentList:   false,
	}

	for _, toolRaw := range toolsRaw {
		tool, ok := toolRaw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		if _, exists := expectedTools[name]; exists {
			expectedTools[name] = true
		}
	}

	for name, found := range expectedTools {
		if !found {
			t.Errorf("expected tool %q not found in tools/list", name)
		}
	}
}

// Test MCP error responses
func TestMCPErrors(t *testing.T) {
	sessions := &sessionStore{sessions: make(map[string]*session)}
	handler := createMCPHandler(sessions, serverConfig{})

	tests := []struct {
		name     string
		body     string
		wantCode int
		wantMsg  string
	}{
		{
			name:     "invalid JSON",
			body:     "not json",
			wantCode: -32700,
			wantMsg:  "invalid JSON",
		},
		{
			name:     "missing method",
			body:     `{"jsonrpc":"2.0","id":1}`,
			wantCode: -32600,
			wantMsg:  "missing method",
		},
		{
			name:     "unknown method",
			body:     `{"jsonrpc":"2.0","method":"unknown/method","id":1}`,
			wantCode: -32601,
			wantMsg:  "method not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			var resp mcpResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}

			if resp.Error == nil {
				t.Fatal("expected error response")
			}
			if resp.Error.Code != tt.wantCode {
				t.Errorf("error code = %d, want %d", resp.Error.Code, tt.wantCode)
			}
			if !strings.Contains(resp.Error.Message, tt.wantMsg) {
				t.Errorf("error message = %q, want containing %q", resp.Error.Message, tt.wantMsg)
			}
		})
	}
}

// Test runCommand
func TestRunCommand(t *testing.T) {
	ctx := context.Background()

	t.Run("successful command", func(t *testing.T) {
		stdout, stderr, exitCode, err := runCommand(ctx, "echo", []string{"hello"}, "", "")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exitCode = %d, want 0", exitCode)
		}
		if strings.TrimSpace(stdout) != "hello" {
			t.Errorf("stdout = %q, want %q", stdout, "hello")
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})

	t.Run("command with working directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		stdout, _, _, err := runCommand(ctx, "pwd", nil, "", tmpDir)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if strings.TrimSpace(stdout) != tmpDir {
			t.Errorf("stdout = %q, want %q", strings.TrimSpace(stdout), tmpDir)
		}
	})

	t.Run("command with stdin", func(t *testing.T) {
		stdout, _, _, err := runCommand(ctx, "cat", nil, "test input", "")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if stdout != "test input" {
			t.Errorf("stdout = %q, want %q", stdout, "test input")
		}
	})

	t.Run("failing command", func(t *testing.T) {
		_, _, exitCode, err := runCommand(ctx, "false", nil, "", "")
		if err == nil {
			t.Error("expected error for failing command")
		}
		if exitCode == 0 {
			t.Errorf("exitCode = %d, want non-zero", exitCode)
		}
	})

	t.Run("context timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		_, _, _, err := runCommand(ctx, "sleep", []string{"10"}, "", "")
		if err == nil {
			t.Error("expected error for timeout")
		}
	})
}

// Test jsonResponseWriter
func TestJsonResponseWriter(t *testing.T) {
	var buf bytes.Buffer
	w := jsonResponseWriter{w: &buf}

	// Empty write
	n, err := w.Write([]byte{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}

	// Whitespace write
	n, err = w.Write([]byte("   \n\t  "))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty buffer for whitespace, got %q", buf.String())
	}

	// Normal write
	buf.Reset()
	n, err = w.Write([]byte("  hello world  "))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "hello world") {
		t.Errorf("buffer = %q, want containing 'hello world'", buf.String())
	}
}

// Test writeMCPError
func TestWriteMCPError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeMCPError(rec, 42, -32000, "test error")

	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), "application/json")
	}

	var resp mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want %q", resp.JSONRPC, "2.0")
	}
	if resp.ID != float64(42) { // JSON numbers are float64
		t.Errorf("id = %v, want 42", resp.ID)
	}
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if resp.Error.Code != -32000 {
		t.Errorf("error code = %d, want %d", resp.Error.Code, -32000)
	}
	if resp.Error.Message != "test error" {
		t.Errorf("error message = %q, want %q", resp.Error.Message, "test error")
	}
}

// Test tools/call with mock command
func TestToolsCallWithMock(t *testing.T) {
	// Create a mock script for testing
	tmpDir := t.TempDir()
	mockScript := filepath.Join(tmpDir, "mock-opencode")

	// Create a simple mock script
	mockContent := `#!/bin/sh
case "$1" in
  models)
    echo "model1"
    echo "model2"
    ;;
  session)
    if [ "$2" = "list" ]; then
      echo "session1"
      echo "session2"
    fi
    ;;
  agent)
    if [ "$2" = "list" ]; then
      echo "agent1"
      echo "agent2"
    fi
    ;;
  run)
    echo "AI response"
    ;;
  *)
    echo "Unknown command: $1"
    exit 1
    ;;
esac
`
	if err := os.WriteFile(mockScript, []byte(mockContent), 0755); err != nil {
		t.Fatalf("failed to create mock script: %v", err)
	}

	cfg := serverConfig{
		Target:         mockScript,
		DefaultTimeout: 5 * time.Second,
	}
	sessions := &sessionStore{sessions: make(map[string]*session)}
	handler := createMCPHandler(sessions, cfg)

	tests := []struct {
		name     string
		tool     string
		args     map[string]any
		wantText string
		wantErr  bool
	}{
		{
			name:     "models",
			tool:     toolModels,
			args:     map[string]any{},
			wantText: "model1",
		},
		{
			name:     "session list",
			tool:     toolSessionList,
			args:     map[string]any{},
			wantText: "session1",
		},
		{
			name:     "agent list",
			tool:     toolAgentList,
			args:     map[string]any{},
			wantText: "agent1",
		},
		{
			name: "exec",
			tool: toolExec,
			args: map[string]any{
				"args": []string{"models"},
			},
			wantText: "model1",
		},
		{
			name: "run",
			tool: toolRun,
			args: map[string]any{
				"message": "Hello",
			},
			wantText: "AI response",
		},
		{
			name:    "unknown tool",
			tool:    "unknown_tool",
			args:    map[string]any{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argsJSON, _ := json.Marshal(tt.args)
			reqBody := map[string]any{
				"jsonrpc": "2.0",
				"method":  "tools/call",
				"id":      1,
				"params": map[string]any{
					"name":      tt.tool,
					"arguments": json.RawMessage(argsJSON),
				},
			}
			body, _ := json.Marshal(reqBody)
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			var resp mcpResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}

			if tt.wantErr {
				if resp.Error == nil {
					t.Error("expected error")
				}
				return
			}

			if resp.Error != nil {
				t.Fatalf("unexpected error: %v", resp.Error)
			}

			result, ok := resp.Result.(map[string]any)
			if !ok {
				t.Fatalf("result is not a map: %T", resp.Result)
			}

			content, ok := result["content"].([]any)
			if !ok || len(content) == 0 {
				t.Fatal("no content in result")
			}

			firstContent, ok := content[0].(map[string]any)
			if !ok {
				t.Fatal("content item is not a map")
			}

			text, _ := firstContent["text"].(string)
			if !strings.Contains(text, tt.wantText) {
				t.Errorf("text = %q, want containing %q", text, tt.wantText)
			}
		})
	}
}

// Test file attachment in tools/call
func TestToolsCallWithFileAttachment(t *testing.T) {
	// Create a mock script that echoes all arguments
	tmpDir := t.TempDir()
	mockScript := filepath.Join(tmpDir, "mock-opencode")

	mockContent := `#!/bin/sh
echo "Args: $@"
`
	if err := os.WriteFile(mockScript, []byte(mockContent), 0755); err != nil {
		t.Fatalf("failed to create mock script: %v", err)
	}

	cfg := serverConfig{
		Target:         mockScript,
		DefaultTimeout: 5 * time.Second,
	}
	sessions := &sessionStore{sessions: make(map[string]*session)}
	handler := createMCPHandler(sessions, cfg)

	// Create test file
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	argsJSON, _ := json.Marshal(map[string]any{
		"message": "Analyze this file",
		"files":   []string{testFile, "another.go"},
	})
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"id":      1,
		"params": map[string]any{
			"name":      toolRun,
			"arguments": json.RawMessage(argsJSON),
		},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var resp mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}

	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatal("no content in result")
	}

	firstContent, ok := content[0].(map[string]any)
	if !ok {
		t.Fatal("content item is not a map")
	}

	text, _ := firstContent["text"].(string)
	// Check that --file arguments are in the output
	if !strings.Contains(text, "--file") {
		t.Errorf("expected --file in command args, got: %q", text)
	}
	if !strings.Contains(text, testFile) {
		t.Errorf("expected test file path in command args, got: %q", text)
	}
	if !strings.Contains(text, "another.go") {
		t.Errorf("expected 'another.go' in command args, got: %q", text)
	}
}

// Test validation errors in tools/call
func TestToolsCallValidation(t *testing.T) {
	cfg := serverConfig{
		Target:         "echo",
		DefaultTimeout: 5 * time.Second,
	}
	sessions := &sessionStore{sessions: make(map[string]*session)}
	handler := createMCPHandler(sessions, cfg)

	tests := []struct {
		name    string
		params  map[string]any
		wantErr string
	}{
		{
			name: "exec missing args",
			params: map[string]any{
				"name":      toolExec,
				"arguments": json.RawMessage(`{}`),
			},
			wantErr: "missing args",
		},
		{
			name: "run missing message",
			params: map[string]any{
				"name":      toolRun,
				"arguments": json.RawMessage(`{}`),
			},
			wantErr: "missing message",
		},
		{
			name: "invalid cwd",
			params: map[string]any{
				"name":      toolRun,
				"arguments": json.RawMessage(`{"message":"test","cwd":"/nonexistent/path"}`),
			},
			wantErr: "invalid cwd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := map[string]any{
				"jsonrpc": "2.0",
				"method":  "tools/call",
				"id":      1,
				"params":  tt.params,
			}
			body, _ := json.Marshal(reqBody)
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			var resp mcpResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}

			if resp.Error == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(resp.Error.Message, tt.wantErr) {
				t.Errorf("error message = %q, want containing %q", resp.Error.Message, tt.wantErr)
			}
		})
	}
}

// Test SSE streaming format
func TestSSEStreaming(t *testing.T) {
	// Create mock script that outputs JSON lines
	tmpDir := t.TempDir()
	mockScript := filepath.Join(tmpDir, "mock-opencode")

	mockContent := `#!/bin/sh
echo '{"type":"text","part":{"text":"Hello"}}'
echo '{"type":"text","part":{"text":"World"}}'
`
	if err := os.WriteFile(mockScript, []byte(mockContent), 0755); err != nil {
		t.Fatalf("failed to create mock script: %v", err)
	}

	cfg := serverConfig{
		Target:         mockScript,
		DefaultTimeout: 5 * time.Second,
	}
	sessions := &sessionStore{sessions: make(map[string]*session)}
	handler := createMCPHandler(sessions, cfg)

	argsJSON, _ := json.Marshal(map[string]any{"message": "test"})
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"id":      1,
		"params": map[string]any{
			"name":      toolRun,
			"arguments": json.RawMessage(argsJSON),
		},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), "text/event-stream")
	}

	// Check SSE format
	body2 := rec.Body.String()
	if !strings.Contains(body2, "data: ") {
		t.Error("response should contain SSE 'data: ' prefix")
	}
}

// Test HTTP method validation
func TestHTTPMethodValidation(t *testing.T) {
	sessions := &sessionStore{sessions: make(map[string]*session)}
	handler := createMCPHandler(sessions, serverConfig{})

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/mcp", nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status code = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

// Helper to create MCP handler for testing
func createMCPHandler(sessions *sessionStore, cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		// Check Accept header for SSE support
		acceptSSE := strings.Contains(r.Header.Get("Accept"), "text/event-stream")

		var req mcpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeMCPError(w, nil, -32700, "invalid JSON")
			return
		}
		if req.Method == "" {
			writeMCPError(w, req.ID, -32600, "missing method")
			return
		}

		// Handle session
		sessionID := r.Header.Get("Mcp-Session-Id")
		var sess *session

		switch req.Method {
		case "initialize":
			// Create new session
			sess = sessions.create()
			sessionID = sess.id
			w.Header().Set("Mcp-Session-Id", sessionID)
			handleInitialize(w, req)
			return
		case "notifications/initialized":
			// Client notification, just acknowledge
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			// Validate session for non-init requests
			if sessionID != "" {
				sess = sessions.get(sessionID)
			}
		}

		if sess != nil {
			w.Header().Set("Mcp-Session-Id", sess.id)
		}

		switch req.Method {
		case "tools/list":
			handleToolsList(w, req)
		case "tools/call":
			if acceptSSE {
				handleToolsCallSSE(w, r.Context(), cfg, req)
			} else {
				handleToolsCall(w, r.Context(), cfg, req)
			}
		default:
			writeMCPError(w, req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
		}
	}
}

// Benchmark tests
func BenchmarkSessionCreate(b *testing.B) {
	store := &sessionStore{sessions: make(map[string]*session)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.create()
	}
}

func BenchmarkSessionGet(b *testing.B) {
	store := &sessionStore{sessions: make(map[string]*session)}
	sess := store.create()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.get(sess.id)
	}
}

func BenchmarkValidateCwd(b *testing.B) {
	tmpDir := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		validateCwd(tmpDir)
	}
}

func BenchmarkExtractEventData(b *testing.B) {
	event := map[string]any{
		"type": "text",
		"part": map[string]any{
			"text": "Hello, world!",
		},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		extractEventData(event)
	}
}

// Test streamLines function
func TestStreamLines(t *testing.T) {
	input := "line1\nline2\nline3\n"
	reader := strings.NewReader(input)
	var buf bytes.Buffer

	// Mock flusher
	flusher := &mockFlusher{w: &buf}

	err := streamLines(reader, flusher, flusher)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "data: line1") {
		t.Errorf("output missing 'data: line1': %q", output)
	}
}

type mockFlusher struct {
	w io.Writer
}

func (m *mockFlusher) Write(p []byte) (n int, err error) {
	return m.w.Write(p)
}

func (m *mockFlusher) Flush() {}

var _ http.Flusher = (*mockFlusher)(nil)
