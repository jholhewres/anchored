package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxFrameBytes bounds one inbound JSON-RPC line. A longer line is answered
// with an error and skipped; the session goes on.
const maxFrameBytes = sdk.DefaultMaxLineLength

// ServeStdio serves one MCP session over newline-delimited JSON-RPC on in and
// out, rooted at the process's working directory, until in reaches EOF or
// ctx is cancelled. Both end the session cleanly and return nil; only a
// genuine read or write error is returned.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	root, err := os.Getwd()
	if err != nil {
		root = ""
	}
	return s.ServeIO(ctx, root, in, out)
}

// ServeIO serves one MCP session whose omitted and relative cwd arguments
// resolve against root.
func (s *Server) ServeIO(ctx context.Context, root string, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w := &frameWriter{w: out}
	frames, toSDK := io.Pipe()
	readErr := make(chan error, 1)
	go func() {
		err := s.forwardFrames(ctx, in, toSDK, w)
		readErr <- err
		if cerr := toSDK.CloseWithError(err); cerr != nil {
			s.logger.Debug("mcp: close the SDK's input", "error", cerr)
		}
	}()

	session, err := s.newSDKServer(&mcpSession{root: root}).Connect(ctx, &sdk.IOTransport{
		Reader:        frames,
		Writer:        w,
		MaxLineLength: -1, // forwardFrames already bounds each line
	}, nil)
	if err != nil {
		return fmt.Errorf("mcp connect: %w", err)
	}
	// Wait's own error only restates why the input ended; readErr says it
	// precisely.
	waited := make(chan struct{})
	go func() {
		if err := session.Wait(); err != nil {
			s.logger.Debug("mcp session ended", "error", err)
		}
		close(waited)
	}()

	select {
	case <-ctx.Done():
		if err := session.Close(); err != nil {
			s.logger.Debug("mcp: close the session", "error", err)
		}
		<-waited
		return nil
	case <-waited:
	}
	// The session ends when its input does: report why the input ended. The
	// reader may still be blocked on stdin, which a cancelled context cannot
	// interrupt; it is abandoned and reaped when the process exits.
	select {
	case err := <-readErr:
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("stdin read: %w", err)
		}
	default:
	}
	if err := w.err(); err != nil {
		return fmt.Errorf("stdout write: %w", err)
	}
	return nil
}

// forwardFrames copies in to the SDK line by line. A line that is not JSON,
// is not a JSON-RPC message, or is longer than maxFrameBytes is answered on w
// with the matching JSON-RPC error and never reaches the SDK, whose reader
// would otherwise end the session on it. It returns nil at EOF.
func (s *Server) forwardFrames(ctx context.Context, in io.Reader, sdkIn io.Writer, w *frameWriter) error {
	r := bufio.NewReaderSize(in, 64*1024)
	for {
		line, tooLong, err := readFrame(r, maxFrameBytes)
		if tooLong {
			s.dlog.Event("mcp.parse_error", map[string]any{"error": "frame too long"})
			w.writeError(nil, jsonrpc.CodeInvalidRequest, fmt.Sprintf("message exceeds %d bytes", maxFrameBytes))
		} else if line = bytes.TrimSpace(line); len(line) > 0 {
			if code, id, msg := checkFrame(line); code != 0 {
				s.dlog.Event("mcp.parse_error", map[string]any{"error": msg, "raw": debuglog.Content(string(line), 200)})
				w.writeError(id, code, msg)
			} else if _, werr := sdkIn.Write(append(line, '\n')); werr != nil {
				return nil // the session is gone
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// readFrame reads one line of at most limit bytes, without its newline. A
// longer line is consumed whole and reported as tooLong.
func readFrame(r *bufio.Reader, limit int) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > limit+1 { // +1 for the newline
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, tooLong, err
	}
}

// checkFrame is the JSON-RPC error a line earns before it reaches the SDK:
// -32700 for a line that is not JSON, -32600 for JSON that is not a single
// JSON-RPC message. It returns code 0 for a line the SDK can read.
//
// Batches are refused whole: MCP dropped them in 2025-06-18, the old server
// never read them, and the SDK's reader ends the session on a batch it
// rejects (one after 2025-06-18, or with a repeated id).
func checkFrame(line []byte) (code int, id json.RawMessage, msg string) {
	if !json.Valid(line) {
		return jsonrpc.CodeParseError, nil, "parse error: invalid JSON"
	}
	if line[0] == '[' {
		return jsonrpc.CodeInvalidRequest, nil, "invalid request: JSON-RPC batches are not supported"
	}
	if _, err := jsonrpc.DecodeMessage(line); err != nil {
		var envelope struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(line, &envelope) == nil && validID(envelope.ID) {
			id = envelope.ID
		}
		return jsonrpc.CodeInvalidRequest, id, "invalid request: " + err.Error()
	}
	return 0, nil, ""
}

// validID reports whether raw is a JSON-RPC id an error response may echo: a
// string or a number.
func validID(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

// frameWriter serialises whole messages from the SDK and from
// forwardFrames onto one output, and keeps the first write error.
type frameWriter struct {
	mu       sync.Mutex
	w        io.Writer
	firstErr error
}

func (f *frameWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.w.Write(p)
	if err != nil && f.firstErr == nil {
		f.firstErr = err
	}
	return n, err
}

// writeLine writes one whole message; a failure is kept for err.
func (f *frameWriter) writeLine(line []byte) {
	if _, err := f.Write(line); err != nil {
		return // kept in firstErr
	}
}

func (f *frameWriter) Close() error { return nil }

func (f *frameWriter) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.firstErr
}

// writeError answers a line the SDK never saw. id nil is written as null.
func (f *frameWriter) writeError(id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	data, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   jsonrpc.Error   `json:"error"`
	}{"2.0", id, jsonrpc.Error{Code: int64(code), Message: message}})
	if err != nil {
		return
	}
	f.writeLine(append(data, '\n'))
}
