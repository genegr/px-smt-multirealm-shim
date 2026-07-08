package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/genegr/px-smt-multirealm-shim/internal/config"
)

// fakeArray is an in-memory stand-in for the FlashArray REST API, modelling just enough of the
// realm/host/connection behaviour to replay the FADA attach sequence offline. Its defining rule
// mirrors the real array: an array-level (non-realm) host may only be connected to a realm volume
// once it has been granted access into that realm via resource-accesses. That grant is exactly
// what the shim must issue — without it the connect is refused as if the shim were not there.
type fakeArray struct {
	mu         sync.Mutex
	hosts      map[string][]string // host name -> WWNs actually stored on the array
	grants     map[string]bool     // "<host>\x00<realm>" -> granted
	conns      map[string]bool     // "<host>\x00<volume>" -> connected
	patchHosts int                 // PATCH /hosts calls that reached the array (should stay 0)
}

func newFakeArray() *fakeArray {
	return &fakeArray{hosts: map[string][]string{}, grants: map[string]bool{}, conns: map[string]bool{}}
}

func (f *fakeArray) hasGrant(host, realm string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grants[host+"\x00"+realm]
}

func (f *fakeArray) hasConn(host, vol string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns[host+"\x00"+vol]
}

func (f *fakeArray) storedWWNs(host string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts[host]
}

func (f *fakeArray) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.patchHosts
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

func (f *fakeArray) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, q := r.URL.Path, r.URL.Query()
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/login"):
		w.Header().Set("x-auth-token", "session-token")
		writeJSON(w, map[string]any{"items": []any{map[string]string{"username": "admin"}}})

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/resource-accesses/batch"):
		var reqs []struct {
			Resource struct{ Name string `json:"name"` } `json:"resource"`
			Scope    struct{ Name string `json:"name"` } `json:"scope"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &reqs)
		for _, x := range reqs {
			f.grants[x.Resource.Name+"\x00"+x.Scope.Name] = true
		}
		writeJSON(w, map[string]any{"items": []any{}})

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/hosts"):
		name := q.Get("names")
		f.hosts[name] = nil // px creates the realm host empty; initiators live on the array host
		writeJSON(w, map[string]any{"items": []any{hostItem(name, nil)}})

	case r.Method == http.MethodPatch && strings.HasSuffix(p, "/hosts"):
		f.patchHosts++ // should never happen for a realm host: the shim fakes these
		writeJSON(w, map[string]any{"items": []any{}})

	case r.Method == http.MethodGet && strings.HasSuffix(p, "/hosts"):
		items := make([]any, 0, len(f.hosts))
		for name, wwns := range f.hosts {
			items = append(items, hostItem(name, wwns))
		}
		writeJSON(w, map[string]any{"items": items})

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/connections"):
		host, vol := q.Get("host_names"), q.Get("volume_names")
		if !strings.Contains(host, "::") && strings.Contains(vol, "::") { // array host + realm volume
			realm := vol[:strings.Index(vol, "::")]
			if !f.grants[host+"\x00"+realm] {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"errors": []any{map[string]string{
					"message": "Host " + host + " does not have access to realm " + realm + ".",
				}}})
				return
			}
		}
		f.conns[host+"\x00"+vol] = true
		writeJSON(w, map[string]any{"items": []any{map[string]any{
			"host": map[string]string{"name": host}, "volume": map[string]string{"name": vol}, "lun": 1,
		}}})

	case r.Method == http.MethodGet && strings.HasSuffix(p, "/connections"):
		writeJSON(w, map[string]any{"items": []any{}})

	default:
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"errors": []any{map[string]string{"message": "unhandled " + r.Method + " " + p}}})
	}
}

func hostItem(name string, wwns []string) map[string]any {
	if wwns == nil {
		wwns = []string{}
	}
	return map[string]any{"name": name, "is_local": true, "wwns": wwns, "iqns": []string{}, "nqns": []string{}}
}

// TestFADAAttachSequenceFC replays the exact Fibre Channel attach sequence Portworx drives on the
// real OCP cluster — create realm host, PATCH its WWNs, list hosts, then connect the realm volume —
// against a mock FlashArray, and asserts the shim's end-to-end behaviour:
//   - px's realm host is created but stays empty of initiators on the array;
//   - the faked PATCH never reaches the array;
//   - the array host's WWNs are injected into px's GET /hosts view;
//   - the connect is preceded by a realm-access grant for the array host (without which the array
//     refuses it), then succeeds;
//   - px only ever sees its own realm host name, never the array host name.
func TestFADAAttachSequenceFC(t *testing.T) {
	const (
		realm     = "it16825251008"
		realmHost = realm + "::worker-0-b99e7284-1d60-4e7e-9559-9764e4b4d1c7"
		arrayHost = "worker-0-hcp01-rn-prod"
		vol       = realm + "::psn03220888::px_2ad8cde2-pvc-cf89f9c6-5054-4606-b618-534a464facbc"
		wwn1      = "10:00:7C:A6:2A:95:07:F6"
		wwn2      = "10:00:7C:A6:2A:95:08:09"
		fqdn      = "it16825251008-hcp01-rn-prod-az1.fa.rn-prod.industrystandard.psn.local"
	)

	fake := newFakeArray()
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	up, _ := url.Parse(srv.URL)

	cfg := &config.Config{
		UpstreamURL:      up,
		InsecureUpstream: true,
		RewriteEnabled:   true,
		MaxLogBodyBytes:  4096,
		ArrayToken:       "array-admin-token",
		Hosts:            []config.HostMapping{{Node: "worker-0", ArrayHost: arrayHost, WWNs: wwn1 + "," + wwn2}},
	}
	handler, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	send := func(method, path string, q url.Values, body string) *httptest.ResponseRecorder {
		target := "https://" + fqdn + path
		if len(q) > 0 {
			target += "?" + q.Encode()
		}
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, r)
		req.Host = fqdn
		req.Header.Set("X-Auth-Token", "px-realm-token")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. px creates its realm host (empty). Passes straight through to the array.
	if rec := send(http.MethodPost, "/api/2.2/hosts", url.Values{"names": {realmHost}}, `{}`); rec.Code != 200 {
		t.Fatalf("create realm host: status %d body %s", rec.Code, rec.Body.String())
	}
	if wwns := fake.storedWWNs(realmHost); len(wwns) != 0 {
		t.Fatalf("realm host must be created empty on the array, got wwns %v", wwns)
	}

	// 2. px tries to add the node's WWNs to the realm host. The shim fakes a 200 and the PATCH
	//    never reaches the array, so the realm host stays empty (its WWNs live on the array host).
	rec := send(http.MethodPatch, "/api/2.2/hosts", url.Values{"names": {realmHost}}, `{"add_wwns":["10007ca62a9507f6","10007ca62a950809"]}`)
	if rec.Code != 200 {
		t.Fatalf("faked PATCH: status %d body %s", rec.Code, rec.Body.String())
	}
	if fake.patchCount() != 0 {
		t.Fatalf("PATCH /hosts reached the array %d time(s); it must be faked by the shim", fake.patchCount())
	}
	if wwns := fake.storedWWNs(realmHost); len(wwns) != 0 {
		t.Fatalf("realm host gained WWNs on the array (%v); it must stay empty", wwns)
	}

	// 3. px lists the realm's hosts. The array returns the realm host empty; the shim injects the
	//    array host's WWNs so px sees its host as already owning them.
	rec = send(http.MethodGet, "/api/2.2/hosts", url.Values{"filter": {"is_local='true'"}, "names": {realm + "::*"}}, "")
	if rec.Code != 200 {
		t.Fatalf("list hosts: status %d body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, wwn1) || !strings.Contains(body, wwn2) {
		t.Fatalf("shim did not inject array-host WWNs into GET /hosts view: %s", body)
	}

	// 4. px connects the realm volume to (what it thinks is) its realm host. The shim rewrites the
	//    connect onto the array host — which first requires granting that host access into the realm.
	rec = send(http.MethodPost, "/api/2.2/connections", url.Values{"host_names": {realmHost}, "volume_names": {vol}}, "")
	if rec.Code != 200 {
		t.Fatalf("connect: status %d body %s", rec.Code, rec.Body.String())
	}
	if !fake.hasGrant(arrayHost, realm) {
		t.Fatal("shim did not grant the array host realm access before connecting")
	}
	if !fake.hasConn(arrayHost, vol) {
		t.Fatal("volume was not connected to the array-level host")
	}
	if body := rec.Body.String(); !strings.Contains(body, realmHost) || strings.Contains(body, arrayHost) {
		t.Fatalf("px must see its realm host name, never the array host name: %s", body)
	}

	// The realm host must still be empty of initiators on the array after the whole sequence.
	if wwns := fake.storedWWNs(realmHost); len(wwns) != 0 {
		t.Fatalf("realm host ended up with WWNs on the array (%v); it must remain empty", wwns)
	}
}

// TestRealmAccessGrantedOnce verifies the resource-access grant is issued at most once per
// (host, realm): a second connect for the same realm host reuses the cached grant.
func TestRealmAccessGrantedOnce(t *testing.T) {
	var grantCalls int
	var mu sync.Mutex
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/login"):
			w.Header().Set("x-auth-token", "s")
			writeJSON(w, map[string]any{"items": []any{}})
		case strings.HasSuffix(r.URL.Path, "/resource-accesses/batch"):
			mu.Lock()
			grantCalls++
			mu.Unlock()
			writeJSON(w, map[string]any{"items": []any{}})
		case strings.HasSuffix(r.URL.Path, "/connections"):
			writeJSON(w, map[string]any{"items": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	up, _ := url.Parse(srv.URL)
	rw := NewRewriter(&config.Config{
		UpstreamURL: up, InsecureUpstream: true, RewriteEnabled: true, ArrayToken: "t",
		Hosts: []config.HostMapping{{Node: "worker-0", ArrayHost: "arr-w0", WWNs: "10:00:00:00:00:00:00:01"}},
	})
	mk := func() *http.Request {
		q := url.Values{"host_names": {"realmX::worker-0-uid"}, "volume_names": {"realmX::pod::vol"}}
		return httptest.NewRequest(http.MethodPost, "https://x/api/2.2/connections?"+q.Encode(), nil)
	}
	for i := 0; i < 3; i++ {
		if s := rw.connection(mk(), nil); s == nil || s.status != 200 {
			t.Fatalf("connect %d: %+v", i, s)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if grantCalls != 1 {
		t.Fatalf("resource-access grant issued %d times, want exactly 1 (cached)", grantCalls)
	}
}
