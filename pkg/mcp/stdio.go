package mcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
)

// ServeStdio serves newline-delimited JSON-RPC from in to out until in
// reaches EOF or ctx is cancelled. A cancelled context is an expected
// shutdown and returns nil; only a genuine read error is returned.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// A blocking read on stdin cannot be interrupted by context cancellation
	// (closing os.Stdin does not unblock an in-flight read on fd 0), so the
	// scanner runs in its own goroutine and feeds lines over a channel. The
	// loop selects between incoming lines and ctx.Done(), so a signal or the
	// parent's death returns at once. The reader goroutine is abandoned on
	// shutdown and reaped when the process exits.
	lines := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			b := scanner.Bytes()
			buf := make([]byte, len(b)) // scanner reuses its buffer; copy before handing off
			copy(buf, b)
			select {
			case lines <- buf:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			scanErr <- err
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-lines:
			if !ok {
				// EOF: the client closed the pipe. Surface a genuine read
				// error, but treat a cancelled context as an expected shutdown.
				if err := drainScanErr(scanErr); err != nil && ctx.Err() == nil {
					return fmt.Errorf("stdin read: %w", err)
				}
				return nil
			}
			if len(line) == 0 {
				continue
			}
			if response := s.HandleMessage(ctx, line); response != nil {
				if _, err := fmt.Fprintf(out, "%s\n", response); err != nil {
					return fmt.Errorf("stdout write: %w", err)
				}
			}
		}
	}
}

// drainScanErr returns the reader goroutine's scan error if one has already
// been reported, without blocking when none is pending.
func drainScanErr(scanErr <-chan error) error {
	select {
	case err := <-scanErr:
		return err
	default:
		return nil
	}
}
