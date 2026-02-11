# opencode-mcp

A standard MCP (Model Context Protocol) server that wraps `opencode-cli`, providing AI code editing capabilities via HTTP.

## Features

- Full MCP 2024-11-05 protocol support
- Session management via `Mcp-Session-Id` header
- SSE streaming for long-running operations
- Multiple specialized tools for AI code editing
- File attachment support for context-aware AI assistance
- Docker support for containerized deployment

## Quick Start

### Local Development

```bash
go run ./cmd/mcpserver
```

Or build and run:

```bash
go build -o opencode-mcp ./cmd/mcpserver
./opencode-mcp
```

### Docker

```bash
# Build and run with docker-compose
docker-compose up -d

# Or build manually
docker build -t opencode-mcp .
docker run -p 9876:9876 -v /path/to/workspace:/workspace opencode-mcp
```

### Run Tests

```bash
go test ./cmd/mcpserver/...
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_ADDR` | `:9876` | Server listen address |
| `MCP_TARGET` | `opencode-cli` | Path to opencode-cli executable |
| `MCP_TIMEOUT_SEC` | `120` | Command timeout in seconds |

### Docker-specific Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_PORT` | `9876` | Host port to expose |
| `WORKSPACE_PATH` | `./workspace` | Host path to mount as workspace |
| `OPENCODE_CLI_PATH` | `/usr/local/bin/opencode-cli` | Path to opencode-cli on host |

## MCP Tools

| Tool | Description |
|------|-------------|
| `opencode_run` | Run AI assistant with a message (main tool for code editing) |
| `opencode_exec` | Run any opencode-cli command with custom arguments |
| `opencode_models` | List available AI models |
| `opencode_session_list` | List saved sessions |
| `opencode_agent_list` | List available agents |

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/mcp` | POST | MCP JSON-RPC endpoint |
| `/mcp` | OPTIONS | Endpoint discovery |
| `/exec` | POST | Direct command execution |
| `/exec/stream` | POST | Streaming command execution |
| `/health` | GET | Health check |

## Usage Examples

### Initialize MCP Session

```bash
curl -s http://localhost:9876/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}'
```

Response includes `Mcp-Session-Id` header for subsequent requests.

### List Available Tools

```bash
curl -s http://localhost:9876/mcp \
  -H 'Content-Type: application/json' \
  -H 'Mcp-Session-Id: <session-id>' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```

### Run AI Assistant with File Attachments

```bash
curl -s http://localhost:9876/mcp \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc":"2.0",
    "id":2,
    "method":"tools/call",
    "params":{
      "name":"opencode_run",
      "arguments":{
        "message":"Review and improve this code",
        "cwd":"/path/to/project",
        "files":["src/main.go", "src/utils.go"],
        "model":"github-copilot/claude-sonnet-4"
      }
    }
  }'
```

### Run AI Assistant

```bash
curl -s http://localhost:9876/mcp \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc":"2.0",
    "id":2,
    "method":"tools/call",
    "params":{
      "name":"opencode_run",
      "arguments":{
        "message":"Analyze this codebase and suggest improvements",
        "cwd":"/path/to/project",
        "model":"github-copilot/claude-sonnet-4"
      }
    }
  }'
```

### List Models

```bash
curl -s http://localhost:9876/mcp \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc":"2.0",
    "id":3,
    "method":"tools/call",
    "params":{"name":"opencode_models","arguments":{}}
  }'
```

### Streaming (SSE)

Add `Accept: text/event-stream` header for streaming responses:

```bash
curl -N http://localhost:9876/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{
    "jsonrpc":"2.0",
    "id":4,
    "method":"tools/call",
    "params":{
      "name":"opencode_run",
      "arguments":{"message":"Hello","cwd":"/tmp"}
    }
  }'
```

### Direct Exec (Non-MCP)

```bash
curl -s http://localhost:9876/exec \
  -H 'Content-Type: application/json' \
  -d '{"args":["models"]}'
```

## MCP Client Configuration

To use with MCP clients, configure the server URL:

```json
{
  "mcpServers": {
    "opencode": {
      "url": "http://localhost:9876/mcp"
    }
  }
}
```

## License

MIT
