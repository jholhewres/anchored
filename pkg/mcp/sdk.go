package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolAliases are old tool names tools/call still answers to. They never
// appear in tools/list.
var toolAliases = map[string]string{
	"kg_query": "anchored_kg_query",
	"kg_add":   "anchored_kg_add",
}

// mcpSession is one MCP session's state: the root its relative and omitted
// cwd arguments resolve against, and its anchored_ctx_search throttle.
type mcpSession struct {
	// root is an absolute directory, or "" for a call made outside any
	// session (direct callers in tests), which keeps the old "." default.
	root string

	// searchCalls counts anchored_ctx_search invocations within the current
	// indexing scope. Reset by anchored_batch_execute, anchored_index, and
	// anchored_fetch_and_index. Drives progressive throttling:
	//   1-3: normal results
	//   4-8: limit=1 with a warning appended
	//   9+:  blocked, redirected to anchored_batch_execute
	searchCalls atomic.Int32
}

type mcpSessionKey struct{}

func withMCPSession(ctx context.Context, sess *mcpSession) context.Context {
	return context.WithValue(ctx, mcpSessionKey{}, sess)
}

// session is the MCP session a call runs in, or the server's own state for a
// call made outside any session.
func (s *Server) session(ctx context.Context) *mcpSession {
	if sess, ok := ctx.Value(mcpSessionKey{}).(*mcpSession); ok && sess != nil {
		return sess
	}
	s.noSessionOnce.Do(func() { s.noSession = &mcpSession{} })
	return s.noSession
}

// sessionCWD is the directory a tool argument names: an omitted cwd is the
// session's root and a relative one is resolved against it, never against
// the process's working directory. Outside a session the old "." default
// stands.
func (s *Server) sessionCWD(ctx context.Context, cwd string) string {
	root := s.session(ctx).root
	switch {
	case root == "":
		return defaultCWD(cwd)
	case cwd == "":
		return root
	case filepath.IsAbs(cwd):
		return cwd
	default:
		return filepath.Join(root, cwd)
	}
}

// sessionPath resolves a relative cwd or path against the session's root and
// keeps an omitted one omitted, for arguments where "" has its own meaning
// (a global search, a call with no project).
func (s *Server) sessionPath(ctx context.Context, path string) string {
	if path == "" {
		return ""
	}
	return s.sessionCWD(ctx, path)
}

// sessionFile resolves a relative file path against the cwd the call names,
// itself resolved against the session's root. Outside a session the path is
// left to the process's working directory, as before.
func (s *Server) sessionFile(ctx context.Context, cwd, path string) string {
	if path == "" || filepath.IsAbs(path) || s.session(ctx).root == "" {
		return path
	}
	return filepath.Join(s.sessionCWD(ctx, cwd), path)
}

// newSDKServer builds the go-sdk server for one session: every tool and
// resource, with the session's state carried into each call.
func (s *Server) newSDKServer(sess *mcpSession) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "anchored", Version: s.version}, &sdk.ServerOptions{
		Instructions: AnchoredMCPInstructions,
		Logger:       sdkLogger(s.logger),
		// Explicit, so the SDK adds neither logging nor listChanged: the tool
		// and resource lists never change during a session.
		Capabilities: &sdk.ServerCapabilities{
			Tools:     &sdk.ToolCapabilities{},
			Resources: &sdk.ResourceCapabilities{},
		},
	})
	for _, tool := range ToolDefinitions() {
		name := tool.Name
		server.AddTool(&sdk.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return s.toolResult(ctx, name, req.Params.Arguments), nil
			})
	}
	for _, resource := range ResourceDefinitions() {
		server.AddResource(&sdk.Resource{
			URI:         resource.URI,
			Name:        resource.Name,
			Description: resource.Description,
			MIMEType:    resource.MIMEType,
		}, s.readResource)
	}
	server.AddReceivingMiddleware(s.sessionMiddleware(sess))
	return server
}

// sessionMiddleware carries the session into every request, maps the hidden
// aliases onto their tools and hides anchored_skill from tools/list until
// the session's repository resolves to a reachable remote project.
func (s *Server) sessionMiddleware(sess *mcpSession) sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			ctx = withMCPSession(ctx, sess)
			s.dlog.Event("mcp.message", map[string]any{"method": method})
			switch method {
			case "tools/call":
				if call, ok := req.(*sdk.CallToolRequest); ok && call.Params != nil {
					if canonical, ok := toolAliases[call.Params.Name]; ok {
						call.Params.Name = canonical
					}
				}
			case "tools/list":
				res, err := next(ctx, method, req)
				if list, ok := res.(*sdk.ListToolsResult); ok && err == nil && !s.remoteSkillsAdvertisable(ctx, s.sessionCWD(ctx, "")) {
					visible := make([]*sdk.Tool, 0, len(list.Tools))
					for _, tool := range list.Tools {
						if tool.Name != "anchored_skill" {
							visible = append(visible, tool)
						}
					}
					filtered := *list
					filtered.Tools = visible
					return &filtered, nil
				}
				return res, err
			}
			return next(ctx, method, req)
		}
	}
}

// toolResult runs one tool. A tool's own failure is a result with isError,
// so the model reads it; JSON-RPC errors are left to the protocol.
func (s *Server) toolResult(ctx context.Context, name string, args json.RawMessage) *sdk.CallToolResult {
	start := time.Now()
	text, err := s.callTool(ctx, name, args)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		s.dlog.Event("mcp.tool_call", map[string]any{
			"stage":      "error",
			"tool":       name,
			"latency_ms": latencyMs,
			"args":       debuglog.Content(string(args), 240),
			"error":      err.Error(),
		})
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}}}
	}
	s.dlog.Event("mcp.tool_call", map[string]any{
		"stage":          "ok",
		"tool":           name,
		"latency_ms":     latencyMs,
		"args":           debuglog.Content(string(args), 240),
		"result_bytes":   len(text),
		"result_preview": debuglog.Content(text, 200),
	})
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}

func (s *Server) readResource(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	uri := req.Params.URI
	text, err := s.resourceText(ctx, uri)
	if err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()}
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "text/plain", Text: text}}}, nil
}

// sdkLogger keeps the SDK's routine lifecycle lines out of stderr, which MCP
// clients surface as errors; the SDK logs at its own levels, so only what
// the server's logger would show at error level passes.
func sdkLogger(logger *slog.Logger) *slog.Logger {
	return slog.New(minLevelHandler{Handler: logger.Handler(), min: slog.LevelError})
}

type minLevelHandler struct {
	slog.Handler
	min slog.Level
}

func (h minLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.min && h.Handler.Enabled(ctx, level)
}

func (h minLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevelHandler{Handler: h.Handler.WithAttrs(attrs), min: h.min}
}

func (h minLevelHandler) WithGroup(name string) slog.Handler {
	return minLevelHandler{Handler: h.Handler.WithGroup(name), min: h.min}
}
