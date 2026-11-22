//go:build !windows

package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// sdkMessage is the part of a JSON-RPC response the SDK tests read.
type sdkMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeSDKMessage(t *testing.T, line []byte) sdkMessage {
	t.Helper()
	if line == nil {
		t.Fatal("no response")
	}
	var m sdkMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	return m
}

// call sends one request and returns its response.
func (c *conformanceClient) call(method, params string) sdkMessage {
	c.t.Helper()
	line, _ := c.request(method, params, false)
	c.send(line)
	return decodeSDKMessage(c.t, c.recv(60*time.Second))
}

type sdkToolResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func (c *conformanceClient) callTool(name string, args any) (sdkToolResult, sdkMessage) {
	c.t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		c.t.Fatal(err)
	}
	msg := c.call("tools/call", string(raw))
	var res sdkToolResult
	if msg.Result != nil {
		if err := json.Unmarshal(msg.Result, &res); err != nil {
			c.t.Fatalf("decode result %s: %v", msg.Result, err)
		}
	}
	return res, msg
}

func (r sdkToolResult) text() string {
	var parts []string
	for _, c := range r.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

func TestSDK_SlowToolCallDoesNotBlockConcurrentPing(t *testing.T) {
	fx := newConformanceFixture(t)
	client := startConformanceClient(t, fx.server)
	client.handshake()

	slow, _ := client.request("tools/call", `{"name":"anchored_execute","arguments":{"language":"shell","code":"sleep 2; echo slow-done"}}`, false)
	client.send(slow)
	ping, pingID := client.request("ping", `{}`, false)
	client.send(ping)

	first := decodeSDKMessage(t, client.recv(30*time.Second))
	if string(first.ID) != jsonNumber(pingID) {
		t.Fatalf("first response is id %s, want the ping (id %d): the slow tools/call blocked it", first.ID, pingID)
	}
	if second := client.recv(30 * time.Second); second == nil {
		t.Fatal("the slow tools/call never answered")
	}
}

func jsonNumber(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestSDK_MessageOverOneMiBDoesNotEndTheSession(t *testing.T) {
	fx := newConformanceFixture(t)
	client := startConformanceClient(t, fx.server)
	client.handshake()

	big := strings.Repeat("blue-green deploy notes ", (2<<20)/24)
	res, msg := client.callTool("anchored_index", map[string]any{"source": "big", "content": big})
	if msg.Error != nil || res.IsError {
		t.Fatalf("2 MiB anchored_index failed: %+v %s", msg.Error, res.text())
	}
	if pong := client.call("ping", `{}`); pong.Error != nil {
		t.Fatalf("ping after a 2 MiB message: %+v", pong.Error)
	}
}

func TestSDK_MalformedLineGetsParseErrorWithNullIDAndSessionContinues(t *testing.T) {
	fx := newConformanceFixture(t)
	client := startConformanceClient(t, fx.server)
	client.handshake()

	client.send([]byte(`this is not json`))
	line := client.recv(10 * time.Second)
	msg := decodeSDKMessage(t, line)
	if msg.Error == nil || msg.Error.Code != -32700 {
		t.Fatalf("malformed line got %s, want error -32700", line)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatal(err)
	}
	if id, ok := fields["id"]; !ok || string(id) != "null" {
		t.Fatalf("parse error must carry id null: %s", line)
	}
	if pong := client.call("ping", `{}`); pong.Error != nil {
		t.Fatalf("ping after a malformed line: %+v", pong.Error)
	}
}

func TestSDK_NegotiatesTheRequestedProtocolVersion(t *testing.T) {
	for _, version := range []string{"2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			fx := newConformanceFixture(t)
			client := startConformanceClient(t, fx.server)
			msg := client.call("initialize", `{"protocolVersion":"`+version+`","capabilities":{},"clientInfo":{"name":"sdk-test","version":"1"}}`)
			var res struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(msg.Result, &res); err != nil {
				t.Fatalf("decode %s: %v", msg.Result, err)
			}
			if res.ProtocolVersion != version {
				t.Fatalf("protocolVersion = %q, want %q", res.ProtocolVersion, version)
			}
		})
	}
}

func TestSDK_ToolErrorIsAResultAndUnknownToolIsInvalidParams(t *testing.T) {
	fx := newConformanceFixture(t)
	client := startConformanceClient(t, fx.server)
	client.handshake()

	res, msg := client.callTool("anchored_save", map[string]any{"category": "fact"})
	if msg.Error != nil {
		t.Fatalf("tool execution error came back as JSON-RPC error %+v, want a result with isError", msg.Error)
	}
	if !res.IsError || !strings.Contains(res.text(), "content cannot be empty") {
		t.Fatalf("result = %+v, want isError with the tool's message", res)
	}

	_, msg = client.callTool("anchored_nope", map[string]any{})
	if msg.Error == nil || msg.Error.Code != -32602 {
		t.Fatalf("unknown tool error = %+v, want -32602", msg.Error)
	}
}

func TestSDK_HiddenAliasesAnswerButStayOutOfToolsList(t *testing.T) {
	fx := newConformanceFixture(t)
	client := startConformanceClient(t, fx.server)
	client.handshake()

	for name, args := range map[string]any{
		"kg_add":   map[string]any{"subject": "alias-api", "predicate": "depends_on", "object": "redis", "cwd": fx.repo},
		"kg_query": map[string]any{"entity": "alias-api", "cwd": fx.repo},
	} {
		res, msg := client.callTool(name, args)
		if msg.Error != nil || res.IsError {
			t.Fatalf("%s: %+v %s", name, msg.Error, res.text())
		}
	}

	msg := client.call("tools/list", `{}`)
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(msg.Result, &list); err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool.Name == "kg_add" || tool.Name == "kg_query" {
			t.Fatalf("tools/list advertises the hidden alias %q", tool.Name)
		}
	}
}

func TestSDK_ToolCallsResolveCWDAgainstTheSessionRoot(t *testing.T) {
	fx := newConformanceFixture(t)
	t.Chdir(t.TempDir()) // the process cwd is unrelated to the session
	client := startConformanceClientAt(t, fx.server, fx.repo)
	client.handshake()

	for _, args := range []map[string]any{
		{"content": "Saved with no cwd: it belongs to the session root.", "category": "fact"},
		{"content": "Saved with cwd '.': it belongs to the session root.", "category": "fact", "cwd": "."},
	} {
		if res, msg := client.callTool("anchored_save", args); msg.Error != nil || res.IsError {
			t.Fatalf("save %v: %+v %s", args, msg.Error, res.text())
		}
	}
	res, _ := client.callTool("anchored_context", map[string]any{"cwd": fx.repo})
	if !strings.Contains(res.text(), `memories="2"`) {
		t.Fatalf("the session root's project does not hold both memories:\n%s", res.text())
	}
}

func TestSDK_BatchExecuteRunsACommandWithoutLanguageAsShell(t *testing.T) {
	fx := newConformanceFixture(t)
	client := startConformanceClient(t, fx.server)
	client.handshake()

	res, msg := client.callTool("anchored_batch_execute", map[string]any{
		"commands": []map[string]any{{"label": "greet", "command": "echo sdk-batch-shell"}},
		"queries":  []string{"sdk-batch-shell"},
	})
	if msg.Error != nil || res.IsError || strings.Contains(res.text(), "unsupported language") {
		t.Fatalf("batch without language: %+v %s", msg.Error, res.text())
	}
	if !strings.Contains(res.text(), "sdk-batch-shell") {
		t.Fatalf("batch output missing the command's stdout:\n%s", res.text())
	}
}

// concurrentFacts are distinct enough that none is a near-duplicate of
// another, so each save adds a memory.
var concurrentFacts = []string{
	"The ledger service writes invoices to Postgres 16.",
	"Kafka retention for the audit topic is seven days.",
	"Grafana dashboards live in the observability repo.",
	"The mobile app ships through Fastlane every Tuesday.",
	"Redis eviction policy on the cache cluster is allkeys-lru.",
	"Terraform state for staging sits in an encrypted bucket.",
	"Payments webhooks retry with exponential backoff up to an hour.",
	"The search index rebuilds nightly from the warehouse snapshot.",
	"Feature flags are evaluated client side with a 30 second cache.",
	"On-call rotation hands over at nine in the morning, Lisbon time.",
	"Image thumbnails are generated by a Lambda written in Rust.",
	"The CI runners use ephemeral ARM machines with 8 cores.",
}

func TestSDK_ConcurrentToolCallsInOneSession(t *testing.T) {
	fx := newConformanceFixture(t)
	cs := sdkClientSession(t, fx.server, fx.repo)
	ctx := t.Context()

	workers := len(concurrentFacts)
	errs := make(chan error, workers*3)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			calls := []sdk.CallToolParams{
				{Name: "anchored_save", Arguments: map[string]any{"content": concurrentFacts[i], "category": "fact"}},
				{Name: "anchored_search", Arguments: map[string]any{"query": "ledger", "cwd": "."}},
				{Name: "anchored_ctx_search", Arguments: map[string]any{"queries": []string{"ledger"}}},
			}
			for _, call := range calls {
				res, err := cs.CallTool(ctx, &call)
				if err != nil {
					errs <- fmt.Errorf("%s: %w", call.Name, err)
					continue
				}
				if res.IsError {
					errs <- fmt.Errorf("%s: isError %v", call.Name, res.Content)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "anchored_context", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*sdk.TextContent).Text
	if !strings.Contains(text, fmt.Sprintf(`memories="%d"`, workers)) {
		t.Fatalf("after %d concurrent saves the session root's project shows: %q", workers, text)
	}
}
