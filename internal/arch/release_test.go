package arch

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// The release workflow grants id-token: write so cosign can sign keylessly,
// and a permission granted at the workflow level reaches every job in it. That
// sets ACTIONS_ID_TOKEN_REQUEST_URL, which CI never sets — so a test that
// reads the ambient environment passes everywhere a developer or a pull
// request would look, and fails only once a tag is pushed.
//
// That is the worst moment to find out, and it cost two tags: v0.3.0 and
// v0.3.1 are tagged with no release attached. The job that runs the suite must
// therefore override permissions down to what CI has, and this is here because
// the alternative is a comment in a YAML file that nothing reads.
func TestTheReleaseSuiteRunsInTheEnvironmentCIRunsIn(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}

	var wf struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Needs       any               `yaml:"needs"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}

	// Find whichever job runs the suite, rather than assuming its name.
	var testJob string
	for name, job := range wf.Jobs {
		for _, s := range job.Steps {
			if s.Run != "" && contains(s.Run, "go test") {
				testJob = name
			}
		}
	}
	if testJob == "" {
		t.Fatal("no job in release.yml runs the test suite; a tag can be moved, " +
			"so this is the last check before artifacts exist that anyone will trust")
	}

	job := wf.Jobs[testJob]
	if job.Permissions == nil {
		t.Fatalf("job %q inherits the workflow's permissions, which include %v:\n"+
			"\tid-token sets ACTIONS_ID_TOKEN_REQUEST_URL, which CI does not set,\n"+
			"\tso a test reading the environment fails only once a tag is pushed",
			testJob, keys(wf.Permissions))
	}
	if _, granted := job.Permissions["id-token"]; granted {
		t.Errorf("job %q grants id-token, so it does not run in CI's environment", testJob)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
