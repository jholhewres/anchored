package mcp

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadFrame_BoundsEachLine(t *testing.T) {
	const limit = 8
	r := bufio.NewReaderSize(strings.NewReader("12345678\n123456789\nok\nlast"), 16)
	for _, want := range []struct {
		line    string
		tooLong bool
	}{{"12345678\n", false}, {"", true}, {"ok\n", false}, {"last", false}} {
		line, tooLong, _ := readFrame(r, limit)
		if string(line) != want.line || tooLong != want.tooLong {
			t.Fatalf("readFrame = (%q, %v), want (%q, %v)", line, tooLong, want.line, want.tooLong)
		}
	}
}

func TestCheckFrame_Classifies(t *testing.T) {
	for _, tc := range []struct {
		line string
		code int
		id   string
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"ping"}`, 0, ""},
		{`not json`, -32700, ""},
		{`{"jsonrpc":"1.0","id":7,"method":"ping"}`, -32600, "7"},
		{`{"jsonrpc":"1.0","id":{"x":1},"method":"ping"}`, -32600, ""},
		{`[]`, -32600, ""},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, -32600, ""},
	} {
		code, id, _ := checkFrame([]byte(tc.line))
		if code != tc.code || string(id) != tc.id {
			t.Errorf("checkFrame(%s) = (%d, %s), want (%d, %s)", tc.line, code, id, tc.code, tc.id)
		}
	}
}
