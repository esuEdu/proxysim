package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"proxysim/internal/origin"
	"proxysim/internal/sim"
)

// controlHub returns a Hub wired with the given controller and stub enumerators,
// served over httptest so the control endpoints can be exercised end to end.
func controlHub(t *testing.T, ctrl *origin.Controller, sims []sim.Device, apps []sim.App) *httptest.Server {
	t.Helper()
	h := New(10)
	h.SetControl(ctrl,
		func(context.Context) ([]sim.Device, error) { return sims, nil },
		func(context.Context, string) ([]sim.App, error) { return apps, nil },
	)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// Criterion 3 & 4: GET /filter reflects the live controller; PUT /filter changes
// what the proxy would read next.
func TestFilterGetAndPut(t *testing.T) {
	ctrl := origin.NewController(origin.BuildFilter(true, nil)) // seeded only-sim
	srv := controlHub(t, ctrl, nil, nil)

	var got filterJSON
	getJSON(t, srv.URL+"/filter", &got)
	if !got.OnlySim || len(got.Apps) != 0 {
		t.Fatalf("GET /filter = %+v, want only-sim", got)
	}

	// Switch to a single app.
	putJSON(t, srv.URL+"/filter", filterJSON{Apps: []string{"com.example.A"}})
	if cur := ctrl.Current(); !cur.Apps["com.example.A"] || !cur.OnlySim {
		t.Fatalf("controller after PUT = %+v, want app A", cur)
	}
	getJSON(t, srv.URL+"/filter", &got)
	if len(got.Apps) != 1 || got.Apps[0] != "com.example.A" || !got.OnlySim {
		t.Fatalf("GET /filter after PUT = %+v", got)
	}

	// Clearing intercepts everything again.
	putJSON(t, srv.URL+"/filter", filterJSON{})
	if ctrl.Current().Active() {
		t.Fatalf("controller after clearing = %+v, want inactive", ctrl.Current())
	}
}

// Criterion 5: PUT validates shape, not existence. An uninstalled bundle id is
// accepted; a malformed body is 400.
func TestFilterPutValidation(t *testing.T) {
	ctrl := origin.NewController(origin.Filter{})
	srv := controlHub(t, ctrl, nil, nil)

	// A bundle id we never enumerated is still a valid selection.
	resp := putRaw(t, srv.URL+"/filter", `{"apps":["com.nowhere.Ghost"]}`)
	if resp != http.StatusOK {
		t.Fatalf("uninstalled bundle id: status %d, want 200", resp)
	}
	if !ctrl.Current().Apps["com.nowhere.Ghost"] {
		t.Error("uninstalled bundle id should still be stored")
	}

	if resp := putRaw(t, srv.URL+"/filter", `{not json`); resp != http.StatusBadRequest {
		t.Fatalf("malformed body: status %d, want 400", resp)
	}
}

// Criterion 6: GET /sims returns booted devices; none yields [] not an error.
func TestSimsEndpoint(t *testing.T) {
	sims := []sim.Device{
		{UDID: "AAAA", Name: "iPhone 15", Runtime: "iOS-17"},
		{UDID: "BBBB", Name: "iPad Pro", Runtime: "iOS-17"},
	}
	srv := controlHub(t, origin.NewController(origin.Filter{}), sims, nil)

	var got []map[string]string
	getJSON(t, srv.URL+"/sims", &got)
	if len(got) != 2 || got[0]["udid"] != "AAAA" || got[0]["name"] != "iPhone 15" {
		t.Fatalf("GET /sims = %+v", got)
	}

	empty := controlHub(t, origin.NewController(origin.Filter{}), nil, nil)
	body := getBody(t, empty.URL+"/sims")
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("GET /sims with no booted device = %q, want []", body)
	}
}

// Criterion 7: GET /sims/{udid}/apps returns that sim's apps; empty yields [].
func TestAppsEndpoint(t *testing.T) {
	apps := []sim.App{{BundleID: "com.example.A", Name: "App A"}}
	srv := controlHub(t, origin.NewController(origin.Filter{}), nil, apps)

	var got []sim.App
	getJSON(t, srv.URL+"/sims/AAAA/apps", &got)
	if len(got) != 1 || got[0].BundleID != "com.example.A" || got[0].Name != "App A" {
		t.Fatalf("GET /sims/AAAA/apps = %+v", got)
	}

	empty := controlHub(t, origin.NewController(origin.Filter{}), nil, nil)
	if body := getBody(t, empty.URL+"/sims/AAAA/apps"); strings.TrimSpace(body) != "[]" {
		t.Fatalf("GET apps with none = %q, want []", body)
	}
}

// Criterion 9: the control endpoints are a capability of the UI server only. A Hub
// with control unwired reports "not enabled" (501), so the endpoints never leak
// interception control onto a server that did not opt in.
func TestControlEndpointsDisabledWithoutSetControl(t *testing.T) {
	srv := httptest.NewServer(New(10).Handler()) // no SetControl
	t.Cleanup(srv.Close)

	for _, path := range []string{"/filter", "/sims", "/sims/AAAA/apps"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("GET %s without control = %d, want 501", path, resp.StatusCode)
		}
	}
}

// ---- helpers (getJSON is shared with hub_test.go) ---------------------------

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

func putJSON(t *testing.T, url string, body filterJSON) {
	t.Helper()
	data, _ := json.Marshal(body)
	if status := putRaw(t, url, string(data)); status != http.StatusOK {
		t.Fatalf("PUT %s = %d, want 200", url, status)
	}
}

func putRaw(t *testing.T, url, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
