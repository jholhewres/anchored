package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

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

	// noSession holds the state of calls made outside any MCP session.
	noSession     *mcpSession
	noSessionOnce sync.Once

	remoteResolutions remoteResolutionCache
}

// resetSearchThrottle is called whenever a tool repopulates / extends the
// indexed corpus, so the session's next round of follow-up searches starts
// at zero.
func (s *Server) resetSearchThrottle(ctx context.Context) { s.session(ctx).searchCalls.Store(0) }

// nextSearchCall returns the 1-based count of the session's current call,
// from which the throttling decision is derived.
func (s *Server) nextSearchCall(ctx context.Context) int32 { return s.session(ctx).searchCalls.Add(1) }

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
	case "anchored_kg_query":
		return s.toolKGQuery(ctx, args)
	case "anchored_kg_add":
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
