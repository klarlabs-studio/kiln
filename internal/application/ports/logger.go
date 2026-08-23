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
