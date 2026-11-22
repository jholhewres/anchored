//go:build !windows

package mcp

// Golden JSON-RPC conformance suite.
//
// Each case opens a fresh server and drives it with newline-delimited
// JSON-RPC through ServeStdio, the same path `anchored serve --stdio` uses,
// so the suite holds across a change of the MCP implementation underneath.
// Every case except the initialize ones starts with the handshake a real
// client sends (initialize, notifications/initialized), which is not
// recorded. The golden is the transcript: each request and the response it
// got, canonicalized (sorted keys, indented) and normalized (temp paths, IDs,
// timestamps and durations replaced by placeholders).
//
// The goldens are the MCP contract. A change that alters one must show it as
// a reviewed diff of testdata/conformance. Regenerate with:
//
//	go test -tags sqlite_fts5 ./pkg/mcp/ -run Conformance -update-conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/session"
)

var updateConformance = flag.Bool("update-conformance", false, "rewrite the golden JSON-RPC transcripts in testdata/conformance")

// conformanceStep is one line sent to the server. Exactly one of Method or
// Raw is set. Params may reference values captured earlier in the case as
// {{name}}; Capture names the value a response yields for later steps.
type conformanceStep struct {
	Method  string
	Params  string
	Notify  bool   // a notification: no id, no response expected
	Raw     string // a line sent verbatim, for malformed input
	Capture string // capture the first memory ID in this response's text
}

type conformanceCase struct {
	Name      string
	Handshake bool // send initialize + notifications/initialized first, unrecorded
	Steps     []conformanceStep
}

const conformanceClientInit = `{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"conformance","version":"1"}}`

func conformanceCases() []conformanceCase {
	initWith := func(version string) string {
		return fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"conformance","version":"1"}}`, version)
	}
	return []conformanceCase{
		{Name: "initialize", Steps: []conformanceStep{
			{Method: "initialize", Params: conformanceClientInit},
			{Method: "notifications/initialized", Notify: true},
			{Method: "ping", Params: `{}`},
		}},
		{Name: "initialize_2025_06_18", Steps: []conformanceStep{{Method: "initialize", Params: initWith("2025-06-18")}}},
		{Name: "initialize_2025_11_25", Steps: []conformanceStep{{Method: "initialize", Params: initWith("2025-11-25")}}},
		{Name: "initialize_unknown_version", Steps: []conformanceStep{{Method: "initialize", Params: initWith("1999-01-01")}}},
		{Name: "tools_list", Handshake: true, Steps: []conformanceStep{
			{Method: "tools/list", Params: `{}`},
		}},
		{Name: "resources", Handshake: true, Steps: []conformanceStep{
			{Method: "resources/list", Params: `{}`},
			{Method: "resources/read", Params: `{"uri":"anchored://memory/stats"}`},
			{Method: "resources/read", Params: `{"uri":"anchored://memory/recent"}`},
			{Method: "resources/read", Params: `{"uri":"anchored://identity"}`},
			{Method: "resources/read", Params: `{"uri":"anchored://projects"}`},
			{Method: "resources/read", Params: `{"uri":"anchored://nope"}`},
		}},
		{Name: "memory_tools", Handshake: true, Steps: []conformanceStep{
			{Method: "tools/call", Params: `{"name":"anchored_save","arguments":{"content":"The conformance fixture runs Go on ARM build hosts.","category":"fact","cwd":"{{repo}}"}}`, Capture: "fact"},
			{Method: "tools/call", Params: `{"name":"anchored_save","arguments":{"content":"Settled on SQLite with FTS5 for the conformance fixture store.","category":"decision","cwd":"{{repo}}"}}`, Capture: "decision"},
			{Method: "tools/call", Params: `{"name":"anchored_save","arguments":{"content":"The conformance fixture runs Go on ARM build hosts.","category":"fact","cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_list","arguments":{"cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_search","arguments":{"query":"SQLite FTS5 store","cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_update","arguments":{"id":"{{fact}}","content":"The conformance fixture runs Go 1.22 on ARM build hosts."}}`},
			{Method: "tools/call", Params: `{"name":"anchored_forget","arguments":{"id":"{{decision}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_stats","arguments":{}}`},
			{Method: "tools/call", Params: `{"name":"anchored_context","arguments":{"cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_session_end","arguments":{"session_id":"conformance-session","summary":"Conformance run."}}`},
		}},
		{Name: "kg_tools", Handshake: true, Steps: []conformanceStep{
			{Method: "tools/call", Params: `{"name":"anchored_kg_add","arguments":{"subject":"conformance-api","predicate":"depends_on","object":"postgres","cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_kg_query","arguments":{"entity":"conformance-api","cwd":"{{repo}}"}}`},
			// The aliases use their own entity: triples saved in the same second
			// have no defined order.
			{Method: "tools/call", Params: `{"name":"kg_add","arguments":{"subject":"conformance-worker","predicate":"deployed_on","object":"fly","cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"kg_query","arguments":{"entity":"conformance-worker","cwd":"{{repo}}"}}`},
		}},
		{Name: "task_tools", Handshake: true, Steps: []conformanceStep{
			{Method: "tools/call", Params: `{"name":"anchored_task","arguments":{"action":"start","key":"conf-1","session_id":"conformance-session","cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_task","arguments":{"action":"note","key":"conf-1","note":"Golden transcript recorded.","session_id":"conformance-session","cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_task","arguments":{"action":"status","key":"conf-1","status":"paused","cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_task","arguments":{"action":"get","key":"conf-1","cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_task","arguments":{"action":"list","cwd":"{{repo}}"}}`},
		}},
		{Name: "optimizer_tools", Handshake: true, Steps: []conformanceStep{
			{Method: "tools/call", Params: `{"name":"anchored_execute","arguments":{"language":"shell","code":"echo conformance-execute"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_execute_file","arguments":{"path":"{{repo}}/notes.md","language":"shell","code":"wc -l < \"$FILE_PATH\" | tr -d ' '"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_index","arguments":{"source":"conformance-notes","content":"# Deploy\n\nThe conformance deploy runs blue-green on Fridays.\n\n# Rollback\n\nRoll back with the previous image tag."}}`},
			{Method: "tools/call", Params: `{"name":"anchored_ctx_search","arguments":{"queries":["blue-green deploy"]}}`},
			{Method: "tools/call", Params: `{"name":"anchored_batch_execute","arguments":{"commands":[{"label":"greet","command":"echo conformance-batch","language":"shell"}],"queries":["conformance-batch"]}}`},
			// The schema says language defaults to shell; today a command
			// without one fails.
			{Method: "tools/call", Params: `{"name":"anchored_batch_execute","arguments":{"commands":[{"label":"greet","command":"echo conformance-batch"}],"queries":["conformance-batch"]}}`},
		}},
		{Name: "errors", Handshake: true, Steps: []conformanceStep{
			{Raw: `this is not json`},
			{Raw: `{"jsonrpc":"1.0","id":91,"method":"ping"}`},
			{Method: "no/such/method", Params: `{}`},
			{Method: "tools/call", Params: `[]`},
			{Method: "tools/call", Params: `{"name":"anchored_nope","arguments":{}}`},
			{Method: "tools/call", Params: `{"name":"anchored_save","arguments":{"category":"fact"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_save","arguments":{"content":"x","category":"opinion"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_save","arguments":"not an object"}`},
			{Method: "tools/call", Params: `{"name":"anchored_update","arguments":{"id":"00000000000000000000000000000000","content":"x"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_skill","arguments":{"action":"search","intent":"deploy","cwd":"{{repo}}"}}`},
			{Method: "tools/call", Params: `{"name":"anchored_fetch_and_index","arguments":{}}`},
			{Method: "resources/read", Params: `[]`},
		}},
	}
}

// conformanceFixture is a full server wired like serveSTDIO: memory service,
// KG with its extractor, session manager and the context optimizer on the
// same store, inside a throwaway HOME and a git repo with a fixed origin.
type conformanceFixture struct {
	dir    string
	repo   string
	server *Server
}

func newConformanceFixture(t *testing.T) *conformanceFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	repo := filepath.Join(dir, "repo")
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "remote", "add", "origin", "https://github.com/test/conformance.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.md"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "test.db")
	cfg.Embedding.Provider = "none"
	cfg.ContextOptimizer.Enabled = true

	svc, err := memory.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	kgSvc := kg.New(svc.StoreDB(), logger)
	svc.SetKGExtractor(kg.NewPatternExtractor(kgSvc, logger))
	sessions := session.NewManager(svc.StoreDB(), logger)
	optimizer, err := NewCtxOptimizer(svc.StoreDB(), cfg.ContextOptimizer, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { optimizer.Close() })

	return &conformanceFixture{dir: dir, repo: repo, server: NewServer(svc, kgSvc, sessions, optimizer, cfg, "test", logger)}
}

// conformanceClient speaks newline-delimited JSON-RPC to ServeStdio.
type conformanceClient struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan []byte
	nextID int
}

func startConformanceClient(t *testing.T, s *Server) *conformanceClient {
	t.Helper()
	return startConformanceClientWith(t, func(ctx context.Context, in io.Reader, out io.Writer) error {
		return s.ServeStdio(ctx, in, out)
	})
}

// startConformanceClientAt serves a session whose root is root, wherever the
// process's working directory is.
func startConformanceClientAt(t *testing.T, s *Server, root string) *conformanceClient {
	t.Helper()
	return startConformanceClientWith(t, func(ctx context.Context, in io.Reader, out io.Writer) error {
		return s.ServeIO(ctx, root, in, out)
	})
}

func startConformanceClientWith(t *testing.T, serve func(ctx context.Context, in io.Reader, out io.Writer) error) *conformanceClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		err := serve(ctx, inR, outW)
		_ = outW.Close()
		done <- err
	}()
	lines := make(chan []byte, 16)
	go func() {
		defer close(lines)
		r := bufio.NewReaderSize(outR, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				lines <- bytes.TrimSpace(line)
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ServeStdio: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("ServeStdio did not return after stdin closed")
		}
		cancel()
	})
	return &conformanceClient{t: t, in: inW, lines: lines}
}

func (c *conformanceClient) send(line []byte) {
	c.t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := c.in.Write(append(line, '\n'))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			c.t.Fatalf("write: %v", err)
		}
	case <-time.After(30 * time.Second):
		c.t.Fatal("write blocked: the server stopped reading its input")
	}
}

// recv returns the next line from the server, or nil when none arrives
// within wait.
func (c *conformanceClient) recv(wait time.Duration) []byte {
	c.t.Helper()
	select {
	case line, ok := <-c.lines:
		if !ok {
			c.t.Fatal("server closed its output")
		}
		return line
	case <-time.After(wait):
		return nil
	}
}

// request builds the wire line for a step; id is 0 for a notification.
func (c *conformanceClient) request(method, params string, notify bool) (json.RawMessage, int) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != "" {
		msg["params"] = json.RawMessage(params)
	}
	id := 0
	if !notify {
		c.nextID++
		id = c.nextID
		msg["id"] = id
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatalf("marshal %s: %v", method, err)
	}
	return raw, id
}

func (c *conformanceClient) handshake() {
	c.t.Helper()
	line, _ := c.request("initialize", conformanceClientInit, false)
	c.send(line)
	if resp := c.recv(30 * time.Second); resp == nil {
		c.t.Fatal("no response to initialize")
	}
	line, _ = c.request("notifications/initialized", "", true)
	c.send(line)
}

type conformanceExchange struct {
	Send json.RawMessage `json:"send"`
	Recv json.RawMessage `json:"recv"`
}

var conformanceMemoryID = regexp.MustCompile(`\b[0-9a-f]{32}\b`)

func TestConformance(t *testing.T) {
	for _, tc := range conformanceCases() {
		t.Run(tc.Name, func(t *testing.T) {
			fx := newConformanceFixture(t)
			client := startConformanceClient(t, fx.server)
			if tc.Handshake {
				client.handshake()
			}
			vars := map[string]string{"repo": fx.repo}
			var transcript []conformanceExchange
			for i, step := range tc.Steps {
				var line []byte
				expectResponse := true
				if step.Raw != "" {
					line = []byte(step.Raw)
				} else {
					params := step.Params
					for k, v := range vars {
						params = strings.ReplaceAll(params, "{{"+k+"}}", v)
					}
					line, _ = client.request(step.Method, params, step.Notify)
					expectResponse = !step.Notify
				}
				client.send(line)
				wait := 60 * time.Second
				if !expectResponse {
					wait = 300 * time.Millisecond
				}
				resp := client.recv(wait)
				if expectResponse && resp == nil {
					t.Fatalf("step %d (%s): no response", i, line)
				}
				if !expectResponse && resp != nil {
					t.Fatalf("step %d (%s): unexpected response %s", i, line, resp)
				}
				if step.Capture != "" {
					id := conformanceMemoryID.FindString(string(resp))
					if id == "" {
						t.Fatalf("step %d: no memory ID to capture in %s", i, resp)
					}
					vars[step.Capture] = id
				}
				transcript = append(transcript, conformanceExchange{
					Send: conformanceCanonical(t, line, fx),
					Recv: conformanceCanonical(t, resp, fx),
				})
			}
			conformanceCompare(t, tc.Name, transcript)
		})
	}
}

// conformanceCanonical re-encodes a message with sorted keys and replaces
// what changes between runs. A line that is not JSON is kept as a string.
func conformanceCanonical(t *testing.T, msg []byte, fx *conformanceFixture) json.RawMessage {
	t.Helper()
	if msg == nil {
		return json.RawMessage("null")
	}
	var v any
	if err := json.Unmarshal(msg, &v); err != nil {
		v = string(msg)
	}
	v = conformanceNormalize(v, fx)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return json.RawMessage(bytes.TrimSpace(buf.Bytes()))
}

var conformanceVolatile = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\b[0-9a-f]{32}\b`), "<id>"},
	{regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<uuid>"},
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2})?`), "<time>"},
	{regexp.MustCompile(`\b\d+(\.\d+)?(ns|µs|us|ms)\b`), "<dur>"},
	{regexp.MustCompile(`\b\d+\.\d+s\b`), "<dur>"},
}

// conformanceProtocolDates are dates that name MCP protocol versions and must
// survive normalization.
var conformanceProtocolDates = regexp.MustCompile(`^(2024-11-05|2025-03-26|2025-06-18|2025-11-25|1999-01-01)$`)

var conformanceDate = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)

func conformanceNormalize(v any, fx *conformanceFixture) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = conformanceNormalize(e, fx)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = conformanceNormalize(e, fx)
		}
		return x
	case string:
		return conformanceNormalizeString(x, fx)
	default:
		return v
	}
}

func conformanceNormalizeString(s string, fx *conformanceFixture) string {
	for _, dir := range conformanceDirs(fx.dir) {
		s = strings.ReplaceAll(s, dir, "<TMP>")
	}
	for _, r := range conformanceVolatile {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return conformanceDate.ReplaceAllStringFunc(s, func(d string) string {
		if conformanceProtocolDates.MatchString(d) {
			return d
		}
		return "<date>"
	})
}

// conformanceDirs is the temp dir as given and as resolved, longest first: on
// macOS t.TempDir() lives under /var, a symlink to /private/var.
func conformanceDirs(dir string) []string {
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		dirs = append([]string{real}, dirs...)
	}
	return dirs
}

func conformanceCompare(t *testing.T, name string, transcript []conformanceExchange) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(transcript); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	path := filepath.Join("testdata", "conformance", name+".golden.json")
	if *updateConformance {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-conformance): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s drifted from its golden transcript; diff it against %s:\n%s", name, path, got)
	}
}
