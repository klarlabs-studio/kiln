package attest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/attest"
)

// examplePath is a real predicate, committed so consumers have something
// concrete to write their parser against.
var examplePath = filepath.Join("..", "..", "examples", "provenance.example.json")

// canonical is the statement the example file must contain. Fixed inputs, so
// the file is byte-stable and a diff means the wire format actually changed.
func canonical(t *testing.T) attest.Statement {
	t.Helper()
	s, err := attest.Build(attest.Input{
		SubjectName:   "ghcr.io/felixgeelhaar/glossa-api",
		SubjectDigest: "sha256:9f2c1e4a7b3d5086c1f9a2b4d6e8103b5c7d9e1f2a3b4c5d6e7f8091a2b3c4d5",
		Repo:          "felixgeelhaar/glossa",
		SHA:           "c3f7aca23fa4bfa8d65b3741f46c509713cd618e",
		Ref:           "refs/tags/v0.2.0",
		Event:         "tag",
		ArtifactKind:  "image",
		Config:        "Dockerfile",
		GateTool:      "warden",
		GateReproved:  true,
		GateReason:    "warden gate passed: vet, test, lint",
		KilnVersion:   "v0.1.0",
		ToolVersions:  map[string]string{"warden": "0.28.0", "cosign": "3.1.3"},
		InvocationID:  "run-20260818T060000Z-1a2b3c4d",
		StartedOn:     time.Date(2026, 8, 18, 6, 0, 0, 0, time.UTC),
		FinishedOn:    time.Date(2026, 8, 18, 6, 1, 30, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTheExampleMatchesWhatKilnEmits keeps the published contract honest.
//
// examples/provenance.example.json is what a consumer — RollOps, or anyone
// verifying a kiln artifact — writes their parser against. If the emitted
// shape changes and the example does not, that consumer breaks in production
// against a file that told them otherwise. Regenerate with:
//
//	go test ./internal/attest -run TestTheExampleMatchesWhatKilnEmits -update
func TestTheExampleMatchesWhatKilnEmits(t *testing.T) {
	want, err := canonical(t).JSON()
	if err != nil {
		t.Fatal(err)
	}

	if *update {
		if err := os.WriteFile(examplePath, want, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Log("rewrote " + examplePath)
		return
	}

	got, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read the published example: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("examples/provenance.example.json is stale — consumers are reading a shape kiln no longer emits.\n"+
			"Regenerate: go test ./internal/attest -run TestTheExampleMatchesWhatKilnEmits -update\n\nhave:\n%s\nwant:\n%s",
			got, want)
	}
}

// TestTheContractFieldsConsumersReadAreStable pins the exact JSON paths named
// in docs/rollops-handoff.md.
//
// A Go rename that changed one of these would compile, pass every other test,
// and silently break every consumer. This is the test that fails instead.
func TestTheContractFieldsConsumersReadAreStable(t *testing.T) {
	raw, err := canonical(t).JSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"predicateType",
		"subject.0.digest.sha256",
		"predicate.buildDefinition.buildType",
		"predicate.buildDefinition.resolvedDependencies.0.digest.gitCommit",
		"predicate.buildDefinition.internalParameters.sourceGate.verified",
		"predicate.buildDefinition.internalParameters.sourceGate.reproved",
		"predicate.buildDefinition.internalParameters.isolated",
		"predicate.runDetails.builder.id",
	} {
		if _, ok := lookup(doc, path); !ok {
			t.Errorf("the published contract path %q is gone; consumers read it by name", path)
		}
	}
}

// lookup walks a dotted path, treating an all-digit segment as a slice index.
func lookup(doc any, path string) (any, bool) {
	cur := doc
	for _, seg := range splitPath(path) {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := index(seg)
			if err != nil || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}
