// Package flowtest holds fixtures shared by tests that must agree on the Flow
// JSON wire contract. It exists for one reason: specs 003 and 008 both require
// that the console, the browser UI, and any future exporter assert the served
// JSON against a single canonical field set, so no consumer can let the format
// drift in a private copy of the list.
//
// It deliberately does not import internal/flow. flow's own in-package tests
// import this package, and pulling flow back in here would create an import
// cycle in flow's test binary. Keeping the fixture flow-free avoids that while
// still giving every consumer one list to compare against.
package flowtest

import (
	"encoding/json"
	"sort"
	"testing"
)

// GoldenFieldSet is the sorted set of top-level JSON keys a marshalled Flow must
// carry — spec 003's stable tags. A field renamed or dropped makes every test
// that compares against this fail, which is the entire point of centralising it.
var GoldenFieldSet = []string{
	"duration_ns", "error", "host", "id", "intercepted", "method", "path",
	"request_body", "request_headers", "request_truncated",
	"response_body", "response_headers", "response_truncated",
	"scheme", "started", "status_code",
}

// FieldSet marshals v as JSON and returns its top-level object keys, sorted, so a
// caller can compare the result against GoldenFieldSet. It fails the test if v
// does not marshal to a JSON object.
func FieldSet(tb testing.TB, v any) []string {
	tb.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		tb.Fatalf("flowtest: marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		tb.Fatalf("flowtest: value did not marshal to a JSON object: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
