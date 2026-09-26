package main

import (
	"testing"
)

func TestIsShortLivedInvocation(t *testing.T) {
	cases := map[string]bool{
		"anchored":                               false,
		"anchored serve --stdio":                 false,
		"anchored --config /x/config.yaml":       false,
		"anchored dashboard --no-open --addr x":  false,
		"anchored hub serve":                     false,
		"anchored hook stop":                     true,
		"anchored maintenance run":               true,
		"anchored search foo":                    true,
		"anchored --config /x/c.yaml search foo": true,
		"anchored --config=/x/c.yaml hook stop":  true,
		"anchored --config /x/c.yaml":            false,
	}
	for args, want := range cases {
		if got := isShortLivedInvocation(args); got != want {
			t.Errorf("%q: got %v, want %v", args, got, want)
		}
	}
}
