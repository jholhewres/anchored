package main

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

func newReorderFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("category", "", "a string flag")
	fs.Bool("global", false, "a bool flag")
	return fs
}

// An unregistered flag must not take the process down. `anchored search --help`
// hit exactly this: fs.Lookup returns nil for --help, and reading f.Value before
// the nil check panicked with SIGSEGV on every subcommand that reorders args.
func TestReorderArgsForFlagSurvivesUnknownFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"query text", "--help"},
		{"--nope", "value", "query text"},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panicked on %v: %v", args, r)
				}
			}()
			reorderArgsForFlag(newReorderFlagSet(), args)
		}()
	}
}

func TestReorderArgsForFlagKeepsKnownFlagBehavior(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "string flag pulls its value ahead of the positional",
			in:   []string{"query text", "--category", "fact"},
			want: []string{"--category", "fact", "query text"},
		},
		{
			name: "bool flag does not consume the next arg",
			in:   []string{"query text", "--global"},
			want: []string{"--global", "query text"},
		},
		{
			name: "string flag with no value gets an empty assignment",
			in:   []string{"--category"},
			want: []string{"--category="},
		},
		{
			name: "explicit assignment passes through",
			in:   []string{"--category=fact", "query text"},
			want: []string{"--category=fact", "query text"},
		},
		{
			name: "unknown flag is passed through for fs.Parse to report",
			in:   []string{"--help", "query text"},
			want: []string{"--help", "query text"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reorderArgsForFlag(newReorderFlagSet(), tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("reorderArgsForFlag(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
