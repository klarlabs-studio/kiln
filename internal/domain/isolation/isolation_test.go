package isolation

import "testing"

// The table in the design doc, transcribed. If a row here ever changes, that is
// a security decision and must be a deliberate one.
func TestForMatchesTheMatrix(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		fork  bool
		want  Policy
	}{
		{"fork PR gets nothing", EventPullRequest, true, Policy{}},
		{"same-repo PR may skip only", EventPullRequest, false, Policy{Skip: true}},
		{"push gets everything", EventPush, false, Policy{Secrets: true, Publish: true, Skip: true}},
		{"tag gets everything", EventTag, false, Policy{Secrets: true, Publish: true, Skip: true}},
		{"push fork flag is irrelevant", EventPush, true, Policy{Secrets: true, Publish: true, Skip: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := For(tt.event, tt.fork); got != tt.want {
				t.Errorf("For(%s, fork=%v) = %+v, want %+v", tt.event, tt.fork, got, tt.want)
			}
		})
	}
}

func TestUnknownEventDeniesEverything(t *testing.T) {
	for _, fork := range []bool{true, false} {
		if got := For(Event("workflow_dispatch"), fork); got != (Policy{}) {
			t.Errorf("unknown event (fork=%v) = %+v, want deny-all", fork, got)
		}
	}
}

func TestZeroPolicyIsDenyAll(t *testing.T) {
	var p Policy
	if p.Secrets || p.Publish || p.Skip {
		t.Error("the zero Policy must permit nothing")
	}
}

func TestParseEvent(t *testing.T) {
	for _, ok := range []string{"pull_request", "push", "tag"} {
		if _, valid := ParseEvent(ok); !valid {
			t.Errorf("ParseEvent(%q) rejected a known event", ok)
		}
	}
	for _, bad := range []string{"", "PUSH", "release", "pull-request"} {
		if _, valid := ParseEvent(bad); valid {
			t.Errorf("ParseEvent(%q) accepted an unknown event", bad)
		}
	}
}
