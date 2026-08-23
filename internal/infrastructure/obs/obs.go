// Package obs is Kiln's logging seam.
//
// Every surface writes bolt JSON to STDERR, never stdout. That is not a style
// preference: `kiln mcp serve` speaks JSON-RPC on stdout, so a single stray log
// line there corrupts the protocol. Keeping the writer fixed at stderr in one
// place means no caller can get it wrong.
//
// The rest of the codebase depends on the narrow ports.Logger interface below rather
// than on *bolt.Logger, so unit tests can pass Discard and the engine has no
// opinion about the logging backend.
package obs

import (
	"fmt"
	"io"
	"os"

	"go.klarlabs.de/bolt"
	"go.klarlabs.de/kiln/internal/application/ports"
)

// New returns the production logger: bolt JSON on stderr at the given level.
func New(level string) ports.Logger {
	return NewTo(os.Stderr, level)
}

// NewTo is New with an explicit writer, for tests that assert on log output.
func NewTo(w io.Writer, level string) ports.Logger {
	lg := bolt.New(bolt.NewJSONHandler(w))
	if level != "" {
		lg.SetLevel(bolt.ParseLevel(level))
	}
	return &boltLogger{log: lg}
}

// Discard is a logger that drops everything. Tests use it so a failing
// assertion is not buried in log noise.
func Discard() ports.Logger { return ports.DiscardLogger() }

type boltLogger struct {
	log  *bolt.Logger
	base []any
}

func (b *boltLogger) Debug(msg string, kv ...any) { b.emit(b.log.Debug(), msg, kv) }
func (b *boltLogger) Info(msg string, kv ...any)  { b.emit(b.log.Info(), msg, kv) }
func (b *boltLogger) Warn(msg string, kv ...any)  { b.emit(b.log.Warn(), msg, kv) }
func (b *boltLogger) Error(msg string, kv ...any) { b.emit(b.log.Error(), msg, kv) }

func (b *boltLogger) With(kv ...any) ports.Logger {
	merged := make([]any, 0, len(b.base)+len(kv))
	merged = append(merged, b.base...)
	merged = append(merged, kv...)
	return &boltLogger{log: b.log, base: merged}
}

// emit applies the child logger's bound fields first, then the call's own, so a
// per-call key overrides an inherited one in the rendered object.
func (b *boltLogger) emit(ev *bolt.Event, msg string, kv []any) {
	apply(ev, b.base)
	apply(ev, kv)
	ev.Msg(msg)
}

// apply walks kv in pairs. Typed cases exist for the shapes Kiln actually logs;
// everything else falls back to Any so no field is silently lost.
func apply(ev *bolt.Event, kv []any) {
	for i := 0; i < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			key = fmt.Sprint(kv[i])
		}
		if i+1 >= len(kv) {
			ev.Str(key, "")
			return
		}
		switch v := kv[i+1].(type) {
		case string:
			ev.Str(key, v)
		case int:
			ev.Int(key, v)
		case bool:
			ev.Bool(key, v)
		case error:
			ev.Str(key, v.Error())
		case []string:
			ev.Strs(key, v)
		default:
			ev.Any(key, v)
		}
	}
}
