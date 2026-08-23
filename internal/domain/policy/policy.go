// Package policy loads a verification policy — what must be true about an
// artifact before a consumer accepts it.
//
// This is the half of kiln that does not require adopting kiln. Producing
// provenance means changing how you build; checking it does not, and an
// operator who runs `kiln verify --policy` in a GitHub Actions job against an
// image somebody else built has adopted nothing. The policy file is what makes
// that check reviewable: the rules live in the repository, in a diff, rather
// than in whatever flags somebody typed into a pipeline step.
//
// Every field fails closed. An empty policy verifies nothing and says so
// rather than passing; a rule that cannot be understood is a load error rather
// than a rule quietly not applied.
package policy

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// The accepted API version and kind, spelled exactly.
const (
	APIVersion = "kiln.klarlabs.de/v1"
	Kind       = "VerificationPolicy"
)

// Policy is a verification policy as it appears in the file.
type Policy struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Signature  Signature  `yaml:"signature"`
	Provenance Provenance `yaml:"provenance"`
	Source     Source     `yaml:"source"`
}

// Signature says who is allowed to have signed the artifact itself.
type Signature struct {
	// Key is a cosign public key path, for keyed verification.
	Key string `yaml:"key,omitempty"`
	// Identity and Issuer pin the certificate for keyless verification. Both
	// are required together: cosign refuses keyless without an identity, and
	// so does this — "signed by somebody" is not a security property.
	Identity string `yaml:"identity,omitempty"`
	Issuer   string `yaml:"issuer,omitempty"`
}

// Provenance says who is allowed to have built the artifact.
type Provenance struct {
	// Builders are acceptable SLSA builder IDs. A trailing @ matches any
	// version, which is how kiln publishes its own ID.
	//
	// This is what lets the policy accept artifacts kiln did not build: a
	// GitHub Actions workflow identity is as valid a builder here as kiln's
	// own, and a consumer verifying both kinds should not need two tools.
	Builders []string `yaml:"builders,omitempty"`
}

// Source says whose verdict about the *commit* counts.
//
// Separate from Provenance because it is a separate authority. The build
// platform says what it built; the source gate says whether the commit was
// allowed to be built. A policy that only pins the builder is trusting the
// builder's account of the gate.
type Source struct {
	// Keys are the gate's own public keys, base64 ed25519 — the form
	// `warden key show` prints. The carried summary is verified against these
	// directly, so checking the source verdict needs neither a clone of the
	// repository nor the gate's binary on the verifier's machine.
	Keys []string `yaml:"keys,omitempty"`
	// Gates are the verifier IDs whose summary is acceptable.
	Gates []string `yaml:"gates,omitempty"`
	// Levels the summary must claim, e.g. WARDEN_SOURCE_SIGNED.
	Levels []string `yaml:"levels,omitempty"`
	// Required makes a missing or uncheckable source verdict a failure rather
	// than a caveat. Off by default: an artifact from a build platform with no
	// source gate at all is a normal thing to verify, and reporting it as
	// broken would train people to ignore the tool.
	Required bool `yaml:"required,omitempty"`
}

// Parse reads and validates a policy.
func Parse(r io.Reader) (Policy, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Policy{}, fmt.Errorf("policy: read: %w", err)
	}

	var p Policy
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	// Unknown fields are an error. A policy is security configuration, and a
	// misspelled `builders:` that silently applied nothing is the worst
	// possible outcome — it reports success having checked less than the
	// author believed.
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return Policy{}, errors.New("policy: the file is empty")
		}
		return Policy{}, fmt.Errorf("policy: parse: %w", err)
	}

	if err := p.validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func (p Policy) validate() error {
	if p.APIVersion != APIVersion {
		return fmt.Errorf("policy: apiVersion %q, want %q", p.APIVersion, APIVersion)
	}
	if p.Kind != Kind {
		return fmt.Errorf("policy: kind %q, want %q", p.Kind, Kind)
	}

	hasKey := strings.TrimSpace(p.Signature.Key) != ""
	hasIdentity := strings.TrimSpace(p.Signature.Identity) != ""
	hasIssuer := strings.TrimSpace(p.Signature.Issuer) != ""

	switch {
	case hasKey && (hasIdentity || hasIssuer):
		return errors.New("policy: signature.key and signature.identity are alternatives, not a pair — " +
			"a policy naming both does not say which one has to hold")
	case hasKey:
	case hasIdentity && hasIssuer:
	case hasIdentity || hasIssuer:
		return errors.New("policy: keyless verification needs signature.identity and signature.issuer together — " +
			"an identity from any issuer can be minted by anyone who runs an OIDC provider")
	default:
		return errors.New("policy: signature needs a key, or an identity and issuer — " +
			"there is no way to check a signature without saying whose it must be")
	}

	if p.Source.Required && len(p.Source.Keys) == 0 {
		return errors.New("policy: source.required needs source.keys — " +
			"requiring a verdict nobody can check would fail every artifact")
	}
	for _, k := range p.Source.Keys {
		if strings.TrimSpace(k) == "" {
			return errors.New("policy: source.keys has an empty entry")
		}
	}
	for _, b := range p.Provenance.Builders {
		if strings.TrimSpace(b) == "" {
			return errors.New("policy: provenance.builders has an empty entry")
		}
	}
	return nil
}

// Checks lists, in the policy's own words, what it will establish. It is
// printed before a verification so an operator reading CI output can see what
// the run was actually asked to prove — not just whether it passed.
func (p Policy) Checks() []string {
	var out []string
	if p.Signature.Key != "" {
		out = append(out, "signature by the pinned key "+p.Signature.Key)
	} else {
		out = append(out, fmt.Sprintf("signature by %s via %s", p.Signature.Identity, p.Signature.Issuer))
	}
	if len(p.Provenance.Builders) > 0 {
		out = append(out, "built by "+strings.Join(p.Provenance.Builders, " or "))
	}
	if len(p.Source.Keys) > 0 {
		requirement := "source verdict signed by a pinned gate key"
		if !p.Source.Required {
			requirement += " (advisory)"
		}
		out = append(out, requirement)
	}
	if len(p.Source.Gates) > 0 {
		out = append(out, "gated by "+strings.Join(p.Source.Gates, " or "))
	}
	if len(p.Source.Levels) > 0 {
		out = append(out, "reaching "+strings.Join(p.Source.Levels, ", "))
	}
	return out
}
