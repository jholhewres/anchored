package memory

import "testing"

func TestVectorCache_RemoveDropsTheVector(t *testing.T) {
	c := NewVectorCache(discardLogger())
	c.Put("a", []float32{0.6, 0.8})
	c.Put("b", []float32{1, 0})
	c.Remove("a")
	if _, ok := c.Get("a"); ok || c.Len() != 1 {
		t.Fatalf("after Remove: present=%v len=%d", ok, c.Len())
	}
	if got := c.Score([]float32{0.6, 0.8}, 1, -1, 10); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("a removed vector still scores: %v", got)
	}
}
