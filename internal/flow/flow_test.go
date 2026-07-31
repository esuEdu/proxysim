package flow

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Criterion 1: a populated flow round-trips through JSON unchanged, including a
// body of arbitrary non-UTF-8 bytes.
func TestFlowJSONRoundTrip(t *testing.T) {
	orig := Flow{
		ID:                42,
		Started:           time.Date(2026, 7, 30, 12, 0, 0, 123456789, time.UTC),
		Duration:          1500 * time.Millisecond,
		Scheme:            "https",
		Method:            "POST",
		Host:              "api.example.com",
		Path:              "/v1/items?limit=10",
		RequestHeaders:    http.Header{"Content-Type": {"application/json"}},
		RequestBody:       []byte{0x00, 0xff, 0xfe, 0x80, 'h', 'i'},
		RequestTruncated:  true,
		StatusCode:        201,
		ResponseHeaders:   http.Header{"Server": {"nginx"}},
		ResponseBody:      []byte{0x89, 0x50, 0x4e, 0x47}, // non-UTF-8 (PNG magic)
		ResponseTruncated: false,
		Intercepted:       true,
		Error:             "",
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Flow
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(orig, back) {
		t.Errorf("round-trip mismatch:\n orig = %#v\n back = %#v", orig, back)
	}
}

// Criterion 2: the JSON key set is a stable contract. This golden test fails if
// a field is renamed or dropped — that is its entire purpose.
func TestFlowJSONFieldSet(t *testing.T) {
	// Every field non-zero so omitempty ones (bodies, error) are present.
	f := Flow{
		ID: 1, Started: time.Unix(0, 0).UTC(), Duration: 1,
		Scheme: "https", Method: "GET", Host: "h", Path: "/",
		RequestHeaders: http.Header{}, RequestBody: []byte("x"), RequestTruncated: true,
		StatusCode: 200, ResponseHeaders: http.Header{}, ResponseBody: []byte("y"), ResponseTruncated: true,
		Intercepted: true, Error: "boom",
	}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)

	want := []string{
		"duration_ns", "error", "host", "id", "intercepted", "method", "path",
		"request_body", "request_headers", "request_truncated",
		"response_body", "response_headers", "response_truncated",
		"scheme", "started", "status_code",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JSON field set changed.\n got = %v\nwant = %v", got, want)
	}
}

// Criterion 3: concurrent NextID calls yield distinct values.
func TestNextIDUnique(t *testing.T) {
	const n = 1000
	var wg sync.WaitGroup
	ids := make([]uint64, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = NextID()
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]struct{}, n)
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	if len(seen) != n {
		t.Errorf("got %d distinct IDs, want %d", len(seen), n)
	}
}

// Criterion 6: internal/flow imports nothing from internal/proxy or internal/ca.
// The natural drift is for flow to reach into proxy for convenience; this guards it.
func TestNoProxyOrCAImports(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "internal/proxy") || strings.Contains(path, "internal/ca") {
				t.Errorf("%s imports %q; flow must not depend on proxy or ca", name, path)
			}
		}
	}
}
