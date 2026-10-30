package health

import (
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

func TestFreeBytesReadsTheFilesystem(t *testing.T) {
	free, err := New(execx.NewFake()).FreeBytes(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Error("a writable temp directory reported no free space")
	}
}

func TestFreeBytesOfAMissingPathIsAnError(t *testing.T) {
	if _, err := New(execx.NewFake()).FreeBytes("/does/not/exist/kiln"); err == nil {
		t.Error("no error for a path that does not exist")
	}
}

func TestDockerReadyWhenTheDaemonAnswers(t *testing.T) {
	f := execx.NewFake().On("docker version", execx.Response{Stdout: "29.1.0\n"})
	if err := New(f).DockerReady(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// The daemon's own sentence is what an operator needs, not "exit 1".
func TestDockerReadyCarriesTheDaemonsExplanation(t *testing.T) {
	f := execx.NewFake().On("docker version", execx.Response{
		ExitCode: 1,
		Stderr:   "Client: 29.1.0\nError response from daemon: Docker Desktop is unable to start\n",
	})
	err := New(f).DockerReady(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Docker Desktop is unable to start") {
		t.Errorf("err = %v", err)
	}
}

func TestDockerReadyWithoutDocker(t *testing.T) {
	err := New(execx.NewFake().Absent("docker")).DockerReady(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("err = %v", err)
	}
}
