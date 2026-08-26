package main

import "testing"

func TestServeConfigPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default", args: nil, want: ""},
		{name: "separate value", args: []string{"--config", "/tmp/anchored-e2e.yaml"}, want: "/tmp/anchored-e2e.yaml"},
		{name: "equals value", args: []string{"--config=/tmp/anchored-e2e.yaml"}, want: "/tmp/anchored-e2e.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serveConfigPath(tc.args); got != tc.want {
				t.Fatalf("serveConfigPath(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
