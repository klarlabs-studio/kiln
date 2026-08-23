package watch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
	"go.klarlabs.de/kiln/internal/engine"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/github"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
	"go.klarlabs.de/kiln/internal/infrastructure/prove"
	"go.klarlabs.de/kiln/internal/infrastructure/publish"
	"go.klarlabs.de/kiln/internal/infrastructure/store"
)

// fixture is a watcher over a real clone of a real repository, driven by an
// engine whose prover and publisher are stubs. Discovery is the thing under
// test here, and faking git well enough to test it would mean faking git.
type fixture struct {
	watcher  *Watcher
	upstream *gittest.Repo
	local    *gittest.Repo
	store    store.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	// A tick reaps abandoned worktrees out of os.TempDir(), so give the
	// watcher a temp directory of its own. Otherwise these tests sweep the
	// real one — the machine's actual kiln leavings, and whatever the worktree
	// package's own reaper tests are doing at the same moment.
	t.Setenv("TMPDIR", t.TempDir())

	upstream := gittest.New(t)
	upstream.Commit("first", "app.txt", "one\n")
	local := upstream.Clone(t)

	f := &fixture{upstream: upstream, local: local, store: store.NewMemory()}
	eng := engine.New(engine.Engine{
		Prover: prove.Func(func(context.Context, prove.Request) error { return nil }),
		Publisher: publish.Func(func(_ context.Context, req publish.Request) (publish.Result, error) {
			return publish.Result{Digest: "sha256:abc", Tags: []string{"ghcr.io/x/y:latest"}, Signed: true}, nil
		}),
		Store: f.store,
		Log:   obs.Discard(),
	})
	f.watcher = &Watcher{
		Engine:   eng,
		Store:    f.store,
		Runner:   execx.NewSystem(),
		Log:      obs.Discard(),
		Dir:      local.Dir,
		Repo:     "klarlabs-studio/kiln",
		Pipeline: defaultPipeline(t),
		// Retry backoff is exercised by fortify's own tests; here it would only
		// add seconds to every negative case.
		FetchAttempts: 1,
	}
	return f
}

func defaultPipeline(t *testing.T) config.Pipeline {
	t.Helper()
	p, err := config.Parse(strings.NewReader(`
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
  push: [prove]
watch:
  remote: origin
  ref: main
`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOnceBuildsTheTrackedBranch(t *testing.T) {
	f := newFixture(t)

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}

	if len(res.Executed) != 1 {
		t.Fatalf("executed %d jobs, want 1: %+v", len(res.Executed), res.Discovered)
	}
	job := res.Executed[0].Job
	if job.Ref != "refs/heads/main" || job.Event != isolation.EventPush {
		t.Errorf("job = %+v", job)
	}
	if job.SHA != f.upstream.Head() {
		t.Errorf("SHA = %s, want the upstream head %s", job.SHA, f.upstream.Head())
	}
}

func TestOnceFetchesNewCommits(t *testing.T) {
	f := newFixture(t)

	// A commit that only exists upstream: the tick must fetch it, not read a
	// stale local ref.
	newSHA := f.upstream.Commit("second", "app.txt", "two\n")

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Executed[0].Job.SHA != newSHA {
		t.Errorf("built %s, want the newly pushed %s", res.Executed[0].Job.SHA, newSHA)
	}
}

func TestSecondTickSkipsAnAlreadyBuiltHead(t *testing.T) {
	f := newFixture(t)

	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}

	// A doubled cron tick must be a no-op, or every minute costs a build.
	if len(res.Executed) != 0 {
		t.Errorf("rebuilt an unchanged head: %+v", res.Executed)
	}
	if len(res.Skipped) != 1 {
		t.Errorf("skipped = %+v", res.Skipped)
	}
}

func TestAFailedRunBacksOffAndIsRetriedLater(t *testing.T) {
	f := newFixture(t)
	failing := *f.watcher.Engine
	failing.Prover = prove.Func(func(context.Context, prove.Request) error {
		return errors.New("gate failed")
	})
	f.watcher.Engine = &failing

	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}

	// Not on the *next* tick. A watch loop runs every few minutes forever, so
	// retrying immediately means re-gating a broken commit until somebody
	// notices — measured on the first real box at 205 failed runs in an
	// afternoon, re-running `go test -race` across thirteen pull requests.
	if len(res.Executed) != 0 {
		t.Errorf("a commit that just failed was retried immediately: %+v", res)
	}
	if len(res.Skipped) != 1 {
		t.Errorf("skipped = %+v", res.Skipped)
	}

	// But it is retried. A failure is not always about the commit — a registry
	// down, a dependency yanked, a tool the box was missing — and those get
	// fixed without anybody pushing anything.
	later := time.Now().Add(engine.RetryBase + time.Minute)
	f.watcher.Now = func() time.Time { return later }

	res, err = f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 1 {
		t.Errorf("a failed head was never retried: %+v", res)
	}
}

func TestTheBackoffGrowsWithRepeatedFailures(t *testing.T) {
	f := newFixture(t)
	failing := *f.watcher.Engine
	failing.Prover = prove.Func(func(context.Context, prove.Request) error {
		return errors.New("gate failed")
	})
	f.watcher.Engine = &failing

	start := time.Now()
	// Two failures, spaced past the first delay so the second one happens.
	f.watcher.Now = func() time.Time { return start }
	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	second := start.Add(engine.RetryBase + time.Minute)
	f.watcher.Now = func() time.Time { return second }
	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	// Two failures means a 30-minute wait, measured from the last one. Twenty
	// minutes is past what a flat 15-minute policy would have required and
	// short of what a growing one does, which is exactly the difference this
	// test is for.
	f.watcher.Now = func() time.Time { return start.Add(20 * time.Minute) }
	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 0 {
		t.Errorf("the second failure did not lengthen the wait: %+v", res)
	}
}

func TestNewCommitAfterASuccessRebuilds(t *testing.T) {
	f := newFixture(t)
	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	next := f.upstream.Commit("third", "app.txt", "three\n")

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 1 || res.Executed[0].Job.SHA != next {
		t.Errorf("did not build the new commit: %+v", res)
	}
}

func TestTagsArePeeledToTheirCommit(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	commit := f.upstream.Head()
	f.upstream.Tag("v1.0.0")

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	var tagJob *Job
	for i := range res.Discovered {
		if res.Discovered[i].Ref == "refs/tags/v1.0.0" {
			tagJob = &res.Discovered[i]
		}
	}
	if tagJob == nil {
		t.Fatalf("tag not discovered: %+v", res.Discovered)
	}
	if tagJob.Event != isolation.EventTag {
		t.Errorf("Event = %s, want tag", tagJob.Event)
	}
	// An annotated tag has its own object id, which no worktree can check out
	// and no warden note is bound to.
	if tagJob.SHA != commit {
		t.Errorf("SHA = %s, want the peeled commit %s", tagJob.SHA, commit)
	}
}

func TestTagsCanBeDisabled(t *testing.T) {
	f := newFixture(t)
	f.upstream.Tag("v1.0.0")
	off := false
	f.watcher.Pipeline.Watch.Tags = &off

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range res.Discovered {
		if strings.HasPrefix(j.Ref, "refs/tags/") {
			t.Errorf("discovered a tag with watch.tags off: %+v", j)
		}
	}
}

func TestPullRequestsAreDiscoveredFromTheParkedRefs(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	// GitHub publishes a pull request head as refs/pull/N/head on the origin.
	// Creating one upstream is exactly the shape discovery fetches.
	prSHA := f.upstream.Commit("pr work", "feature.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/7/head", prSHA)
	f.upstream.Git("reset", "--hard", "HEAD~1")

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	var pr *Job
	for i := range res.Discovered {
		if res.Discovered[i].Ref == "refs/pull/7/head" {
			pr = &res.Discovered[i]
		}
	}
	if pr == nil {
		t.Fatalf("pull request not discovered: %+v", res.Discovered)
	}
	if pr.Event != isolation.EventPullRequest || pr.SHA != prSHA {
		t.Errorf("job = %+v", pr)
	}
}

func TestWithoutATokenEveryPullRequestIsAFork(t *testing.T) {
	f := newFixture(t)
	prSHA := f.upstream.Commit("pr work", "feature.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/7/head", prSHA)
	f.upstream.Git("reset", "--hard", "HEAD~1")
	f.watcher.Forge = nil

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	for _, j := range res.Discovered {
		if j.Event == isolation.EventPullRequest && !j.Fork {
			// The alternative hands an unknown author the operator's
			// credentials.
			t.Errorf("PR treated as same-repo without a token: %+v", j)
		}
	}
}

func TestTokenResolvesSameRepoPullRequests(t *testing.T) {
	f := newFixture(t)
	prSHA := f.upstream.Commit("pr work", "feature.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/7/head", prSHA)
	f.upstream.Git("update-ref", "refs/pull/8/head", prSHA)
	f.upstream.Git("reset", "--hard", "HEAD~1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"number": 7, "head": {"sha": "x", "ref": "f", "repo": {"full_name": "klarlabs-studio/kiln"}},
			 "base": {"repo": {"full_name": "klarlabs-studio/kiln"}}},
			{"number": 8, "head": {"sha": "y", "ref": "g", "repo": {"full_name": "stranger/kiln"}},
			 "base": {"repo": {"full_name": "klarlabs-studio/kiln"}}}
		]`))
	}))
	defer srv.Close()

	c := github.NewClient("tok", github.Repo{Owner: "klarlabs-studio", Name: "kiln"}, obs.Discard())
	c.BaseURL = srv.URL
	f.watcher.Forge = c

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	byRef := map[string]Job{}
	for _, j := range res.Discovered {
		byRef[j.Ref] = j
	}
	if got := byRef["refs/pull/7/head"]; got.Fork {
		t.Error("PR 7 is same-repo and should not be marked a fork")
	}
	if got := byRef["refs/pull/8/head"]; !got.Fork {
		t.Error("PR 8 comes from a fork")
	}
}

func TestAPIFailureFallsBackToFork(t *testing.T) {
	f := newFixture(t)
	prSHA := f.upstream.Commit("pr work", "feature.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/7/head", prSHA)
	f.upstream.Git("reset", "--hard", "HEAD~1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := github.NewClient("tok", github.Repo{Owner: "o", Name: "r"}, obs.Discard())
	c.BaseURL = srv.URL
	c.Attempts = 1
	f.watcher.Forge = c

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range res.Discovered {
		if j.Event == isolation.EventPullRequest && !j.Fork {
			t.Errorf("an unreachable API must not downgrade to same-repo: %+v", j)
		}
	}
}

func TestBranchesOnlySkipsPullRequestsAndTags(t *testing.T) {
	f := newFixture(t)
	f.upstream.Tag("v1.0.0")
	prSHA := f.upstream.Commit("pr", "f.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/7/head", prSHA)
	f.upstream.Git("reset", "--hard", "HEAD~1")
	f.watcher.BranchesOnly = true

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Discovered) != 1 || res.Discovered[0].Ref != "refs/heads/main" {
		t.Errorf("poll should see only the tracked branch, got %+v", res.Discovered)
	}
}

func TestDryRunExecutesNothing(t *testing.T) {
	f := newFixture(t)

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Executed) != 1 {
		t.Fatalf("dry run should still report the plan: %+v", res)
	}
	if res.Executed[0].Run != nil {
		t.Error("a dry run must not produce a run record")
	}
	if _, err := f.store.Latest(); err == nil {
		t.Error("a dry run wrote to the ledger")
	}
}

func TestOneFailingJobDoesNotStopTheTick(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.upstream.Tag("v1.0.0")

	failFirst := true
	eng := *f.watcher.Engine
	eng.Prover = prove.Func(func(context.Context, prove.Request) error {
		if failFirst {
			failFirst = false
			return errors.New("first job fails")
		}
		return nil
	})
	f.watcher.Engine = &eng

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatalf("a failing job must not fail the tick: %v", err)
	}

	// A broken pull request must not stop main from building; otherwise
	// anybody who can open a PR can halt the pipeline.
	if len(res.Executed) != 2 {
		t.Errorf("executed %d jobs, want both: %+v", len(res.Executed), res.Executed)
	}
	if res.Failures() != 1 {
		t.Errorf("failures = %d, want 1", res.Failures())
	}
}

func TestFetchFailureFailsTheTick(t *testing.T) {
	f := newFixture(t)
	f.watcher.Runner = execx.NewFake().On("git fetch", execx.Response{ExitCode: 128, Stderr: "no such remote"})

	if _, err := f.watcher.Once(t.Context(), false); err == nil {
		t.Error("a tick that cannot fetch the tracked branch must fail")
	}
}

func TestMissingPullRefsAreNotFatal(t *testing.T) {
	// A non-GitHub remote has no refs/pull/*. The branch must still build.
	f := newFixture(t)
	fake := execx.NewFake()
	fake.On("git", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.NewSystem().Run(t.Context(), c)
	}})
	fake.On("git fetch --prune --quiet origin +refs/pull", execx.Response{
		ExitCode: 128, Stderr: "couldn't find remote ref refs/pull/*/head",
	})
	f.watcher.Runner = fake

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(res.Discovered) == 0 {
		t.Error("the branch job was lost when the pull fetch failed")
	}
}

func TestCancellationStopsTheTick(t *testing.T) {
	f := newFixture(t)
	f.upstream.Tag("v1.0.0")

	ctx, cancel := context.WithCancel(t.Context())
	eng := *f.watcher.Engine
	eng.Prover = prove.Func(func(context.Context, prove.Request) error {
		cancel()
		return nil
	})
	f.watcher.Engine = &eng

	res, err := f.watcher.Once(ctx, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Continuing would run the rest against a dead context and record a
	// cascade of confusing failures.
	if len(res.Executed) != 1 {
		t.Errorf("executed %d jobs after cancellation, want 1", len(res.Executed))
	}
}

func TestEveryRunsImmediatelyThenStops(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- f.watcher.Every(ctx, time.Hour, false) }()

	// The first tick must not wait for the interval: an operator starting a
	// watcher wants to know now whether it works.
	deadline := time.After(10 * time.Second)
	for {
		if _, err := f.store.Latest(); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the first tick did not run immediately")
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Every returned %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Every did not return after cancellation")
	}
}

func TestEveryRejectsANonPositiveInterval(t *testing.T) {
	f := newFixture(t)

	if err := f.watcher.Every(t.Context(), 0, false); err == nil {
		t.Error("want an error for a zero interval")
	}
}

func TestResultFailures(t *testing.T) {
	r := Result{Executed: []Outcome{
		{Err: errors.New("x")},
		{Run: &run.Run{}},
		{Err: errors.New("y")},
	}}

	if got := r.Failures(); got != 2 {
		t.Errorf("Failures = %d, want 2", got)
	}
}

func TestAClosedPullRequestStopsBeingBuilt(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	prSHA := f.upstream.Commit("pr work", "feature.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/7/head", prSHA)
	f.upstream.Git("reset", "--hard", "HEAD~1")

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRef(res.Discovered, "refs/pull/7/head") {
		t.Fatalf("the open pull request was not discovered: %+v", res.Discovered)
	}

	// GitHub does not remove refs/pull/N/head when a pull request closes —
	// that is what #29 was about. A ref genuinely disappearing from the remote
	// is a different case, and the pruning fetch must still carry it through.
	f.upstream.Git("update-ref", "-d", "refs/pull/7/head")

	res, err = f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if hasRef(res.Discovered, "refs/pull/7/head") {
		t.Errorf("a closed pull request is still being built: %+v", res.Discovered)
	}
}

func hasRef(jobs []Job, ref string) bool {
	for _, j := range jobs {
		if j.Ref == ref {
			return true
		}
	}
	return false
}

// alreadyWatching marks the box as having ticked before, with nothing on the
// repository at the time. A tag or pull request created after this is new work
// rather than history the box inherited, which is the case most tests mean to
// describe.
func (f *fixture) alreadyWatching(t *testing.T) {
	t.Helper()
	if err := SaveBaseline(f.watcher.Dir, &Baseline{Tags: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
}
