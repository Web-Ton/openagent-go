package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	openagent "github.com/yusheng-g/openagent-go"
)

// Server exposes openagent.Tool instances as an MCP server.
//
// Create with [NewServer], add tools with [Server.AddTool], then serve:
//
//	server := mcp.NewServer("my-agent", "1.0.0")
//	server.AddTool(myTool)
//	server.Run(ctx, &mcpsdk.StdioTransport{})          // stdio
//	http.ListenAndServe(":9090", server.HTTPHandler())  // HTTP/SSE
type Server struct {
	inner *mcpsdk.Server
	opts  ServerOptions
	// toolCallObserver receives a ToolCallEvent at the completion of each
	// tool.Execute (every tools/call request).  nil = no tracking.  Wired at
	// assembly time via SetToolCallObserver — the mcp package does NOT import
	// track, mirroring the acp package's "server does not import track, only
	// the assembly layer wires concrete observers" constraint.
	toolCallObserver openagent.ToolCallObserver
}

// ServerOptions configures a [Server].
type ServerOptions struct {
	// Logger is an optional slog.Logger for MCP protocol logging.
	Logger *slog.Logger
}

// NewServer creates an MCP [Server] with the given implementation identity.
// name and version are reported to MCP clients during initialization.
func NewServer(name, version string, opts *ServerOptions) *Server {
	var o ServerOptions
	if opts != nil {
		o = *opts
	}
	s := &Server{opts: o}
	s.inner = mcpsdk.NewServer(&mcpsdk.Implementation{
		Name: name, Version: version,
	}, &mcpsdk.ServerOptions{
		Logger: o.Logger,
	})
	return s
}

// SetToolCallObserver wires a ToolCallObserver that receives a
// ToolCallEvent at the completion of every tool.Execute (each tools/call
// request).  Call once at assembly time (e.g. iac-server main.go) before
// serving.  nil = no tracking (events silently dropped).  Safe to call
// before the server starts serving.
func (s *Server) SetToolCallObserver(obs openagent.ToolCallObserver) {
	s.toolCallObserver = obs
}

// AddTool registers an openagent.Tool as an MCP tool on this server.
// The tool's FunctionDefinition and Execute are adapted to MCP's
// ToolHandler interface.
func (s *Server) AddTool(tool openagent.Tool) error {
	def := tool.Definition()
	if def.Name == "" {
		return fmt.Errorf("mcp: tool name is required")
	}

	mcpTool := ToMCPTool(def)
	s.inner.AddTool(mcpTool, s.buildToolHandler(tool))
	return nil
}

// withProgress builds a [openagent.ProgressFunc] from the MCP server session
// and the client-supplied progress token, then injects it into ctx.
// If no token is present, ctx is returned unchanged (ProgressFunc will be nil).
func withProgress(ctx context.Context, req *mcpsdk.CallToolRequest) context.Context {
	token := req.Params.GetProgressToken()
	if token == nil {
		return ctx
	}
	ss := req.Session
	return openagent.WithProgress(ctx, func(message string, progress, total float64) {
		_ = ss.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{
			ProgressToken: token,
			Message:       message,
			Progress:      progress,
			Total:         total,
		})
	})
}

// AddTools registers multiple openagent.Tool instances.
func (s *Server) AddTools(tools []openagent.Tool) error {
	for _, t := range tools {
		if err := s.AddTool(t); err != nil {
			return err
		}
	}
	return nil
}

// AddToolWithSchema registers an openagent.Tool with an explicit JSON Schema
// override. Use this when the tool's Definition().Parameters is not a valid
// JSON Schema or you want to provide a different schema.
func (s *Server) AddToolWithSchema(tool openagent.Tool, inputSchema json.RawMessage) error {
	def := tool.Definition()
	mcpTool := &mcpsdk.Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: inputSchema,
	}
	s.inner.AddTool(mcpTool, s.buildToolHandler(tool))
	return nil
}

// buildToolHandler adapts an openagent.Tool to the MCP ToolHandler
// signature.  It is the single shared handler used by both AddTool and
// AddToolWithSchema, so tool-call tracking is wired once here instead of
// being duplicated across the two registration paths.
//
// The handler injects progress + sessionID, executes the tool, emits a
// ToolCallEvent (for tracking/usage counting) when a ToolCallObserver is
// wired, and wraps the result in MCP TextContent.  If the client supplied
// a progressToken, a [ProgressFunc] is built from the server session and
// injected into the context so the tool can stream progress notifications
// back to the client during long-running calls.  The session ID is also
// injected so tools can distinguish same-client retries from cross-client
// conflicts on shared resources.
func (s *Server) buildToolHandler(tool openagent.Tool) func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	def := tool.Definition()
	return func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		ctx = withProgress(ctx, req)
		ctx = withSessionID(ctx, req)
		start := time.Now()
		output := tool.Execute(ctx, req.Params.Arguments)

		// Emit a tool-call event for observers (tracking, etc.).  nil =
		// silently dropped.  The event carries the wall-clock duration of
		// tool.Execute; for async tools (iac-server jobs) this is the job
		// *submission* time, not the full LLM run — by design (usage-count
		// granularity, not end-to-end).
		if s.toolCallObserver != nil {
			s.toolCallObserver.OnToolCall(ctx, openagent.ToolCallEvent{
				ToolName:   def.Name,
				EntryPoint: openagent.EntryPointIAC,
				DurationMs: time.Since(start).Milliseconds(),
				Err:        output.AsError(),
			})
		}

		if output.Error != nil {
			return &mcpsdk.CallToolResult{
				IsError: true,
				Content: []mcpsdk.Content{
					&mcpsdk.TextContent{Text: output.Error.Message},
				},
			}, nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{
				&mcpsdk.TextContent{Text: output.Content},
			},
		}, nil
	}
}

// withSessionID injects the MCP server session ID into ctx so tools can
// distinguish same-client retries from cross-client conflicts. The session
// ID is empty for stdio (single client) and unique per HTTP connection.
func withSessionID(ctx context.Context, req *mcpsdk.CallToolRequest) context.Context {
	if req.Session != nil {
		return WithSessionID(ctx, req.Session.ID())
	}
	return ctx
}

// sessionIDKey is the context key for the MCP session ID (Mcp-Session-Id
// header). Empty for stdio (single client), unique per HTTP connection.
// withSessionID injects it; SessionIDFromContext extracts it.
type sessionIDKey struct{}

// WithSessionID returns a context that carries the MCP session ID.
// An empty sessionID is valid (stdio transport) and means "single client".
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext extracts the MCP session ID from ctx, or "" if none
// was set (e.g. stdio transport, or a call outside an MCP server).
func SessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}

// Run starts the MCP server on the given transport. It blocks until the
// transport closes or ctx is cancelled.
//
// Common transports:
//   - &mcpsdk.StdioTransport{} — stdin/stdout (for subprocess spawning)
//   - in-memory transports (see mcpsdk.NewInMemoryTransports for testing)
func (s *Server) Run(ctx context.Context, transport mcpsdk.Transport) error {
	return s.inner.Run(ctx, transport)
}

// HTTPHandler returns an http.Handler that serves MCP over SSE (Server-Sent
// Events). Use this to expose the server over HTTP:
//
//	http.ListenAndServe(":9090", server.HTTPHandler())
func (s *Server) HTTPHandler() http.Handler {
	return mcpsdk.NewStreamableHTTPHandler(
		func(r *http.Request) *mcpsdk.Server { return s.inner },
		&mcpsdk.StreamableHTTPOptions{},
	)
}

// Inner returns the underlying mcpsdk.Server for advanced use cases.
func (s *Server) Inner() *mcpsdk.Server {
	return s.inner
}
