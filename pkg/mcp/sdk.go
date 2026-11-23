package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/semaphore"
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

	slotsOnce sync.Once
	slots     *semaphore.Weighted // see maxConcurrentExecutions
	waiting   atomic.Int32        // calls waiting for a slot; see maxWaitingExecutions
}

type mcpSessionKey struct{}

func withMCPSession(ctx context.Context, sess *mcpSession) context.Context {
	return context.WithValue(ctx, mcpSessionKey{}, sess)
}

// mcpState is the state of the MCP session a call runs in, or the server's
// own for a call made outside any session. (Server.sessions, by contrast, is
// Anchored's record of the tools' session_id.)
func (s *Server) mcpState(ctx context.Context) *mcpSession {
	if sess, ok := ctx.Value(mcpSessionKey{}).(*mcpSession); ok && sess != nil {
		return sess
	}
	s.noSessionOnce.Do(func() { s.noSession = &mcpSession{} })
	return s.noSession
}

// rootedCWD is the directory a tool argument names: an omitted cwd is the
// session's root and a relative one is resolved against it, never against
// the process's working directory. Outside a session the old "." default
// stands.
func (s *Server) rootedCWD(ctx context.Context, cwd string) string {
	root := s.mcpState(ctx).root
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

// rootedPath resolves a relative cwd or path against the session's root and
// keeps an omitted one omitted, for arguments where "" has its own meaning
// (a global search, a call with no project).
func (s *Server) rootedPath(ctx context.Context, path string) string {
	if path == "" {
		return ""
	}
	return s.rootedCWD(ctx, path)
}

// optionalProjectID is the project of an optional cwd argument, resolved
// against the session's root, or "" when the call names none.
func (s *Server) optionalProjectID(ctx context.Context, cwd string) string {
	if cwd = s.rootedPath(ctx, cwd); cwd == "" {
		return ""
	}
	return s.mem.ResolveProject(cwd)
}

// rootedFile resolves a relative file path against the cwd the call names,
// itself resolved against the session's root. Outside a session the path is
// left to the process's working directory, as before.
func (s *Server) rootedFile(ctx context.Context, cwd, path string) string {
	if path == "" || filepath.IsAbs(path) || s.mcpState(ctx).root == "" {
		return path
	}
	return filepath.Join(s.rootedCWD(ctx, cwd), path)
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
	texts := s.resourceTexts()
	for _, resource := range ResourceDefinitions() {
		text, ok := texts[resource.URI]
		if !ok {
			continue
		}
		server.AddResource(&sdk.Resource{
			URI:         resource.URI,
			Name:        resource.Name,
			Description: resource.Description,
			MIMEType:    resource.MIMEType,
		}, readResource(text))
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
				if list, ok := res.(*sdk.ListToolsResult); ok && err == nil {
					filtered := *list
					filtered.Tools = s.visibleTools(ctx, s.rootedCWD(ctx, ""), list.Tools)
					return &filtered, nil
				}
				return res, err
			}
			return next(ctx, method, req)
		}
	}
}

// visibleTools is tools as cwd's session sees them: anchored_skill only once
// the repository is proven to resolve to a reachable remote project. A
// configured remote or a stale linked-project id alone must not advertise an
// unusable tool.
func (s *Server) visibleTools(ctx context.Context, cwd string, tools []*sdk.Tool) []*sdk.Tool {
	if s.remoteSkillsAdvertisable(ctx, cwd) {
		return tools
	}
	visible := make([]*sdk.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name != "anchored_skill" {
			visible = append(visible, tool)
		}
	}
	return visible
}

// toolResult runs one tool. A tool's own failure is a result with isError,
// so the model reads it; JSON-RPC errors are left to the protocol.
func (s *Server) toolResult(ctx context.Context, name string, args json.RawMessage) *sdk.CallToolResult {
	if boundedTools[name] {
		release, err := s.mcpState(ctx).executionSlots(ctx, executionWeight(args))
		if err != nil {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}}}
		}
		defer release()
	}
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

// readResource serves one resource from the function that renders its text.
func readResource(text func(context.Context) (string, error)) sdk.ResourceHandler {
	return func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		content, err := text(ctx)
		if err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()}
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: content}}}, nil
	}
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

// maxConcurrentExecutions bounds the sandbox processes and fetches one
// session runs at once. Requests run concurrently, so without a bound a
// prompt-injected model could fire any number of them. A batch call holds
// one slot per command it runs at once.
var maxConcurrentExecutions = 4

// maxWaitingExecutions bounds the calls of one session waiting for an
// execution slot. Past it a call fails at once instead of parking, so a flood
// of calls cannot grow the server without bound.
var maxWaitingExecutions = 8

// boundedTools are the tools maxConcurrentExecutions applies to.
var boundedTools = map[string]bool{
	"anchored_execute":         true,
	"anchored_execute_file":    true,
	"anchored_batch_execute":   true,
	"anchored_fetch_and_index": true,
}

// executionSlots waits for n of the session's execution slots, all at once,
// and returns their release, or the context's error when the call is
// cancelled first.
func (m *mcpSession) executionSlots(ctx context.Context, n int) (release func(), err error) {
	m.slotsOnce.Do(func() { m.slots = semaphore.NewWeighted(int64(executionLimit())) })
	weight := int64(min(max(n, 1), executionLimit()))
	if !m.slots.TryAcquire(weight) {
		if m.waiting.Add(1) > int32(maxWaitingExecutions) {
			m.waiting.Add(-1)
			return nil, errors.New("too many executions waiting in this session; retry when one finishes")
		}
		err := m.slots.Acquire(ctx, weight)
		m.waiting.Add(-1)
		if err != nil {
			return nil, err
		}
	}
	return func() { m.slots.Release(weight) }, nil
}

// executionLimit is maxConcurrentExecutions, at least 1.
func executionLimit() int { return max(maxConcurrentExecutions, 1) }

// executionWeight is how many slots a call holds: the commands or fetches
// a batch runs at once, 1 for anything else.
func executionWeight(args json.RawMessage) int {
	var p struct {
		Concurrency int `json:"concurrency"`
	}
	if json.Unmarshal(args, &p) != nil {
		return 1
	}
	return min(max(p.Concurrency, 1), executionLimit())
}
