package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatchParentPoll_CancelsOnReparent simulates the parent exiting: getppid
// starts returning a different PID, so the watchdog must cancel.
func TestWatchParentPoll_CancelsOnReparent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var reparented atomic.Bool
	getppid := func() int {
		if reparented.Load() {
			return 1 // reparented to init after the "parent" exited
		}
		return 4242 // original parent
	}

	cancelled := make(chan struct{})
	go watchParentPoll(ctx, func() { close(cancelled) }, slog.Default(), getppid, 5*time.Millisecond)

	// let a couple of polls confirm the parent is still alive (no cancel yet)
	time.Sleep(25 * time.Millisecond)
	select {
	case <-cancelled:
		t.Fatal("cancelled while parent PID was unchanged")
	default:
	}

	reparented.Store(true)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("did not cancel after reparent")
	}
}

// TestWatchParentPoll_NoParentReturnsImmediately: an already-orphaned process
// (ppid <= 1) has nothing to watch and must return without cancelling.
func TestWatchParentPoll_NoParentReturnsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cancelledCalls atomic.Int32
	done := make(chan struct{})
	go func() {
		watchParentPoll(ctx, func() { cancelledCalls.Add(1) }, slog.Default(), func() int { return 1 }, time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchParentPoll did not return for an orphaned process")
	}
	if cancelledCalls.Load() != 0 {
		t.Errorf("cancel called %d times, want 0", cancelledCalls.Load())
	}
}

// TestWatchParentPoll_StopsOnContextDone: cancelling the context must stop the
// watchdog goroutine without invoking the shutdown cancel.
func TestWatchParentPoll_StopsOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var cancelledCalls atomic.Int32
	done := make(chan struct{})
	go func() {
		watchParentPoll(ctx, func() { cancelledCalls.Add(1) }, slog.Default(), func() int { return 4242 }, 5*time.Millisecond)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchParentPoll did not return after context cancel")
	}
	if cancelledCalls.Load() != 0 {
		t.Errorf("shutdown cancel called %d times on ctx-done exit, want 0", cancelledCalls.Load())
	}
}
