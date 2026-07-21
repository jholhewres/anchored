package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

func TestPrintQueueHealthUsesStableStateOrder(t *testing.T) {
	oldest := time.Now().Add(-90 * time.Second)
	health := memory.QueueStateHealth{
		Counts: map[string]int64{
			"failed": 2, "processing": 1, "pending": 3, "done": 4,
		},
		OldestPending: &oldest,
	}
	var output bytes.Buffer
	printQueueHealth(&output, "processing", health,
		[]string{"pending", "processing", "done", "failed"})

	got := output.String()
	if !strings.HasPrefix(got,
		"  processing: pending=3 processing=1 done=4 failed=2 oldest_pending_age=") {
		t.Fatalf("output=%q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("output missing newline: %q", got)
	}
}
