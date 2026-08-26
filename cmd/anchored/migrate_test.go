package main

import (
	"errors"
	"testing"
)

func TestIsMissingTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "missing table", err: errors.New("SQL logic error: no such table: kg_triples (1)"), want: true},
		{name: "locked database", err: errors.New("database is locked"), want: false},
		{name: "missing column", err: errors.New("SQL logic error: no such column: valid_to (1)"), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMissingTable(tc.err); got != tc.want {
				t.Fatalf("isMissingTable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
