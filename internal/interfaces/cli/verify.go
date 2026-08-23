package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/domain/policy"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/policyfile"
	"go.klarlabs.de/kiln/internal/infrastructure/verify"
)

// runVerify walks a published artifact's provenance chain.
//
// This is the command that makes kiln's claim worth making. Everything else
// produces provenance; this is what checks it, and it is deliberately usable
// by somebody who did not build the artifact and does not trust the machine
// that did — the only credential it needs is a public key.
//
// With --policy it is usable by somebody who does not run kiln at all: the
// rules live in a file in their repository, the builder may be anyone they
// name, and the source verdict is checked against the gate's own signature
// read off the artifact. Nothing about that requires having adopted kiln to
// build anything.
func runVerify(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("verify", io)
	policyPath := fs.String("policy", "", "verification policy file; the rules live in your repo, not in flags")
	dir := fs.String("dir", "", "local clone, so the warden note on the source commit can be read")
	key := fs.String("key", "", "cosign public key; omit for keyless, which needs --identity and --issuer")
	identity := fs.String("identity", "", "certificate identity to require, for keyless verification")
	issuer := fs.String("issuer", "", "certificate OIDC issuer to require, for keyless verification")
	trusted := fs.String("trusted-keys", "", "comma-separated warden signers the note must match")
	asJSON := fs.Bool("json", false, "emit the report as JSON, for a gate that has to act on it")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	reference := fs.Arg(0)
	if strings.TrimSpace(reference) == "" {
		return failWith(ExitUsage,
			"usage: kiln verify <image-ref> [--policy p.yaml | --key k | --identity i --issuer u]")
	}

	opts := verify.Options{
		Reference:   reference,
		RepoDir:     *dir,
		CosignKey:   *key,
		Identity:    *identity,
		Issuer:      *issuer,
		TrustedKeys: splitCommas(*trusted),
	}

	var checks []string
	if *policyPath != "" {
		// A policy exists so the check does not depend on what somebody typed
		// into a pipeline step. Silently letting a flag override it would undo
		// exactly that, so a conflict is a usage error rather than a
		// precedence rule nobody remembers.
		if *key != "" || *identity != "" || *issuer != "" || *trusted != "" {
			return failWith(ExitUsage,
				"--policy already says whose signature counts; remove the conflicting flags "+
					"(--key/--identity/--issuer/--trusted-keys) or drop --policy")
		}
		p, err := policyfile.Load(*policyPath)
		if err != nil {
			return wrapExit(ExitError, err)
		}
		applyPolicy(&opts, p)
		checks = p.Checks()
	}

	// A repository under the cursor is the obvious place to read the note
	// from, so default to it — but only if it is one, since verify must stay
	// usable from anywhere.
	runner := execx.NewSystem()
	if opts.RepoDir == "" {
		if deps, err := boot.Build(ctx, boot.Options{}); err == nil {
			opts.RepoDir = deps.Dir
			opts.WardenBin = deps.Env.Warden
			if len(opts.TrustedKeys) == 0 && *policyPath == "" {
				opts.TrustedKeys = deps.Env.TrustedKeys
			}
		}
	}

	report, err := verify.New(runner).Verify(ctx, opts)

	if *asJSON {
		return emitJSON(io, report, err)
	}

	if len(checks) > 0 {
		// What the run was asked to prove, before whether it did. A reader
		// scrolling CI output should not have to infer the policy from which
		// lines happen to appear.
		io.print("policy requires:\n")
		for _, c := range checks {
			io.print("  - " + c + "\n")
		}
		io.print("\n")
	}
	io.print(report.String())

	switch {
	case errors.Is(err, verify.ErrIncomplete):
		return failWith(ExitFailed, "provenance chain incomplete")
	case err != nil:
		return wrapExit(ExitError, err)
	}
	if !report.Complete() {
		// Everything checkable held, but not everything was checked. Exit zero
		// — the artifact is sound as far as anyone could tell — while saying
		// plainly that a link was skipped, so a script gating on this can
		// require --dir rather than assume it got a full answer.
		io.print("\nchain verified, with unchecked links above\n")
		return nil
	}
	io.print("\nchain verified end to end\n")
	return nil
}

// applyPolicy turns the policy into verifier options.
func applyPolicy(opts *verify.Options, p policy.Policy) {
	opts.CosignKey = p.Signature.Key
	opts.Identity = p.Signature.Identity
	opts.Issuer = p.Signature.Issuer
	opts.AllowedBuilders = p.Provenance.Builders
	opts.SourceKeys = p.Source.Keys
	opts.AllowedGates = p.Source.Gates
	opts.RequiredLevels = p.Source.Levels
	opts.SourceRequired = p.Source.Required
}

// jsonReport is the machine-readable shape. It is a separate type from
// verify.Report on purpose: this is a published contract with whatever script
// gates on it, and it must not change silently because an internal field was
// renamed.
type jsonReport struct {
	Reference string     `json:"reference"`
	Verified  bool       `json:"verified"`
	Complete  bool       `json:"complete"`
	Links     []jsonLink `json:"links"`
	Commit    string     `json:"sourceCommit,omitempty"`
	Builder   string     `json:"builder,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type jsonLink struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// emitJSON prints the report as JSON and preserves the exit code.
//
// The exit code is the part a CI job acts on; the JSON is for the job that
// wants to say *which* link broke. Emitting one without the other would make
// this either unscriptable or silently passing.
func emitJSON(io IO, report verify.Report, verifyErr error) error {
	out := jsonReport{
		Reference: report.Reference,
		Verified:  verifyErr == nil,
		Complete:  report.Complete(),
	}
	for _, l := range report.Links {
		out.Links = append(out.Links, jsonLink{l.Name, string(l.Status), l.Detail})
	}
	if report.Statement != nil {
		out.Commit = report.Statement.SourceCommit()
		out.Builder = report.Statement.Predicate.RunDetails.Builder.ID
	}
	if verifyErr != nil {
		out.Error = verifyErr.Error()
	}

	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return wrapExit(ExitError, err)
	}
	io.print(string(body) + "\n")

	// The message goes to stderr, so stdout stays parseable JSON and the exit
	// code still means what it means everywhere else in this CLI.
	switch {
	case errors.Is(verifyErr, verify.ErrIncomplete):
		return failWith(ExitFailed, "provenance chain incomplete")
	case verifyErr != nil:
		return wrapExit(ExitError, verifyErr)
	}
	return nil
}

func splitCommas(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := make([]string, 0, 2)
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
