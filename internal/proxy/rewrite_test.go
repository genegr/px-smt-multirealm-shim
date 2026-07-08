package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/genegr/px-smt-multirealm-shim/internal/config"
)

// newMultiTransportRewriter models a host reachable over FC and NVMe-TCP (no iSCSI), with a
// comma-separated (and space-padded) WWN list to exercise CSV parsing.
func newMultiTransportRewriter() *Rewriter {
	return NewRewriter(&config.Config{
		RewriteEnabled: true,
		Hosts: []config.HostMapping{{
			Node: "worker2", ArrayHost: "ocp4-1-worker2",
			WWNs: "21000024ff00aaaa, 21000024ff00bbbb",
			NQNs: "nqn.2014-08.org.nvmexpress:uuid:abc",
		}},
	})
}

func itemInitiators(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Items) != 1 {
		t.Fatalf("unexpected body %s (err=%v)", body, err)
	}
	return payload.Items[0]
}

// A double-prefixed GET for an FC/NVMe host must synthesize a host carrying the configured WWNs
// and NQNs (comma-separated, trimmed) and NOT an empty iqns field.
func TestSynthesizesConfiguredTransports(t *testing.T) {
	rw := newMultiTransportRewriter()
	req := httptest.NewRequest(http.MethodGet, "https://realm/api/2.2/hosts?names=ocp4-1-realm1::ocp4-1-worker2", nil)
	s := rw.getHostSynth(req)
	if s == nil {
		t.Fatal("expected a synthesized host for the double-prefixed name")
	}
	item := itemInitiators(t, s.body)
	if _, ok := item["iqns"]; ok {
		t.Errorf("iqns should be absent for an FC/NVMe-only host: %v", item)
	}
	wwns, _ := item["wwns"].([]any)
	if len(wwns) != 2 || wwns[0] != "21000024ff00aaaa" || wwns[1] != "21000024ff00bbbb" {
		t.Errorf("wwns not parsed/trimmed: %v", item["wwns"])
	}
	if nqns, _ := item["nqns"].([]any); len(nqns) != 1 {
		t.Errorf("nqns: %v", item["nqns"])
	}
}

// PATCH add_wwns / add_nqns on a realm host is faked with a 200 echoing the added transports;
// a body with no add_* initiators passes through (nil).
func TestPatchFakesAllTransports(t *testing.T) {
	rw := newMultiTransportRewriter()
	req := httptest.NewRequest(http.MethodPatch, "https://realm/api/2.2/hosts?names=ocp4-1-realm1::worker2-uid", nil)

	s := rw.patchHostInitiators(req, []byte(`{"add_wwns":["21000024ff00cccc"],"add_nqns":["nqn.x"]}`))
	if s == nil || s.status != http.StatusOK {
		t.Fatalf("expected 200 synth, got %#v", s)
	}
	item := itemInitiators(t, s.body)
	if w, _ := item["wwns"].([]any); len(w) != 1 || w[0] != "21000024ff00cccc" {
		t.Errorf("wwns not echoed: %v", item["wwns"])
	}
	if _, ok := item["nqns"]; !ok {
		t.Errorf("nqns not echoed: %v", item)
	}

	if s := rw.patchHostInitiators(req, []byte(`{"name_only":true}`)); s != nil {
		t.Errorf("PATCH with no add_* initiators should pass through, got %#v", s)
	}
}

// GET /hosts injection fills in every configured transport whose field is empty on the realm host.
func TestInjectAllTransports(t *testing.T) {
	rw := newMultiTransportRewriter()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Request:    httptest.NewRequest(http.MethodGet, "https://realm/api/2.2/hosts", nil),
		Body:       io.NopCloser(strings.NewReader(`{"items":[{"name":"ocp4-1-realm1::worker2-uid","wwns":[],"nqns":[]}]}`)),
	}
	rw.ModifyHostsResponse(resp)
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "21000024ff00aaaa") || !strings.Contains(string(out), "nqn.2014-08.org.nvmexpress:uuid:abc") {
		t.Errorf("initiators not injected: %s", out)
	}
}

// The startup array-admin self-check succeeds when the array returns a session token on login and
// fails loudly otherwise, so a bad SHIM_ARRAY_TOKEN surfaces at boot rather than on first attach.
func TestArraySessionValidate(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-auth-token", "sess-123")
	}))
	defer ok.Close()
	u, _ := url.Parse(ok.URL)
	if err := newArraySession(u, "tok", true).validate(); err != nil {
		t.Errorf("validate should succeed when a token is returned, got %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // no x-auth-token
	}))
	defer bad.Close()
	ub, _ := url.Parse(bad.URL)
	if err := newArraySession(ub, "tok", true).validate(); err == nil {
		t.Error("validate should fail when no x-auth-token is returned")
	}
}

// A wildcard GET /hosts that 400s with "No matching hosts found" is converted to an empty 200 so
// px creates its realm host instead of looping; an unrelated 400 passes through untouched.
func TestEmptyHostsOnNoMatchWildcard(t *testing.T) {
	rw := newTestRewriter()
	mk := func(target, body string) *http.Response {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{},
			Request:    httptest.NewRequest(http.MethodGet, target, nil),
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}

	resp := mk("https://realm/api/2.2/hosts?filter=is_local%3D%27true%27&names=ocp4-1-realm1::*",
		`{"errors":[{"context":"ocp4-1-realm1::*","message":"No matching hosts found."}]}`)
	rw.ModifyHostsResponse(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wildcard no-match not converted, status=%d", resp.StatusCode)
	}
	if out, _ := io.ReadAll(resp.Body); !strings.Contains(string(out), `"items":[]`) {
		t.Errorf("expected empty items, got %s", out)
	}

	pass := mk("https://realm/api/2.2/hosts?names=ocp4-1-realm1::*",
		`{"errors":[{"message":"Some other error."}]}`)
	rw.ModifyHostsResponse(pass)
	if pass.StatusCode != http.StatusBadRequest {
		t.Errorf("unrelated 400 should pass through, got %d", pass.StatusCode)
	}

	// A non-wildcard 400 must also pass through (only list queries get the empty-200 treatment).
	nonWild := mk("https://realm/api/2.2/hosts?names=ocp4-1-realm1::worker1-x",
		`{"errors":[{"message":"No matching hosts found."}]}`)
	rw.ModifyHostsResponse(nonWild)
	if nonWild.StatusCode != http.StatusBadRequest {
		t.Errorf("non-wildcard 400 should pass through, got %d", nonWild.StatusCode)
	}
}

// The legacy singular "iqn" field is still honored when "iqns" is unset.
func TestLegacyIQNFallback(t *testing.T) {
	got := config.HostMapping{LegacyIQN: "iqn.legacy:1"}.Initiators()
	if len(got) != 1 || got[0].Field != "iqns" || got[0].Vals[0] != "iqn.legacy:1" {
		t.Errorf("legacy iqn not folded into iqns: %#v", got)
	}
}

func newTestRewriter() *Rewriter {
	return NewRewriter(&config.Config{
		RewriteEnabled: true,
		Hosts: []config.HostMapping{
			{Node: "worker1", ArrayHost: "ocp4-1-worker1", IQNs: "iqn.1994-05.com.redhat:aaa"},
		},
	})
}

// The bug: a volume-filtered GET /connections leaks the array host name, so px derives a
// double-prefixed name and churns the connection. The fix learns px's realm host name from
// host-filtered requests and maps the array host name back in connection listings.
func TestLearnAndMapConnHostsBack(t *testing.T) {
	rw := newTestRewriter()
	body := []byte(`{"items":[{"host":{"name":"ocp4-1-worker1"},"volume":{"name":"v"}}]}`)

	// Before learning, the shim has no realm host name to map back to → leaves it untouched.
	if got := string(rw.mapConnHostsBack(body)); !strings.Contains(got, `"ocp4-1-worker1"`) {
		t.Fatalf("mapped before learning anything: %s", got)
	}

	// px references its realm host in a host-filtered request → shim learns the mapping.
	rw.learnRealmHost("ocp4-1-realm1::worker1-efd4bfdc-1234")
	got := string(rw.mapConnHostsBack(body))
	if strings.Contains(got, `"ocp4-1-worker1"`) {
		t.Errorf("array host name still leaked: %s", got)
	}
	if !strings.Contains(got, "ocp4-1-realm1::worker1-efd4bfdc-1234") {
		t.Errorf("realm host name not mapped in: %s", got)
	}
}

// With array identity disabled (SHIM_ARRAY_IDENTITY=false → ident nil), the connection-churn fix
// must still map array-level host names back to px's realm host name in volume-filtered listings.
func TestConnMapBackWithoutArrayIdentity(t *testing.T) {
	rw := newTestRewriter() // ArrayIdentity unset → ident nil
	if rw.ident != nil {
		t.Fatal("expected nil array identity for this config")
	}
	rw.learnRealmHost("ocp4-1-realm1::worker1-uid")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Request:    httptest.NewRequest(http.MethodGet, "https://realm/api/2.2/connections?volume_names=v", nil),
		Body:       io.NopCloser(strings.NewReader(`{"items":[{"host":{"name":"ocp4-1-worker1"},"volume":{"name":"v"}}]}`)),
	}
	rw.ModifyResponse("realm1-fa.demo.pure", resp)
	out, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(out), `"ocp4-1-worker1"`) || !strings.Contains(string(out), "ocp4-1-realm1::worker1-uid") {
		t.Errorf("array host name not mapped back with identity off: %s", out)
	}
}

// The double-prefixed form px produces is the symptom, not px's real name — never learn it.
func TestLearnIgnoresDoublePrefix(t *testing.T) {
	rw := newTestRewriter()
	rw.learnRealmHost("ocp4-1-realm1::ocp4-1-worker1")
	if _, ok := rw.byArrayHost.Load("ocp4-1-worker1"); ok {
		t.Fatal("learned the double-prefixed form; would echo px's own bug back to it")
	}
}
