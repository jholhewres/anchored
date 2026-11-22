package sync

import "testing"

// TestSafeTriplesForRemote_SecretSplitAcrossFields is an exploit test: a
// credential whose name sits in one field and whose value sits in another
// matches no rule field by field, so the triple leaves the machine, while the
// same fact saved as a memory is blocked. It fails while each field is
// filtered alone.
func TestSafeTriplesForRemote_SecretSplitAcrossFields(t *testing.T) {
	root := "/home/dev/proj"
	if r := RemoteSafetyFilter("staging-db password: NotReal2Pass!", nil, root); !r.Blocked {
		t.Fatalf("control: the memory form is not blocked: %+v", r)
	}
	for _, tr := range []SyncTriple{
		{Subject: "staging-db", Predicate: "password", Object: "NotReal2Pass!"},
		{Subject: "billing", Predicate: "api_key", Object: "fake0123456789abcdefwxyz"},
		{Subject: "ci", Predicate: "token", Object: "fake9876543210fedcbazyxw"},
		{Subject: "staging-db-password", Predicate: "is", Object: "NotReal2Pass!"},
	} {
		safe, withheld := SafeTriplesForRemote([]SyncTriple{tr}, root)
		if withheld != 1 || len(safe) != 0 {
			t.Errorf("triple %+v went out as %+v; a secret's name in one field and its value in another must keep the triple local", tr, safe)
		}
	}
	safe, withheld := SafeTriplesForRemote([]SyncTriple{{Subject: "api", Predicate: "uses", Object: "postgres"}}, root)
	if withheld != 0 || len(safe) != 1 {
		t.Fatalf("a clean triple was held back: sent %+v, withheld %d", safe, withheld)
	}
}
