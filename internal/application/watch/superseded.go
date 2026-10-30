package watch

import (
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/release"
	"go.klarlabs.de/kiln/internal/domain/run"
)

// dropSuperseded drops the version tags a newer release on the same major
// line has overtaken.
//
// The baseline covers what a box inherited, but only on a box that starts
// with an empty ledger, and only the tags it saw on that first tick. Neither
// helps a box that is already running: a tag that fails is never silenced by
// a success, so the backoff merely spaces its retries out, forever. One box
// here retried v0.0.0, v0.1.0 and v0.2.0 about seventy times each, long after
// newer releases had shipped — a release nobody will deploy, re-gated hourly.
//
// Judged per major line, not across the repository: v1.9.5 cut after v2.0.0
// is usually a security backport, and skipping that silently would be the
// worse failure. See release.Lines.
//
// all is every job discovery found, before the baseline dropped any: a line's
// newest release is the newest the repository has, not the newest this box
// has yet to build. What shipped is a successful run in the ledger or a tag
// recorded in the baseline — an inherited tag shipped too, just not from this
// box.
//
// Tags that are not versions are left alone. There is no order to overtake
// them by, so they keep the behaviour they always had.
func (w *Watcher) dropSuperseded(jobs, all []Job, base *Baseline) []Job {
	if w.Pipeline.BuildSupersededTags() {
		return jobs
	}
	var lines release.Lines
	for _, j := range all {
		if v, ok := tagVersion(j); ok {
			lines.Tagged(v)
		}
	}
	w.recordShipped(&lines, base)

	kept := jobs[:0:0]
	var dropped []string
	for _, j := range jobs {
		if v, isVersion := tagVersion(j); isVersion && lines.Superseded(v) {
			dropped = append(dropped, v.String())
			continue
		}
		kept = append(kept, j)
	}
	// Said out loud, as for closed pull requests: "why is my tag not being
	// built" is the question this line answers.
	if len(dropped) > 0 {
		w.logger().Info("skipped superseded tags",
			"count", len(dropped), "first", dropped[0],
			"why", "a newer release on the same major line has overtaken them; "+
				"set watch.superseded_tags to build them anyway")
	}
	return kept
}

// recordShipped feeds every release this repository is known to have
// published into lines: each tag with a successful run in the ledger, and
// each tag the box inherited.
//
// The ledger rather than engine.AlreadyBuilt, because the question is not
// whether this commit built on this ref but whether the release ever shipped.
// A tag moved since its success still shipped, and a tag since deleted still
// tells us how far its line had got.
func (w *Watcher) recordShipped(lines *release.Lines, base *Baseline) {
	consider := func(ref string) {
		if v, ok := release.ParseTag(ref); ok {
			lines.Shipped(v)
		}
	}
	if base != nil {
		for ref := range base.Tags {
			consider(ref)
		}
	}
	if w.Store == nil {
		return
	}
	runs, err := w.Store.List()
	if err != nil {
		// Not fatal, and not a reason to build everything: without the
		// ledger the watermark is lower, so fewer old tags are current,
		// which is the conservative reading for a publishing event.
		w.logger().Warn("could not read the ledger: judging superseded tags without it", "err", err)
	}
	for _, r := range runs {
		if r.Phase == run.PhaseSucceeded && r.Event == isolation.EventTag.String() {
			consider(r.Ref)
		}
	}
}

// tagVersion reads a tag job as a version. Anything else — a branch, a pull
// request, a tag that is not a version — is not one.
func tagVersion(j Job) (release.Version, bool) {
	if j.Event != isolation.EventTag {
		return release.Version{}, false
	}
	return release.ParseTag(j.Ref)
}
