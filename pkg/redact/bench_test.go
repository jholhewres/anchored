package redact

import (
	"strings"
	"testing"
)

// Tool output handed to the debug log or a memory can be megabytes; RE2 keeps
// the cost linear, and this tracks the constant.
func BenchmarkString_1MB(b *testing.B) {
	line := "2026-09-25 build ok: go test ./... passed in 12.3s, see https://ci.example.com/run/42?page=1 token_count=1234 \n"
	text := strings.Repeat(line, (1<<20)/len(line))
	b.SetBytes(int64(len(text)))
	for i := 0; i < b.N; i++ {
		_ = String(text)
	}
}
