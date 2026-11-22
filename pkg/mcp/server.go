package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/session"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

type Server struct {
	mem       *memory.Service
	kg        *kg.KG
	sessions  *session.Manager
	optimizer OptimizerFacade
	cfg       *config.Config
	logger    *slog.Logger
	dlog      *debuglog.Logger
	version   string

	// searchCalls counts anchored_ctx_search invocations within the current
	// indexing scope. Reset by anchored_batch_execute, anchored_index, and
	// anchored_fetch_and_index. Drives progressive throttling:
	//   1-3: normal results
	//   4-8: limit=1 with a warning appended
	//   9+:  blocked, redirected to anchored_batch_execute
	searchCalls atomic.Int32

	remoteResolutions remoteResolutionCache
}

// resetSearchThrottle is called whenever a tool repopulates / extends the
// indexed corpus, so a fresh round of follow-up searches starts at zero.
func (s *Server) resetSearchThrottle() { s.searchCalls.Store(0) }

// nextSearchCall returns the 1-based count for the current call and the
// throttling decision derived from it.
func (s *Server) nextSearchCall() int32 { return s.searchCalls.Add(1) }

func NewServer(mem *memory.Service, kg *kg.KG, sessions *session.Manager, optimizer OptimizerFacade, cfg *config.Config, version string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{mem: mem, kg: kg, sessions: sessions, optimizer: optimizer, cfg: cfg, logger: logger, version: version}
	if cfg != nil {
		remotesync.UseSecretPatterns(cfg.Sanitizer.Patterns)
	}
	if mem != nil {
		mem.SetRemoteOutboxDeliverer(server.saveRemoteIdempotent)
	}
	return server
}

// SetDebugLogger attaches an optional NDJSON debug logger. When set, every
// inbound MCP message and every tool dispatch is recorded so users can audit
// "did the model actually call anchored?" after the fact. Safe with nil.
func (s *Server) SetDebugLogger(d *debuglog.Logger) {
	s.dlog = d
}

func (s *Server) HandleMessage(ctx context.Context, data []byte) []byte {
	req, err := ParseRequest(data)
	if err != nil {
		s.dlog.Event("mcp.parse_error", map[string]any{"error": err.Error(), "raw": debuglog.Content(string(data), 200)})
		return MarshalResponse(NewErrorResponse(nil, NewError(-32700, err.Error())))
	}

	s.dlog.Event("mcp.message", map[string]any{"method": req.Method, "bytes": len(data)})

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID, req.Params)
	case "notifications/initialized":
		return nil
	case "tools/list":
		return s.handleToolsList(ctx, req.ID)
	case "tools/call":
		return s.handleToolsCall(ctx, req.ID, req.Params)
	case "resources/list":
		return s.handleResourcesList(req.ID)
	case "resources/read":
		return s.handleResourcesRead(ctx, req.ID, req.Params)
	case "ping":
		return MarshalResponse(NewResponse(req.ID, map[string]string{}))
	default:
		return MarshalResponse(NewErrorResponse(req.ID, NewError(-32601, fmt.Sprintf("unknown method: %s", req.Method))))
	}
}

func (s *Server) handleInitialize(id json.RawMessage, params json.RawMessage) []byte {
	result := InitializeResult{
		ProtocolVersion: MCPVersion,
		ServerInfo: ServerInfo{
			Name:    "anchored",
			Version: s.version,
		},
		Instructions: AnchoredMCPInstructions,
	}
	result.Capabilities.Tools.ListChanged = false
	result.Capabilities.Resources.Subscribe = false
	result.Capabilities.Resources.ListChanged = false

	return MarshalResponse(NewResponse(id, result))
}

func (s *Server) handleToolsList(ctx context.Context, id json.RawMessage) []byte {
	tools := s.toolsForCWD(ctx, ".")
	return MarshalResponse(NewResponse(id, map[string]any{"tools": tools}))
}

// toolsForCWD conditionally exposes anchored_skill only after this repository
// is proven to resolve to a reachable remote project. A configured remote or
// stale linked-project ID alone must not advertise an unusable tool.
func (s *Server) toolsForCWD(ctx context.Context, cwd string) []Tool {
	tools := ToolDefinitions()
	if !s.remoteSkillsAdvertisable(ctx, cwd) {
		filtered := tools[:0]
		for _, tool := range tools {
			if tool.Name != "anchored_skill" {
				filtered = append(filtered, tool)
			}
		}
		tools = filtered
	}
	SortTools(tools)
	return tools
}

// remoteSkillsAdvertisable answers the tools/list question without paying for
// it on the common path: a client with no remote configured cannot have a
// remote project, so it never reaches the network. Only a user who opted into
// a remote pays the bounded probe.
//
// The timeout is deliberately not tightened below remoteSkillPriorityTimeout.
// A probe that expires hides anchored_skill for the rest of the session, which
// is a worse outcome than a bounded wait on a path clients call once; the
// answer is cached, so a silent remote costs that wait once per remoteMissTTL.
func (s *Server) remoteSkillsAdvertisable(ctx context.Context, cwd string) bool {
	if !s.hasAnyRemote() {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, remoteSkillPriorityTimeout())
	defer cancel()
	return s.hasReachableRemoteRepositoryProject(probeCtx, cwd)
}

func (s *Server) handleToolsCall(ctx context.Context, id json.RawMessage, params json.RawMessage) []byte {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		s.dlog.Event("mcp.tool_call", map[string]any{"stage": "params_invalid", "error": err.Error()})
		return MarshalResponse(NewErrorResponse(id, InvalidParams("invalid params")))
	}

	start := time.Now()
	result, err := s.callTool(ctx, p.Name, p.Arguments)
	latencyMs := time.Since(start).Milliseconds()

	if err != nil {
		s.dlog.Event("mcp.tool_call", map[string]any{
			"stage":      "error",
			"tool":       p.Name,
			"latency_ms": latencyMs,
			"args":       debuglog.Content(string(p.Arguments), 240),
			"error":      err.Error(),
		})
		return MarshalResponse(NewErrorResponse(id, InternalError(err)))
	}

	s.dlog.Event("mcp.tool_call", map[string]any{
		"stage":          "ok",
		"tool":           p.Name,
		"latency_ms":     latencyMs,
		"args":           debuglog.Content(string(p.Arguments), 240),
		"result_bytes":   len(result),
		"result_preview": debuglog.Content(result, 200),
	})

	return MarshalResponse(NewResponse(id, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": result},
		},
	}))
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "anchored_context":
		return s.toolContext(ctx, args)
	case "anchored_search":
		return s.toolSearch(ctx, args)
	case "anchored_skill":
		return s.toolSkill(ctx, args)
	case "anchored_save":
		return s.toolSave(ctx, args)
	case "anchored_list":
		return s.toolList(ctx, args)
	case "anchored_forget":
		return s.toolForget(ctx, args)
	case "anchored_update":
		return s.toolUpdate(ctx, args)
	case "anchored_stats":
		return s.toolStats(ctx)
	case "anchored_kg_query", "kg_query":
		return s.toolKGQuery(ctx, args)
	case "anchored_kg_add", "kg_add":
		return s.toolKGAdd(ctx, args)
	case "anchored_session_end":
		return s.toolSessionEnd(ctx, args)
	case "anchored_task":
		return s.toolTask(ctx, args)
	case "anchored_execute":
		return s.toolCtxExecute(ctx, args)
	case "anchored_execute_file":
		return s.toolCtxExecuteFile(ctx, args)
	case "anchored_batch_execute":
		return s.toolCtxBatchExecute(ctx, args)
	case "anchored_index":
		return s.toolCtxIndex(ctx, args)
	case "anchored_ctx_search":
		return s.toolCtxSearch(ctx, args)
	case "anchored_fetch_and_index":
		return s.toolCtxFetchAndIndex(ctx, args)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func joinLines(lines []string) string {
	result := ""
	for i, line := range lines {
		if i > 0 {
			result += "\n"
		}
		result += line
	}
	return result
}
