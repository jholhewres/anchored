package memory

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Replace fans the copy+quantize pass across cores. The result must be
// identical to the sequential form, including the memoized quantized entries
// that scoring reads.
func TestReplaceIsEquivalentAcrossBatchSizes(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, n := range []int{0, 1, parallelForMinBatch - 1, parallelForMinBatch, parallelForMinBatch * 3} {
		vectors := make(map[string][]float32, n)
		for i := 0; i < n; i++ {
			v := make([]float32, 8)
			for d := range v {
				v[d] = rng.Float32()*2 - 1
			}
			vectors[string(rune('a'+i%26))+string(rune('0'+i/26))] = v
		}

		parallel := NewVectorCache(discardLogger())
		parallel.Replace(vectors)

		sequential := NewVectorCache(discardLogger())
		for id, v := range vectors {
			sequential.Put(id, v)
		}

		if parallel.Len() != sequential.Len() {
			t.Fatalf("n=%d: len %d != %d", n, parallel.Len(), sequential.Len())
		}
		if !reflect.DeepEqual(parallel.All(), sequential.All()) {
			t.Fatalf("n=%d: exact vectors diverged", n)
		}
		for id, want := range sequential.quant {
			got, ok := parallel.quant[id]
			if !ok {
				t.Fatalf("n=%d: %s missing from quantized map", n, id)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("n=%d: %s quantized entry diverged", n, id)
			}
		}
	}
}

// Replace must not leave entries from the previous generation behind.
func TestReplaceEvictsPreviousContents(t *testing.T) {
	c := NewVectorCache(discardLogger())
	c.Replace(map[string][]float32{"old": {1, 0}})
	c.Replace(map[string][]float32{"new": {0, 1}})

	if _, ok := c.Get("old"); ok {
		t.Fatal("old entry survived Replace")
	}
	if _, ok := c.Get("new"); !ok {
		t.Fatal("new entry missing after Replace")
	}
	if c.Len() != 1 {
		t.Fatalf("len = %d, want 1", c.Len())
	}
}

func TestWaitWarmReturnsImmediatelyWhenNoFillIsRunning(t *testing.T) {
	c := NewVectorCache(discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !c.WaitWarm(ctx) {
		t.Fatal("WaitWarm should settle immediately with no fill in flight")
	}
}

// A search arriving mid-fill has to see the finished cache, not the empty one
// it started from.
func TestWaitWarmBlocksUntilFillCompletes(t *testing.T) {
	c := NewVectorCache(discardLogger())
	done := c.BeginWarm()

	filled := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		c.Replace(map[string][]float32{"a": {1, 0}})
		done()
		close(filled)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !c.WaitWarm(ctx) {
		t.Fatal("WaitWarm reported an unsettled cache")
	}
	<-filled
	if c.Len() != 1 {
		t.Fatalf("len after warm = %d, want 1", c.Len())
	}
}

func TestWaitWarmHonoursContextCancellation(t *testing.T) {
	c := NewVectorCache(discardLogger())
	done := c.BeginWarm()
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if c.WaitWarm(ctx) {
		t.Fatal("WaitWarm should report an unsettled cache when the caller gives up")
	}
}

// Concurrent BeginWarm calls collapse into one handshake; finishing any of them
// releases every waiter exactly once.
func TestBeginWarmIsReentrant(t *testing.T) {
	c := NewVectorCache(discardLogger())
	first := c.BeginWarm()
	second := c.BeginWarm()

	first()
	second() // must not panic on a second close

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !c.WaitWarm(ctx) {
		t.Fatal("cache should be settled after the warm finished")
	}
}
