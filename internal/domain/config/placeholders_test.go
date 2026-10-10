package config

import (
	"strings"
	"testing"
)

func TestArgs_KnownPlaceholdersLoad(t *testing.T) {
	_, err := Parse(strings.NewReader(argsBase + `  - kind: image
    image: ghcr.io/acme/web
    tags: [sha, latest]
    args:
      VERSION: "${kiln.version}"
      REVISION: "${kiln.sha}"
      SHORT: "r-${kiln.short_sha}"
      REF: "${kiln.ref}"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestArgs_UnknownPlaceholderIsALoadError(t *testing.T) {
	_, err := Parse(strings.NewReader(argsBase + `  - kind: image
    image: ghcr.io/acme/web
    tags: [sha, latest]
    args:
      X: "${kiln.nope}"
`))
	if err == nil {
		t.Fatal("want error for unknown placeholder")
	}
	for _, want := range []string{"args.X", "kiln.nope", "kiln.sha", "kiln.short_sha", "kiln.version", "kiln.ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestArgs_NonKilnDollarsStayLiteral(t *testing.T) {
	_, err := Parse(strings.NewReader(argsBase + `  - kind: image
    image: ghcr.io/acme/web
    tags: [sha, latest]
    args:
      A: "${HOME}"
      B: "$PATH"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestResolveArgs(t *testing.T) {
	ctx := ArgContext{SHA: "0123456789abcdef", ShortSHA: "0123456", Version: "1.2.3", Ref: "refs/tags/v1.2.3"}
	got, err := ResolveArgs(map[string]string{
		"V": "${kiln.version}", "R": "${kiln.sha}", "S": "${kiln.short_sha}",
		"E": "v${kiln.version}-${kiln.short_sha}", "L": "literal", "F": "${kiln.ref}", "H": "${HOME}",
	}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"V": "1.2.3", "R": "0123456789abcdef", "S": "0123456",
		"E": "v1.2.3-0123456", "L": "literal", "F": "refs/tags/v1.2.3", "H": "${HOME}",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

func TestResolveArgs_VersionWithoutTagIsAnError(t *testing.T) {
	_, err := ResolveArgs(map[string]string{"V": "${kiln.version}"}, ArgContext{SHA: "abc", ShortSHA: "abc", Ref: "refs/heads/main"})
	if err == nil || !strings.Contains(err.Error(), "kiln.version") {
		t.Fatalf("err = %v", err)
	}
	// Other placeholders still resolve without a tag.
	if _, err := ResolveArgs(map[string]string{"S": "${kiln.sha}"}, ArgContext{SHA: "abc", ShortSHA: "abc"}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveArgs_NilStaysNil(t *testing.T) {
	if got, err := ResolveArgs(nil, ArgContext{}); got != nil || err != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}
