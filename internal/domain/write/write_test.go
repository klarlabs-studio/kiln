package write

import "testing"

func TestOwnedAcceptsTheKilnNamespace(t *testing.T) {
	for _, branch := range []string{"kiln/remediate", "kiln/nox-fix", "kiln/docs"} {
		if err := Owned(branch); err != nil {
			t.Errorf("Owned(%q) = %v, want nil", branch, err)
		}
	}
}

func TestOwnedRefusesSourceOfTruth(t *testing.T) {
	for _, branch := range []string{"main", "master", "develop", "release/1.0", "refs/heads/main"} {
		if err := Owned(branch); err == nil {
			t.Errorf("Owned(%q) accepted a source-of-truth branch", branch)
		}
	}
}

func TestOwnedRefusesEscape(t *testing.T) {
	for _, branch := range []string{"kiln/../main", "kiln/", "", "kiln/foo:refs/heads/main"} {
		if err := Owned(branch); err == nil {
			t.Errorf("Owned(%q) accepted an escape", branch)
		}
	}
}
