package gitea

import (
	"context"

	"go.klarlabs.de/kiln/internal/application/ports"
)

// NewProposer adapts the client to ports.PullProposer, or returns nil when
// there is no usable token. A typed nil inside the interface is avoided for
// the same reason as the GitHub proposer.
func NewProposer(c *Client) ports.PullProposer {
	if c == nil || !c.Enabled() {
		return nil
	}
	return proposer{client: c}
}

type proposer struct{ client *Client }

func (p proposer) OpenPullRequest(ctx context.Context, head, base, title, body string) (int, bool, error) {
	pull, opened, err := p.client.OpenPullRequest(ctx, head, base, title, body)
	return pull.Number, opened, err
}

func (p proposer) LabelPull(ctx context.Context, number int, labels []string) error {
	return p.client.LabelPull(ctx, number, labels)
}
