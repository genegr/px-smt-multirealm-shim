//go:build integration

// This file builds only under `-tags integration`. It exercises the shim's FADA connection
// rewrite — including the realm-access grant — against a REAL FlashArray, so the REST version and
// body of POST /resource-accesses/batch (and the grant-then-connect behaviour) are validated on
// actual Purity rather than a mock. It is inert in normal `go test`.
//
// Run it from a host that can reach the array (e.g. the jump host), supplying the array admin
// token and the objects it should use. Everything it creates uses the distinctive "eg-ittest"
// marker and is torn down at the end (array-safety rule: only touch what we created).
//
//	IT_ARRAY_URL=https://<array-ip> \
//	IT_ARRAY_TOKEN=<array-admin token> \
//	IT_REALM=ocp4-1-realm1 IT_POD=pxe-1 \
//	IT_NODE=worker0 IT_ARRAY_HOST=ocp4-1-worker0 \
//	IT_WWNS=<wwn1,wwn2>            # or IT_IQNS / IT_NQNS, matching the array host's transport
//	go test -tags integration -run TestFADAConnectAgainstRealArray -v ./internal/proxy/
package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/genegr/px-smt-multirealm-shim/internal/config"
)

func itEnv(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Skipf("integration test skipped: %s not set", key)
	}
	return v
}

// qs builds a percent-encoded query string for the given key/value pairs.
func qs(kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return q.Encode()
}

// TestRealmAccessGrantAgainstRealArray validates ONLY the new REST call — POST /resource-accesses/batch
// at resourceAccessPath's version — against real Purity, creating and connecting nothing. It grants
// the (already-provisioned) array host access into the realm; Purity replies "already exists", which
// ensureRealmAccess treats as success. A failure here — especially a 404 — means the REST version in
// resourceAccessPath is wrong for this array. This is the safe, non-mutating real-array check.
func TestRealmAccessGrantAgainstRealArray(t *testing.T) {
	arrayURL := itEnv(t, "IT_ARRAY_URL")
	token := itEnv(t, "IT_ARRAY_TOKEN")
	realm := itEnv(t, "IT_REALM")
	arrayHost := itEnv(t, "IT_ARRAY_HOST")

	up, err := url.Parse(arrayURL)
	if err != nil {
		t.Fatalf("IT_ARRAY_URL: %v", err)
	}
	rw := NewRewriter(&config.Config{
		UpstreamURL: up, InsecureUpstream: true, RewriteEnabled: true, ArrayToken: token,
		Hosts: []config.HostMapping{{Node: "ignored", ArrayHost: arrayHost}},
	})
	if err := rw.ensureRealmAccess(arrayHost, realm); err != nil {
		t.Fatalf("resource-access grant against real array failed (a 404 => wrong REST version in resourceAccessPath=%q): %v", resourceAccessPath, err)
	}
	t.Logf("OK: POST %s accepted the grant for host=%s realm=%s on the real array", resourceAccessPath, arrayHost, realm)
}

// TestFADAConnectAgainstRealArray replays the FADA attach against a real FlashArray: it simulates
// px by creating a throwaway realm volume and an empty realm host, drives the connect through the
// shim (which must grant the array host realm access, then connect it to the volume), and verifies
// on the array that the volume ended up connected to the array-level host. It cleans up afterwards.
func TestFADAConnectAgainstRealArray(t *testing.T) {
	arrayURL := itEnv(t, "IT_ARRAY_URL")
	token := itEnv(t, "IT_ARRAY_TOKEN")
	realm := itEnv(t, "IT_REALM")
	pod := itEnv(t, "IT_POD")
	node := itEnv(t, "IT_NODE")
	arrayHost := itEnv(t, "IT_ARRAY_HOST")
	// At least one transport's initiators (only used for the GET /hosts injection assertion).
	wwns, iqns, nqns := os.Getenv("IT_WWNS"), os.Getenv("IT_IQNS"), os.Getenv("IT_NQNS")

	up, err := url.Parse(arrayURL)
	if err != nil {
		t.Fatalf("IT_ARRAY_URL: %v", err)
	}

	vol := realm + "::" + pod + "::eg-ittest-fada"
	realmHost := realm + "::" + node + "-egittest"

	// A direct array-admin session for setup, verification, and teardown (mirrors what px + the
	// array would have on the real cluster).
	sess := newArraySession(up, token, true)
	if err := sess.validate(); err != nil {
		t.Fatalf("array-admin login failed (bad IT_ARRAY_TOKEN or unreachable IT_ARRAY_URL?): %v", err)
	}

	// Clean any leftovers from a previous run, then ensure teardown at the end.
	teardown := func() {
		sess.do(http.MethodDelete, "/api/2.2/connections?"+qs("host_names", arrayHost, "volume_names", vol), nil)
		sess.do(http.MethodDelete, "/api/2.2/hosts?"+qs("names", realmHost), nil)
		sess.do(http.MethodPatch, "/api/2.2/volumes?"+qs("names", vol), []byte(`{"destroyed":true}`))
		sess.do(http.MethodDelete, "/api/2.2/volumes?"+qs("names", vol), nil)
	}
	teardown()
	t.Cleanup(teardown)

	// Simulate px: create the realm volume (in the pod) and the empty realm host.
	if st, body, err := sess.do(http.MethodPost, "/api/2.2/volumes?"+qs("names", vol), []byte(`{"provisioned":1073741824}`)); err != nil || st != 200 {
		t.Fatalf("create realm volume %s: status=%d err=%v body=%s", vol, st, err, body)
	}
	if st, body, err := sess.do(http.MethodPost, "/api/2.2/hosts?"+qs("names", realmHost), []byte(`{}`)); err != nil || st != 200 {
		t.Fatalf("create realm host %s: status=%d err=%v body=%s", realmHost, st, err, body)
	}

	// Build the shim pointed at the real array, mapping the node prefix to the array-level host.
	handler, err := New(&config.Config{
		UpstreamURL: up, InsecureUpstream: true, RewriteEnabled: true, MaxLogBodyBytes: 4096,
		ArrayToken: token,
		Hosts:      []config.HostMapping{{Node: node, ArrayHost: arrayHost, IQNs: iqns, WWNs: wwns, NQNs: nqns}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fqdn := realm + "-fa.demo.pure"
	shimReq := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://"+fqdn+path, nil)
		req.Host = fqdn
		// Pass-through requests (e.g. GET /hosts) carry the caller's session token to the array;
		// use the array-admin session so they authenticate. The intercepted connect path uses the
		// shim's own array session regardless of this header.
		req.Header.Set("X-Auth-Token", sess.token())
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// Sanity: px lists the realm's hosts through the shim; the array host's initiators are injected.
	if wwns != "" || iqns != "" || nqns != "" {
		rec := shimReq(http.MethodGet, "/api/2.2/hosts?"+qs("filter", "is_local='true'", "names", realm+"::*"))
		if rec.Code != 200 {
			t.Fatalf("GET /hosts through shim: %d %s", rec.Code, rec.Body.String())
		}
		want := firstNonEmpty(wwns, iqns, nqns)
		first := strings.Split(want, ",")[0]
		if !strings.Contains(rec.Body.String(), first) {
			t.Errorf("shim did not inject array-host initiator %q into GET /hosts view", first)
		}
	}

	// The real thing: connect the realm volume to (what px thinks is) its realm host. The shim must
	// grant the array host access into the realm, then connect it to the volume.
	rec := shimReq(http.MethodPost, "/api/2.2/connections?"+qs("host_names", realmHost, "volume_names", vol))
	if rec.Code != 200 {
		t.Fatalf("connect through shim failed: %d %s\n(if this is a resource-accesses 404, adjust resourceAccessPath's REST version)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, realmHost) || strings.Contains(body, arrayHost) {
		t.Errorf("px must see its realm host name, not the array host name: %s", body)
	}

	// Verify on the array that the volume is actually connected to the array-level host.
	st, body, err := sess.do(http.MethodGet, "/api/2.2/connections?"+qs("host_names", arrayHost, "volume_names", vol), nil)
	if err != nil || st != 200 {
		t.Fatalf("verify connection on array: status=%d err=%v body=%s", st, err, body)
	}
	if !strings.Contains(string(body), vol) {
		t.Fatalf("array does not show the volume connected to %s: %s", arrayHost, body)
	}
	t.Logf("OK: %s is connected to array host %s (via the shim's realm-access grant + rewrite)", vol, arrayHost)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
