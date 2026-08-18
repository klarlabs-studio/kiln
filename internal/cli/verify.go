package cli

import (
	"context"
	"errors"
	"strings"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/verify"
)

// runVerify walks a published artifact's provenance chain.
//
// This is the command that makes kiln's claim worth making. Everything else
// produces provenance; this is what checks it, and it is deliberately usable
// by somebody who did not build the artifact and does not trust the machine
// that did — the only credential it needs is a public key.
func runVerify(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("verify", io)
	dir := fs.String("dir", "", "local clone, so the warden note on the source commit can be read")
	key := fs.String("key", "", "cosign public key; omit for keyless, which needs --identity and --issuer")
	identity := fs.String("identity", "", "certificate identity to require, for keyless verification")
	issuer := fs.String("issuer", "", "certificate OIDC issuer to require, for keyless verification")
	trusted := fs.String("trusted-keys", "", "comma-separated warden signers the note must match")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	reference := fs.Arg(0)
	if strings.TrimSpace(reference) == "" {
		return failWith(ExitUsage, "usage: kiln verify <image-ref> [--key k | --identity i --issuer u]")
	}

	opts := verify.Options{
		Reference:   reference,
		RepoDir:     *dir,
		CosignKey:   *key,
		Identity:    *identity,
		Issuer:      *issuer,
		TrustedKeys: splitCommas(*trusted),
	}
	// A repository under the cursor is the obvious place to read the note
	// from, so default to it — but only if it is one, since verify must stay
	// usable from anywhere.
	runner := execx.NewSystem()
	if opts.RepoDir == "" {
		if deps, err := boot.Build(ctx, boot.Options{}); err == nil {
			opts.RepoDir = deps.Dir
			opts.WardenBin = deps.Env.Warden
			if len(opts.TrustedKeys) == 0 {
				opts.TrustedKeys = deps.Env.TrustedKeys
			}
		}
	}

	report, err := verify.New(runner).Verify(ctx, opts)
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
