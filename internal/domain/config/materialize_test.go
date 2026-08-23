package config

import (
	"strings"
	"testing"
)

// withMaterialize extends the fixture's existing prove block rather than
// adding a second one, which yaml rejects as a duplicate key.
func withMaterialize(entry string) string {
	return strings.Replace(minimal,
		"prove:\n  from: warden",
		"prove:\n  from: warden\n  materialize: [\""+entry+"\"]", 1)
}

func TestMaterializeParses(t *testing.T) {
	p := parse(t, withMaterialize("node_modules"))

	if len(p.Prove.Materialize) != 1 || p.Prove.Materialize[0] != "node_modules" {
		t.Errorf("Materialize = %v", p.Prove.Materialize)
	}
}

// Refused where the message can explain itself, rather than only at the
// boundary where it would be a bare error.
func TestMaterializeMustStayInsideTheRepository(t *testing.T) {
	for _, bad := range []string{"../../.ssh", "/etc/passwd"} {
		if _, err := Parse(strings.NewReader(withMaterialize(bad))); err == nil {
			t.Errorf("materialize %q was accepted", bad)
		}
	}
}
