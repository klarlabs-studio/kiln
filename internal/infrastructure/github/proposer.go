package github

import (
	"context"

	"go.klarlabs.de/kiln/internal/application/ports"
)

// NewProposer adapts the client to ports.PullProposer, or returns nil when
// there is no usable token.
//
// Returning a plain nil rather than a proposer that fails on use is
// deliberate, and so is returning it as the interface type from a function
// instead of assigning a typed nil: a typed nil inside an interface satisfies
// `!= nil` and then panics on the first call, which is the classic version of
// this bug.
func NewProposer(c *Client) ports.PullProposer {
	if c == nil || !c.Enabled() {
		return nil
	}
	return proposer{client: c}
}

// proposer is narrow on purpose. Proposing needs two calls, not the client's
// ten, and a test can supply four lines instead of an HTTP server.
type proposer struct{ client *Client }

func (p proposer) OpenPullRequest(ctx context.Context, head, base, title, body string) (int, bool, error) {
	pull, opened, err := p.client.OpenPullRequest(ctx, head, base, title, body)
	return pull.Number, opened, err
}

func (p proposer) LabelPull(ctx context.Context, number int, labels []string) error {
	return p.client.LabelPull(ctx, number, labels)
}
