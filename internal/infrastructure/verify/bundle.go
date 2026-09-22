package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.klarlabs.de/kiln/internal/infrastructure/attest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// Bundle file names. A directory of these is enough to walk the chain
// without a registry or a clone.
const (
	BundleStatement = "statement.json"
	BundleSource    = "source.json"
	BundleSignature = "signature.bundle"
)

func (opts Options) offline() bool {
	return opts.BundleDir != "" || opts.StatementPath != ""
}

func (v *Verifier) verifyOffline(ctx context.Context, opts Options) (Report, error) {
	report := Report{SourceRequired: opts.SourceRequired, Offline: true}

	stmtPath, err := resolveStatementPath(opts)
	if err != nil {
		report.Links = append(report.Links, Link{"provenance", Fail, err.Error()})
		return report, ErrIncomplete
	}

	raw, err := os.ReadFile(stmtPath) //nolint:gosec // operator-supplied bundle path
	if err != nil {
		report.Links = append(report.Links, Link{"provenance", Fail, "cannot read " + stmtPath + ": " + err.Error()})
		return report, ErrIncomplete
	}
	stmt, err := attest.Parse(raw)
	if err != nil {
		report.Links = append(report.Links, Link{"provenance", Fail, err.Error()})
		return report, ErrIncomplete
	}

	report.Statement = &stmt
	report.Reference = offlineReference(opts, stmt)

	report.Links = append(report.Links, v.checkLocalSignature(ctx, opts, stmtPath))
	report.Links = append(report.Links, Link{
		"provenance", Pass,
		fmt.Sprintf("local statement built from %s on %s",
			short(stmt.SourceCommit()),
			orNone(stmt.Predicate.BuildDefinition.ExternalParameters.Ref)),
	})
	report.Links = append(report.Links, checkBuilder(stmt, opts.AllowedBuilders))
	report.Links = append(report.Links, v.checkOfflineSource(ctx, opts, stmt))

	if !report.OK() {
		return report, ErrIncomplete
	}
	return report, nil
}

func resolveStatementPath(opts Options) (string, error) {
	if opts.StatementPath != "" {
		return opts.StatementPath, nil
	}
	if opts.BundleDir == "" {
		return "", fmt.Errorf("no statement.json in the bundle")
	}
	path := filepath.Join(opts.BundleDir, BundleStatement)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("bundle %s has no %s", opts.BundleDir, BundleStatement)
	}
	return path, nil
}

func offlineReference(opts Options, stmt attest.Statement) string {
	if opts.Reference != "" {
		return opts.Reference
	}
	if len(stmt.Subject) == 0 {
		return "local statement"
	}
	name := stmt.Subject[0].Name
	if hex := stmt.Subject[0].Digest["sha256"]; hex != "" {
		return name + "@sha256:" + hex
	}
	return name
}

func (v *Verifier) checkLocalSignature(ctx context.Context, opts Options, statementPath string) Link {
	if opts.BundleDir == "" {
		return Link{"signature", Unknown, "offline: no local cosign bundle; not re-checked against a registry"}
	}
	bundle := filepath.Join(opts.BundleDir, BundleSignature)
	if _, err := os.Stat(bundle); err != nil {
		return Link{"signature", Unknown, "offline: no local cosign bundle; not re-checked against a registry"}
	}
	if _, err := v.Runner.LookPath(v.Cosign); err != nil {
		return Link{"signature", Unknown, "offline: local bundle present but cosign is not installed"}
	}
	args, err := verifyArgs("verify-blob-attestation", opts)
	if err != nil {
		return Link{"signature", Unknown, "offline: local bundle present; " + err.Error()}
	}
	args = append(args, "--bundle", bundle, "--type", attest.CosignType, statementPath)
	if _, err := v.Runner.Run(ctx, execx.Cmd{Name: v.Cosign, Args: args, Dir: opts.RepoDir}); err != nil {
		return Link{"signature", Fail, condense(err)}
	}
	return Link{"signature", Pass, "local cosign bundle accepted"}
}

func (v *Verifier) checkOfflineSource(ctx context.Context, opts Options, stmt attest.Statement) Link {
	if opts.BundleDir == "" {
		return v.checkSource(ctx, opts, stmt)
	}
	path := filepath.Join(opts.BundleDir, BundleSource)
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied bundle path
	if err != nil {
		return v.checkSource(ctx, opts, stmt)
	}

	commit := stmt.SourceCommit()
	if commit == "" {
		return Link{"source gate", Fail, "the provenance names no commit"}
	}
	if len(opts.SourceKeys) == 0 {
		return Link{"source gate", Unknown,
			"offline source file present; pass --policy so the gate's signature can be checked"}
	}

	envelope, summary, err := attest.ParseEnvelope(data)
	if err != nil {
		return Link{"source gate", Fail, "offline source file is not a signed gate envelope: " + err.Error()}
	}
	keyID, ok := envelope.VerifiedBy(opts.SourceKeys)
	if !ok {
		return Link{"source gate", Fail, "the source summary is not signed by any pinned gate key"}
	}
	if detail, ok := acceptSummary(summary, keyID, commit, opts); ok {
		return Link{"source gate", Pass, detail}
	} else {
		return Link{"source gate", Fail, detail}
	}
}
