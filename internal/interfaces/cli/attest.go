package cli

import (
	"context"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/infrastructure/attest"
)

// runAttest emits the provenance predicate for an artifact this machine built,
// without kiln having built it.
//
// The verification half of kiln already works for anyone: `kiln verify
// --policy` names allowed builders and reads the attestation off the artifact,
// and the shipped example lists a GitHub Actions identity beside kiln's own.
// The producing half did not. A GitLab pipeline could emit SLSA provenance,
// but not the two fields that make the chain worth verifying — the commit the
// artifact was built from, and the source gate's verdict about that commit.
//
// This is the smallest thing that closes it: one command, no runner to adopt,
// output that pipes into the cosign every pipeline already has.
//
//	kiln attest --subject app@sha256:… --commit "$CI_COMMIT_SHA" \
//	  --repo acme/app --builder "https://gitlab.com/acme/app" > predicate.json
//	cosign attest --predicate predicate.json \
//	  --type https://slsa.dev/provenance/v1 app@sha256:…
//
// It writes the predicate BODY, not the statement: cosign builds the statement
// and derives the subject from the artifact itself, so a subject named here
// that disagreed with the artifact would be ignored rather than obeyed.
func runAttest(_ context.Context, args []string, io IO) error {
	fs := newFlagSet("attest", io)
	subject := fs.String("subject", "", "artifact reference as name@sha256:… (required)")
	commit := fs.String("commit", "", "source commit the artifact was built from (required)")
	repo := fs.String("repo", "", "source repository as owner/name (required)")
	ref := fs.String("ref", "", "git ref that triggered the build, e.g. refs/heads/main")
	event := fs.String("event", "push", "what triggered the build: push, tag or pull_request")
	kind := fs.String("kind", "image", "artifact kind: image or binaries")
	builder := fs.String("builder", "", "the platform that built it, e.g. https://gitlab.com/acme/app (required)")
	gateTool := fs.String("gate", "", "the source gate that passed this commit, e.g. warden")
	reproved := fs.Bool("gate-reproved", false, "the gate's checks ran during THIS build, rather than being inherited")
	gateReason := fs.String("gate-reason", "", "why the gate verdict stands, carried verbatim into the predicate")
	isolated := fs.Bool("isolated", false, "the build ran without the operator's credentials")
	invocation := fs.String("invocation", "", "an id for this build, so two builds of one commit are distinguishable")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	name, digest, err := splitSubject(*subject)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*commit) == "" {
		return failWith(ExitUsage, "kiln attest: --commit is required — provenance that names no commit is provenance about nothing")
	}
	if strings.TrimSpace(*repo) == "" {
		return failWith(ExitUsage, "kiln attest: --repo is required")
	}
	// Required, not defaulted. This command runs when something other than
	// kiln built the artifact, so falling back to kiln's own id would have
	// every foreign pipeline quietly signing a claim to be kiln — the exact
	// field RollOps and --policy pin their trust on.
	if strings.TrimSpace(*builder) == "" {
		return failWith(ExitUsage, "kiln attest: --builder is required — name the platform that built this, "+
			"because a verifier pins its trust on that name")
	}

	// A gate is claimed only when one is named. Recording verified: true with
	// no gate would assert a check nobody ran, which is the one thing this
	// predicate exists to make checkable.
	gate := strings.TrimSpace(*gateTool)

	stmt, err := attest.Build(ports.AttestInput{
		SubjectName:   name,
		SubjectDigest: digest,
		Repo:          strings.TrimSpace(*repo),
		SHA:           strings.TrimSpace(*commit),
		Ref:           *ref,
		Event:         *event,
		ArtifactKind:  *kind,
		BuilderID:     strings.TrimSpace(*builder),
		GateTool:      gate,
		GateVerified:  gate != "",
		GateReproved:  gate != "" && *reproved,
		GateReason:    *gateReason,
		Isolated:      *isolated,
		InvocationID:  *invocation,
		StartedOn:     time.Now().UTC(),
	})
	if err != nil {
		return err
	}

	body, err := stmt.PredicateJSON()
	if err != nil {
		return err
	}
	io.print(string(body) + "\n")
	return nil
}

// splitSubject parses name@sha256:hex, the only form that identifies an
// artifact by content. A tag is not accepted: it moves, and provenance about a
// moving target says nothing a week later.
func splitSubject(s string) (name, digest string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", failWith(ExitUsage, "kiln attest: --subject is required, as name@sha256:…")
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return "", "", failWith(ExitUsage, "kiln attest: --subject %q must be name@sha256:… — "+
			"a tag moves, so provenance about one says nothing later", s)
	}
	name, digest = s[:at], s[at+1:]
	if !strings.HasPrefix(digest, "sha256:") {
		return "", "", failWith(ExitUsage, "kiln attest: --subject digest %q must start with sha256:", digest)
	}
	return name, digest, nil
}
