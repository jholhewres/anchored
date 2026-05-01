package main

import (
	"testing"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"hello\nworld", 20, "hello world"},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := truncate(tt.input, tt.max)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
		}
	}
}

func TestProjDisplay(t *testing.T) {
	if projDisplay("") != "" {
		t.Errorf("expected empty for empty project")
	}
	if projDisplay("foo") != " (foo)" {
		t.Errorf("expected ' (foo)', got %q", projDisplay("foo"))
	}
}
