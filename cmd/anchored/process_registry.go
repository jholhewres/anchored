package main

import (
	"context"

	"github.com/jholhewres/anchored/pkg/memory"
)

// registerProcess lists this process in the database's process registry until
// the returned stop is called, which waits for the row to be removed. A
// process that dies without calling stop is pruned by the next reader.
func registerProcess(svc *memory.Service, role string, capabilities ...string) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunProcessRegistration(ctx, memory.LocalProcess(Version, role, capabilities...))
	}()
	return func() {
		cancel()
		<-done
	}
}
