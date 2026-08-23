package attest_test

import (
	"flag"
	"strconv"
	"strings"
)

// update rewrites the published example instead of comparing against it.
var update = flag.Bool("update", false, "rewrite examples/provenance.example.json")

func splitPath(p string) []string { return strings.Split(p, ".") }

func index(s string) (int, error) { return strconv.Atoi(s) }
