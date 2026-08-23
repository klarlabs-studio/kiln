package execx

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Fake is a scripted Runner for tests. It records every invocation and answers
// from a set of prefix-matched responses, so a test can assert both what Kiln
// decided to run and what it did with the answer.
//
// It lives in the non-test build because three packages' tests need it and a
// shared fake in one place beats three subtly different ones.
type Fake struct {
	mu sync.Mutex

	// Responses maps a command prefix ("git rev-parse", "docker push") to the
	// reply. The longest matching prefix wins, so a test can set a default for
	// "docker" and override "docker push" specifically.
	Responses map[string]Response
	// Missing names binaries LookPath should report as absent.
	Missing map[string]bool
	// Default is returned when no prefix matches. The zero value is a
	// successful, silent command.
	Default Response

	calls []Cmd
}

// Response is a scripted reply.
type Response struct {
	Stdout string
	Stderr string
	// ExitCode non-zero makes Run return an *ExitError, exactly as a real
	// failing subprocess would.
	ExitCode int
	// Err, when set, is returned verbatim — for modelling a start failure or a
	// cancelled context rather than a command that ran and failed.
	Err error
	// Fn, when set, runs instead of returning the static fields. Use it when a
	// response has to depend on the arguments (a tag-specific digest, say).
	Fn func(c Cmd) (Result, error)
}

// NewFake returns an empty Fake whose every command succeeds silently.
func NewFake() *Fake {
	return &Fake{Responses: map[string]Response{}, Missing: map[string]bool{}}
}

// On scripts a response for a command prefix and returns the Fake for
// chaining.
func (f *Fake) On(prefix string, r Response) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Responses == nil {
		f.Responses = map[string]Response{}
	}
	f.Responses[prefix] = r
	return f
}

// Absent marks a binary as not installed.
func (f *Fake) Absent(name string) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing == nil {
		f.Missing = map[string]bool{}
	}
	f.Missing[name] = true
	return f
}

func (f *Fake) LookPath(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[name] {
		return "", &NotFoundError{Name: name}
	}
	return "/usr/bin/" + name, nil
}

func (f *Fake) Run(ctx context.Context, c Cmd) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if _, err := f.LookPath(c.Name); err != nil {
		return Result{}, err
	}

	f.mu.Lock()
	f.calls = append(f.calls, c)
	resp := f.match(c.String())
	f.mu.Unlock()

	if resp.Fn != nil {
		return resp.Fn(c)
	}
	if resp.Err != nil {
		return Result{}, resp.Err
	}
	res := Result{Stdout: resp.Stdout, Stderr: resp.Stderr, ExitCode: resp.ExitCode}
	if resp.ExitCode != 0 {
		return res, &ExitError{
			Cmd: c.String(), Code: resp.ExitCode,
			Stderr: resp.Stderr, Combined: resp.Stdout + resp.Stderr,
		}
	}
	return res, nil
}

// match finds the longest scripted prefix of line.
func (f *Fake) match(line string) Response {
	best, bestLen := f.Default, -1
	for prefix, r := range f.Responses {
		if strings.HasPrefix(line, prefix) && len(prefix) > bestLen {
			best, bestLen = r, len(prefix)
		}
	}
	return best
}

// Calls returns every command run so far, in order.
func (f *Fake) Calls() []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Cmd(nil), f.calls...)
}

// Lines renders the calls as command strings, which is what assertions
// generally want to compare against.
func (f *Fake) Lines() []string {
	calls := f.Calls()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.String()
	}
	return out
}

// Ran reports whether any call started with prefix.
func (f *Fake) Ran(prefix string) bool { return f.Find(prefix) != nil }

// Find returns the first call starting with prefix, or nil.
func (f *Fake) Find(prefix string) *Cmd {
	for _, c := range f.Calls() {
		if strings.HasPrefix(c.String(), prefix) {
			cp := c
			return &cp
		}
	}
	return nil
}

// Count returns how many calls started with prefix.
func (f *Fake) Count(prefix string) int {
	n := 0
	for _, line := range f.Lines() {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// Transcript renders every call one per line, for failure messages.
func (f *Fake) Transcript() string {
	return fmt.Sprintf("commands run:\n  %s", strings.Join(f.Lines(), "\n  "))
}
