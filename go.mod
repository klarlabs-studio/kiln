module go.klarlabs.de/kiln

go 1.25.0

// The language floor above is what consumers must meet. This is what the Go
// command actually builds with, and it is a different question: go1.25.0 is
// fourteen patch releases behind, and govulncheck reports reachable call paths
// into its standard library — asn1.Unmarshal, tls.Conn.Write,
// template.Template.Execute among them.
//
// CI resolves the toolchain from this file, so without this line every
// release carries those vulnerabilities: kiln 0.4.1 reports go1.25.0 under
// `go version -m`. Raising the go directive instead would fix the artifact
// and raise the floor for anything importing this module, which is a cost
// with no matching benefit.
toolchain go1.25.14

require (
	go.klarlabs.de/bolt v1.6.0
	go.klarlabs.de/fortify v1.10.0
	go.klarlabs.de/mcp v1.27.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260630182238-925bb5da69e7 // indirect
	google.golang.org/grpc v1.82.1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
