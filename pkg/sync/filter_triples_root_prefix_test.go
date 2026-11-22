package sync

import "testing"

// TestSafeTriplesForRemote_PathOnlySharingTheRootPrefix is an exploit test:
// the inside-the-project check is a bare string prefix on an uncleaned path,
// so a sibling directory and a path that climbs out with .. count as inside
// and go out. It fails while the check has no separator boundary.
func TestSafeTriplesForRemote_PathOnlySharingTheRootPrefix(t *testing.T) {
	root := "/home/dev/proj"
	for _, tr := range []SyncTriple{
		{Subject: "deploy", Predicate: "reads", Object: "/home/dev/proj-private/notes"},
		{Subject: "backup", Predicate: "reads", Object: "/home/dev/proj/../../someone/.ssh/id_ed25519"},
	} {
		safe, withheld := SafeTriplesForRemote([]SyncTriple{tr}, root)
		if withheld != 1 || len(safe) != 0 {
			t.Errorf("triple %+v went out as %+v; a path outside the project must keep it local", tr, safe)
		}
	}
	safe, withheld := SafeTriplesForRemote([]SyncTriple{{Subject: "build", Predicate: "writes", Object: "/home/dev/proj/bin/anchored"}}, root)
	if withheld != 0 || len(safe) != 1 || safe[0].Object != "./bin/anchored" {
		t.Fatalf("an in-project path: sent %+v, withheld %d; want it relative", safe, withheld)
	}
}
