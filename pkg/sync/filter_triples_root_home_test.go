package sync

import "testing"

// TestSafeTriplesForRemote_RootHomeIsALocalPath is an exploit test: the home
// path patterns miss the root user's home, so a triple naming a file there
// leaves the machine. It fails while /root is not a local path to the filter.
func TestSafeTriplesForRemote_RootHomeIsALocalPath(t *testing.T) {
	safe, withheld := SafeTriplesForRemote([]SyncTriple{{Subject: "backup", Predicate: "reads", Object: "/root/.ssh/id_ed25519"}}, "/home/dev/proj")
	if withheld != 1 || len(safe) != 0 {
		t.Fatalf("a path in root's home went out as %+v; it must keep the triple local", safe)
	}
}
