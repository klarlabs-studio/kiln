// Package verify checks a published artifact's whole provenance chain.
//
// Kiln's value is a claim: "this artifact was built from that commit, and that
// commit passed its gate." Producing the claim is only half the job — until
// something can check it, an operator holding an image reference has to take
// the build box's word for it.
//
// This walks the chain in the order a sceptic would:
//
//	signature   somebody vouched for these exact bytes
//	provenance  an attestation says where they came from
//	builder     that attestation was made by kiln, not by anyone
//	source      the commit it names carries a trustworthy warden note
//
// Each link is reported separately, and a break in one does not hide the
// others. "The signature is fine but the source gate is missing" is a far more
// useful answer than "invalid".
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.klarlabs.de/kiln/internal/attest"
	"go.klarlabs.de/kiln/internal/execx"
)

// Status is one link's outcome.
type Status string

const (
	// Pass means the link was checked and held.
	Pass Status = "ok"
	// Fail means the link was checked and broke. Any Fail makes the whole
	// verification fail.
	Fail Status = "FAIL"
	// Unknown means the link could not be checked — no key pinned, no local
	// clone to read a note from. Not a pass: a verifier that reports "fine"
	// when it looked at nothing is worse than useless.
	Unknown Status = "unknown"
)

// Link is one step in the chain.
type Link struct {
	Name   string
	Status Status
	Detail string
}

// Report is the whole walk.
type Report struct {
	// Reference is the artifact that was checked.
	Reference string
	Links     []Link
	// Statement is the parsed provenance, when one was found.
	Statement *attest.Statement
	// SourceRequired records that the policy demanded a source verdict, so
	// OK() can treat an unestablished one as a break rather than a caveat.
	SourceRequired bool
}

// essentialLinks are the ones that must be positively established, not merely
// un-broken. They are the artifact's own claims: who signed it, what the
// provenance says, and whether kiln wrote that provenance. All three are
// checkable from the artifact alone.
var essentialLinks = []string{"signature", "provenance", "builder"}

// OK reports whether the artifact's own chain held.
//
// A Fail anywhere breaks it. An Unknown breaks it too — but only for the
// essential links, because "I could not check the signature" must never read
// the same as "the signature is good". A source gate left Unknown is a caveat
// rather than a break: the artifact really is signed and really does carry
// kiln provenance, and what went unchecked is the commit's gate, which needs a
// local clone the verifier may legitimately not have.
func (r Report) OK() bool {
	for _, l := range r.Links {
		if l.Status == Fail {
			return false
		}
		if l.Status == Unknown && slices.Contains(r.essential(), l.Name) {
			return false
		}
	}
	return true
}

// essential is the link set this report had to establish. A policy with
// source.required promotes the source gate into it, which is the difference
// between "we would like the commit to have been gated" and "we do not deploy
// commits that were not".
func (r Report) essential() []string {
	if r.SourceRequired {
		return append(slices.Clone(essentialLinks), "source gate")
	}
	return essentialLinks
}

// Complete reports whether the chain was fully established — no link left
// Unknown. A caller enforcing provenance should require this, not just OK.
func (r Report) Complete() bool {
	for _, l := range r.Links {
		if l.Status != Pass {
			return false
		}
	}
	return true
}

// String renders the report the way the CLI prints it.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", r.Reference)
	for _, l := range r.Links {
		fmt.Fprintf(&b, "  %-8s %-12s %s\n", l.Status, l.Name, l.Detail)
	}
	return b.String()
}

// Options configure a walk.
type Options struct {
	// Reference is the artifact: an image ref, ideally by digest.
	Reference string
	// RepoDir is a local clone, used to check the warden note on the commit
	// the provenance names. Without it the source link stays Unknown.
	RepoDir string
	// CosignKey is a public key for keyed verification. Empty selects keyless,
	// which additionally needs Identity and Issuer.
	CosignKey string
	// Identity and Issuer pin who is allowed to have signed, for keyless
	// verification. cosign refuses keyless verification without them, and so
	// it should: a signature by *anyone* proves nothing.
	Identity string
	Issuer   string
	// TrustedKeys are the warden signers whose note counts as a gate.
	TrustedKeys []string
	// WardenBin is the gate binary.
	WardenBin string

	// AllowedBuilders are the SLSA builder IDs this verification accepts. A
	// trailing @ matches any version.
	//
	// Empty means "kiln only", which is the right default for `kiln verify`
	// with no policy and the wrong one for a consumer checking somebody else's
	// artifact — hence the field. Verifying a GitHub Actions build with the
	// same command and the same report is what makes this tool adoptable by
	// people who will never run kiln.
	AllowedBuilders []string

	// SourceKeys are the gate's own public keys, base64 ed25519.
	//
	// With these the source verdict is checked against the signature the gate
	// made, read off the artifact itself — no clone, no warden binary, no
	// trust in the builder that carried it. Without them the walk falls back
	// to reading the note out of a local clone, which is strictly weaker and
	// needs a machine that has one.
	SourceKeys []string
	// AllowedGates are verifier IDs whose summary is acceptable. Empty accepts
	// any, and says so rather than implying a check happened.
	AllowedGates []string
	// RequiredLevels the summary must claim, e.g. WARDEN_SOURCE_SIGNED.
	RequiredLevels []string
	// SourceRequired turns an unestablished source verdict from a caveat into
	// a failure.
	SourceRequired bool
}

// Verifier walks the chain.
type Verifier struct {
	Runner execx.Runner
	Cosign string
}

// New builds a verifier.
func New(r execx.Runner) *Verifier { return &Verifier{Runner: r, Cosign: "cosign"} }

// ErrIncomplete reports a chain with a broken link.
var ErrIncomplete = errors.New("provenance chain incomplete")

// Verify walks every link and returns the report. The error is non-nil when a
// link failed; the report is populated either way, because a caller needs to
// see which link broke.
func (v *Verifier) Verify(ctx context.Context, opts Options) (Report, error) {
	report := Report{Reference: opts.Reference, SourceRequired: opts.SourceRequired}

	if err := checkReferenceShape(opts.Reference); err != nil {
		report.Links = append(report.Links, Link{"reference", Fail, err.Error()})
		return report, ErrIncomplete
	}

	if _, err := v.Runner.LookPath(v.Cosign); err != nil {
		report.Links = append(report.Links, Link{
			"signature", Unknown, "cosign is not installed, so nothing could be checked",
		})
		return report, fmt.Errorf("%w: cosign is required to verify anything", ErrIncomplete)
	}

	report.Links = append(report.Links, v.checkSignature(ctx, opts))

	stmt, link := v.checkProvenance(ctx, opts)
	report.Links = append(report.Links, link)
	if stmt != nil {
		report.Statement = stmt
		report.Links = append(report.Links, checkBuilder(*stmt, opts.AllowedBuilders))
		report.Links = append(report.Links, v.checkSource(ctx, opts, *stmt))
	}

	if !report.OK() {
		return report, ErrIncomplete
	}
	return report, nil
}

// checkReferenceShape rejects what this walk cannot check.
//
// A binary release is verified against downloaded files rather than a
// registry, so pointing this at a release tag would produce a cosign error
// about a missing image — technically true and completely unhelpful. Saying
// what to run instead costs three lines and saves an afternoon.
func checkReferenceShape(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return errors.New("no artifact reference given")
	}
	if strings.Contains(ref, "@sha256:") || strings.Contains(ref, "/") {
		return nil
	}
	return fmt.Errorf(
		"%q looks like a release tag, not an image. A binary release is verified against its "+
			"downloaded files:\n"+
			"           cosign verify-blob-attestation --key <key> \\\n"+
			"             --type %s \\\n"+
			"             --bundle %s checksums.txt",
		ref, attest.CosignType, "provenance.intoto.jsonl")
}

// checkSignature asks cosign whether anyone vouched for these bytes.
func (v *Verifier) checkSignature(ctx context.Context, opts Options) Link {
	args, err := verifyArgs("verify", opts)
	if err != nil {
		return Link{"signature", Unknown, err.Error()}
	}
	args = append(args, opts.Reference)

	if _, err := v.Runner.Run(ctx, execx.Cmd{Name: v.Cosign, Args: args, Dir: opts.RepoDir}); err != nil {
		return Link{"signature", Fail, condense(err)}
	}
	return Link{"signature", Pass, "cosign accepted " + describeTrust(opts)}
}

// checkProvenance fetches the attestation and parses the predicate out of it.
func (v *Verifier) checkProvenance(ctx context.Context, opts Options) (*attest.Statement, Link) {
	args, err := verifyArgs("verify-attestation", opts)
	if err != nil {
		return nil, Link{"provenance", Unknown, err.Error()}
	}
	args = append(args, "--type", attest.CosignType, opts.Reference)

	res, err := v.Runner.Run(ctx, execx.Cmd{Name: v.Cosign, Args: args, Dir: opts.RepoDir})
	if err != nil {
		return nil, Link{"provenance", Fail, condense(err)}
	}

	stmt, err := statementFromAttestations(res.Stdout)
	if err != nil {
		return nil, Link{"provenance", Fail, err.Error()}
	}
	return &stmt, Link{
		"provenance", Pass,
		fmt.Sprintf("built from %s on %s", short(stmt.SourceCommit()),
			orNone(stmt.Predicate.BuildDefinition.ExternalParameters.Ref)),
	}
}

// checkBuilder confirms kiln made the claim.
//
// This link is not ceremony. cosign verified that a *trusted key* signed an
// attestation; it did not check what the attestation says. Anyone with that
// key can attest anything, so a reader about to trust kiln-specific fields —
// the source gate in particular — must first confirm kiln wrote them.
func checkBuilder(stmt attest.Statement, allowed []string) Link {
	id := stmt.Predicate.RunDetails.Builder.ID

	if len(allowed) == 0 {
		// No policy: this is `kiln verify` on what is presumed to be kiln's
		// own output, and the kiln-specific fields below are only meaningful
		// if kiln wrote them.
		if !stmt.BuiltByKiln() {
			return Link{"builder", Fail, fmt.Sprintf(
				"attested by %q, which is not kiln — its sourceGate claim means nothing here. "+
					"Name it in provenance.builders to accept it", id)}
		}
		return Link{"builder", Pass, id}
	}

	if !builderAllowed(id, allowed) {
		return Link{"builder", Fail, fmt.Sprintf(
			"built by %s, which the policy does not allow", orNone(id))}
	}
	return Link{"builder", Pass, id}
}

// builderAllowed matches a builder ID against the policy's roster.
//
// A trailing @ is a version wildcard: "…/kiln@" accepts "…/kiln@v0.1.0". It is
// a prefix match and nothing cleverer on purpose — a regex here would be a
// place for a policy author to write something that accidentally matches a
// builder they have never heard of.
func builderAllowed(id string, allowed []string) bool {
	for _, want := range allowed {
		switch {
		case strings.HasSuffix(want, "@"):
			if strings.HasPrefix(id, want) {
				return true
			}
		case id == want:
			return true
		}
	}
	return false
}

// checkSource walks back to Warden's note on the commit the provenance names.
//
// This is the link that makes the chain a chain. Everything above proves the
// artifact came from a commit; only this says the commit was ever gated.
func (v *Verifier) checkSource(ctx context.Context, opts Options, stmt attest.Statement) Link {
	commit := stmt.SourceCommit()
	if commit == "" {
		return Link{"source gate", Fail, "the provenance names no commit"}
	}

	// The gate's own signed summary, if the policy says whose signature counts.
	// This is the strongest form of the check and the only one that works from
	// anywhere: the verdict travels on the artifact, signed by the gate, so
	// neither a clone nor the gate's binary has to exist on this machine.
	if len(opts.SourceKeys) > 0 {
		return v.checkCarriedSummary(ctx, opts, commit)
	}

	gate := stmt.Predicate.BuildDefinition.InternalParameters.SourceGate
	if opts.RepoDir == "" {
		// The predicate's own claim is worth reporting, but it is kiln
		// vouching for itself. Only the note settles it.
		return Link{"source gate", Unknown, fmt.Sprintf(
			"provenance claims %s (reproved=%v); pass --dir <clone> to check the note itself",
			gate.Tool, gate.Reproved)}
	}

	bin := opts.WardenBin
	if bin == "" {
		bin = "warden"
	}
	if _, err := v.Runner.LookPath(bin); err != nil {
		return Link{"source gate", Unknown, bin + " is not installed, so the note could not be read"}
	}

	args := []string{"verify", "--commit", commit, "--quiet"}
	if len(opts.TrustedKeys) > 0 {
		args = append(args, "--require-signed", "--key", strings.Join(opts.TrustedKeys, ","))
	}
	if _, err := v.Runner.Run(ctx, execx.Cmd{Name: bin, Args: args, Dir: opts.RepoDir}); err != nil {
		return Link{"source gate", Fail, fmt.Sprintf("no trustworthy warden note on %s", short(commit))}
	}

	detail := fmt.Sprintf("warden note on %s", short(commit))
	if len(opts.TrustedKeys) == 0 {
		// Without pinned keys the note was only checked for existence and
		// integrity, not for who signed it. Saying "ok" without that caveat
		// would overstate what was established.
		detail += " (unpinned: signer not checked — pass --key)"
	}
	if !gate.Reproved {
		detail += "; checks were inherited, not re-run"
	}
	return Link{"source gate", Pass, detail}
}

// checkCarriedSummary verifies the source verdict the artifact carries.
//
// cosign downloads it; cosign does not judge it. `verify-attestation` would
// check the signature of whoever *attached* the envelope — the build platform
// — and that is precisely the signature that does not matter. The claim is the
// gate's, so it stands or falls on the gate's key.
//
// Every configured key is tried rather than the one the envelope names. A DSSE
// keyid is attacker-controlled metadata: fine for selecting a key from a
// roster, worthless as an authorisation.
func (v *Verifier) checkCarriedSummary(ctx context.Context, opts Options, builtFrom string) Link {
	res, err := v.Runner.Run(ctx, execx.Cmd{
		Name: v.Cosign,
		Args: []string{"download", "attestation", opts.Reference},
		Dir:  opts.RepoDir,
	})
	if err != nil {
		return Link{"source gate", Fail, "no source summary on this artifact: " + condense(err)}
	}

	var lastDetail string
	for line := range strings.SplitSeq(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		envelope, summary, err := attest.ParseEnvelope([]byte(line))
		if err != nil {
			// Some other attestation on the same artifact — the build
			// provenance, an SBOM. Not a failure; just not this.
			continue
		}
		keyID, ok := envelope.VerifiedBy(opts.SourceKeys)
		if !ok {
			lastDetail = "the source summary is not signed by any pinned gate key"
			continue
		}
		if detail, ok := acceptSummary(summary, keyID, builtFrom, opts); ok {
			return Link{"source gate", Pass, detail}
		} else {
			lastDetail = detail
		}
	}

	if lastDetail == "" {
		lastDetail = "this artifact carries no source summary"
	}
	return Link{"source gate", Fail, lastDetail}
}

// acceptSummary applies the policy to an authenticated summary.
func acceptSummary(s attest.VSAStatement, keyID, builtFrom string, opts Options) (string, bool) {
	verifier := s.Predicate.Verifier.ID
	if len(opts.AllowedGates) > 0 && !slices.Contains(opts.AllowedGates, verifier) {
		return fmt.Sprintf("gated by %s, which the policy does not allow", orNone(verifier)), false
	}
	if !s.Passed() {
		return fmt.Sprintf("the source gate reported %q", orNone(s.Predicate.VerificationResult)), false
	}
	for _, want := range opts.RequiredLevels {
		if !hasLevel(s.Predicate.VerifiedLevels, want) {
			return fmt.Sprintf("the source gate did not reach %s (got %v)",
				want, s.Predicate.VerifiedLevels), false
		}
	}

	// The join. Two verified claims about two different commits are not a
	// chain: without this, a summary for a well-gated commit could ride on an
	// artifact built from an ungated one, and both signatures would check out.
	commit := s.SourceCommit()
	if commit == "" {
		return "the source summary names no commit", false
	}
	if commit != builtFrom {
		return fmt.Sprintf("the source summary is for %s but the artifact was built from %s",
			short(commit), short(builtFrom)), false
	}

	detail := fmt.Sprintf("%s at %s, signed by %s", orNone(verifier), short(commit), orNone(keyID))
	if len(opts.AllowedGates) == 0 {
		detail += " (no allowed-gate policy set)"
	}
	return detail, true
}

func hasLevel(levels []string, want string) bool {
	for _, l := range levels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

// verifyArgs builds the identity flags shared by both cosign verifications.
func verifyArgs(sub string, opts Options) ([]string, error) {
	args := []string{sub}
	switch {
	case opts.CosignKey != "":
		return append(args, "--key", opts.CosignKey), nil
	case opts.Identity != "" && opts.Issuer != "":
		return append(args,
			"--certificate-identity", opts.Identity,
			"--certificate-oidc-issuer", opts.Issuer), nil
	default:
		// cosign refuses keyless verification without an identity, and so it
		// should: "signed by somebody" is not a security property.
		return nil, errors.New(
			"no --key, and keyless needs --identity with --issuer — a signature by anyone proves nothing")
	}
}

func describeTrust(opts Options) string {
	if opts.CosignKey != "" {
		return "the pinned key"
	}
	return opts.Identity + " via " + opts.Issuer
}

// statementFromAttestations reads cosign's output and picks the usable
// provenance.
//
// cosign emits one JSON object per line, each a DSSE envelope whose payload is
// the base64 statement. A digest routinely carries several — an SBOM, a
// rebuild's second provenance, one left by an older tool version — and taking
// the first SLSA-typed one is not enough: a stale or malformed entry would
// mask the good one sitting behind it. cosign has already established every
// envelope was signed by a trusted key, so preferring one that actually names
// a builder and a commit costs nothing and is what a reader wants.
func statementFromAttestations(out string) (attest.Statement, error) {
	var lastErr error
	var fallback *attest.Statement
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var env struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			lastErr = err
			continue
		}
		payload, err := decodePayload(env.Payload)
		if err != nil {
			lastErr = err
			continue
		}
		stmt, err := attest.Parse(payload)
		if err != nil {
			lastErr = err
			continue
		}
		if stmt.BuiltByKiln() && stmt.SourceCommit() != "" {
			return stmt, nil
		}
		if fallback == nil {
			// Keep it: if nothing better turns up, reporting what is actually
			// there beats reporting nothing at all.
			kept := stmt
			fallback = &kept
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	if lastErr != nil {
		return attest.Statement{}, fmt.Errorf("no readable slsa provenance in the attestation: %w", lastErr)
	}
	return attest.Statement{}, errors.New("cosign returned no attestation")
}

func short(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

func orNone(s string) string {
	if s == "" {
		return "an unrecorded ref"
	}
	return s
}

// condense trims a cosign failure to its last meaningful line. cosign is
// verbose on failure and the actionable part is at the end.
func condense(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i > 0 && i < len(msg)-2 {
		return msg[i+2:]
	}
	return msg
}
