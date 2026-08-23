package ports

// Logger is the subset of structured logging Kiln uses. Fields are key/value
// pairs; an odd trailing key is rendered with an empty value rather than
// dropped, so a miscounted call still shows up in the log.
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
	With(kv ...any) Logger
}

// DiscardLogger is a Logger that throws everything away.
//
// It lives beside the port rather than with the real logger so a component
// whose Log is nil can default itself without reaching into infrastructure for
// a no-op — the same reason NoopReporter is here.
func DiscardLogger() Logger { return discardLogger{} }

type discardLogger struct{}

func (discardLogger) Debug(string, ...any) {}
func (discardLogger) Info(string, ...any)  {}
func (discardLogger) Warn(string, ...any)  {}
func (discardLogger) Error(string, ...any) {}
func (discardLogger) With(...any) Logger   { return discardLogger{} }
