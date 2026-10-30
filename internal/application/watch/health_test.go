package watch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
)

// stubHealth is a box whose disk and daemon say whatever the test needs.
type stubHealth struct {
	free      uint64
	freeErr   error
	docker    error
	dockerAsk int
}

func (s *stubHealth) FreeBytes(string) (uint64, error) { return s.free, s.freeErr }

func (s *stubHealth) DockerReady(context.Context) error {
	s.dockerAsk++
	return s.docker
}

const gib = 1 << 30

func imagePipeline(t *testing.T) config.Pipeline {
	t.Helper()
	p, err := config.Parse(strings.NewReader(`
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
  push: [prove, publish]
publish:
  - image: ghcr.io/klarlabs-studio/kiln
watch:
  remote: origin
  ref: main
`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A full disk failed every job of every tick in the linker, and recorded each
// failure against a commit that had done nothing wrong: 239 such runs on one
// box before anybody looked. The tick should refuse once, saying why, and
// leave the ledger alone.
func TestAFullDiskBuildsNothingAndBlamesNoCommit(t *testing.T) {
	f := newFixture(t)
	f.watcher.Health = &stubHealth{free: 1 * gib}
	f.watcher.MinFree = 5 * gib

	res, err := f.watcher.Once(t.Context(), false)
	if !errors.Is(err, ports.ErrBoxNotReady) {
		t.Fatalf("err = %v, want ErrBoxNotReady", err)
	}
	if !strings.Contains(err.Error(), "1.0 GiB free") {
		t.Errorf("the error does not say how full the disk is: %v", err)
	}
	if len(res.Executed) != 0 {
		t.Errorf("a full box built anyway: %+v", res.Executed)
	}
	if runs, _ := f.store.List(); len(runs) != 0 {
		t.Errorf("a full disk was recorded as %d run(s)", len(runs))
	}
}

func TestEnoughDiskBuildsAsUsual(t *testing.T) {
	f := newFixture(t)
	f.watcher.Health = &stubHealth{free: 50 * gib}
	f.watcher.MinFree = 5 * gib

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 1 {
		t.Errorf("executed = %+v", res.Executed)
	}
}

// A check that cannot look must not become the outage. Unknown is not full.
func TestAnUnreadableDiskDoesNotStopTheTick(t *testing.T) {
	f := newFixture(t)
	f.watcher.Health = &stubHealth{freeErr: errors.New("statfs: permission denied")}
	f.watcher.MinFree = 5 * gib

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 1 {
		t.Errorf("executed = %+v", res.Executed)
	}
}

// Docker Desktop failing to start turned every image build into a failed run.
// The job is held back instead, unrecorded, so the first tick after docker
// recovers builds it — not an hour of backoff later.
func TestAJobNeedingDockerIsDeferredWhileTheDaemonIsDown(t *testing.T) {
	f := newFixture(t)
	f.watcher.Pipeline = imagePipeline(t)
	box := &stubHealth{docker: errors.New("Docker Desktop is unable to start")}
	f.watcher.Health = box

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deferred) != 1 || len(res.Executed) != 0 {
		t.Fatalf("deferred = %+v, executed = %+v", res.Deferred, res.Executed)
	}
	if runs, _ := f.store.List(); len(runs) != 0 {
		t.Errorf("a down daemon was recorded as %d run(s)", len(runs))
	}

	box.docker = nil
	res, err = f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 1 {
		t.Errorf("the deferred job was not built once docker answered: %+v", res)
	}
}

// Only jobs that start a container wait for docker. A box whose daemon is down
// can still gate a pull request that builds no image.
func TestJobsThatNeedNoDockerRunWithoutIt(t *testing.T) {
	f := newFixture(t)
	box := &stubHealth{docker: errors.New("down")}
	f.watcher.Health = box

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 1 || len(res.Deferred) != 0 {
		t.Errorf("executed = %+v, deferred = %+v", res.Executed, res.Deferred)
	}
	if box.dockerAsk != 0 {
		t.Errorf("asked docker %d time(s) for a tick that needed none", box.dockerAsk)
	}
}

// KILN_DRY plans images without building them, so a dry box needs no daemon
// for a publish.
func TestPlannedImagesNeedNoDocker(t *testing.T) {
	f := newFixture(t)
	f.watcher.Pipeline = imagePipeline(t)
	f.watcher.Health = &stubHealth{docker: errors.New("down")}
	f.watcher.PlanImages = true

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 1 {
		t.Errorf("executed = %+v, deferred = %+v", res.Executed, res.Deferred)
	}
}
