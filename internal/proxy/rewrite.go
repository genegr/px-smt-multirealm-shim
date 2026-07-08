package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/genegr/px-smt-multirealm-shim/internal/config"
)

// Rewriter implements the FADA host-connection rewrites. px creates realm hosts named
// "<realm>::<node>-<random>" and tries to give them the node's initiators — which fails because
// those initiators already belong to a pre-provisioned array-level host. Initiators span every
// supported transport: iSCSI (IQNs), Fibre Channel (WWNs), and NVMe-TCP (NQNs). The Rewriter maps
// every realm host px references to its array-level host (matched by the stable <node> prefix
// from a static config) and:
//   - injects the array host's initiators into GET /hosts responses (so px sees its realm host as
//     already having them and stops the create/patch retry loop),
//   - fakes PATCH …add_iqns / add_wwns / add_nqns with a synthetic 200,
//   - rewrites POST/DELETE /connections onto the array-level host, issued with the array-level
//     token (px's realm token cannot address that host).
//
// See the README for the px↔array flow & shim rewrite design.
type Rewriter struct {
	cfg    *config.Config
	byNode map[string]config.HostMapping
	arr    *arraySession
	ident  *arrayIdentity
	// byArrayHost maps an array-level host name (e.g. "ocp4-1-worker1") to the realm host name
	// px currently uses for it (e.g. "ocp4-1-realm1::worker1-<uid>"), learned from px's own
	// host-filtered requests. Needed to map the array host name back to px's name in
	// connection listings px filters by volume, where the shim otherwise cannot know px's
	// random realm-host suffix. Without this, px sees the array host name, derives a
	// double-prefixed name, and churns the connection (delete+reconnect) — reassigning the LUN
	// each cycle until the multipath device goes read-only and FADA writes are lost.
	byArrayHost sync.Map // string -> string
}

func NewRewriter(cfg *config.Config) *Rewriter {
	byNode := make(map[string]config.HostMapping, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		byNode[h.Node] = h
	}
	var arr *arraySession
	if cfg.ArrayToken != "" {
		arr = newArraySession(cfg.UpstreamURL, cfg.ArrayToken, cfg.InsecureUpstream)
	}
	logf("[rewrite] enabled=%v hosts=%d arrayToken=%v", cfg.RewriteEnabled, len(byNode), arr != nil)
	rw := &Rewriter{cfg: cfg, byNode: byNode, arr: arr}
	rw.ident = &arrayIdentity{rw: rw}
	return rw
}

// RewriteRequest maps this FQDN's synthetic array identity back to the real array id/name in
// the request URL query (px learned the synthetic id from GET /arrays). Called before proxying.
func (rw *Rewriter) RewriteRequest(fqdn string, r *http.Request) {
	if !rw.cfg.RewriteEnabled || rw.ident == nil {
		return
	}
	if rq := r.URL.RawQuery; rq != "" {
		if nrq := rw.ident.rewriteRequestString(fqdn, rq); nrq != rq {
			r.URL.RawQuery = nrq
		}
	}
}

// ModifyResponse applies response-body rewrites: inject host initiators into GET /hosts, then map
// the real array identity to this FQDN's synthetic one so px sees each realm as a distinct array.
func (rw *Rewriter) ModifyResponse(fqdn string, resp *http.Response) {
	rw.ModifyHostsResponse(resp)
	if !rw.cfg.RewriteEnabled || rw.ident == nil || resp.Body == nil {
		return
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(strings.NewReader(""))
		return
	}
	out := rw.ident.rewriteResponseBody(fqdn, body)
	// Map array-level host names back to px's realm host names in connection listings px filters
	// by volume. connection() only maps host-filtered queries (where px supplied its own name);
	// a volume-only GET /connections otherwise leaks the array host name and px churns the
	// connection under a double-prefixed name (reassigning the LUN → read-only device → data loss).
	if resp.Request != nil && resp.Request.Method == http.MethodGet && strings.HasSuffix(resp.Request.URL.Path, "/connections") {
		out = rw.mapConnHostsBack(out)
	}
	resp.Body = io.NopCloser(strings.NewReader(string(out)))
	resp.ContentLength = int64(len(out))
	resp.Header.Del("Content-Length")
}

type synth struct {
	status int
	body   []byte
}

// isRealmHost reports whether a host name is realm-scoped (contains the "::" realm separator).
func isRealmHost(name string) bool { return strings.Contains(name, "::") }

// resolve maps a realm host name "<realm>::<node>-<suffix>" to its array-level HostMapping by
// matching the stable <node> prefix. Also defensively matches a double-prefixed form
// "<realm>::<arrayHost>" (which px can produce if it ever records the array host name).
// Returns nil for array-level hosts or unknown nodes.
func (rw *Rewriter) resolve(realmHost string) *config.HostMapping {
	i := strings.Index(realmHost, "::")
	if i < 0 {
		return nil
	}
	after := realmHost[i+2:] // "<node>-<suffix>", "<node>", or "<arrayHost>"
	for node := range rw.byNode {
		h := rw.byNode[node]
		if after == node || strings.HasPrefix(after, node+"-") || after == h.ArrayHost {
			return &h
		}
	}
	return nil
}

// learnRealmHost records that px is using realmHost for its array-level host, so the shim can
// map the array host name back to px's own name in connection listings px filters by volume.
// The double-prefixed form "<realm>::<arrayHost>" is ignored — it is px's *symptom*, not the
// name we want to echo back.
func (rw *Rewriter) learnRealmHost(realmHost string) {
	hm := rw.resolve(realmHost)
	if hm == nil {
		return
	}
	i := strings.Index(realmHost, "::")
	if i < 0 || realmHost[i+2:] == hm.ArrayHost {
		return // array-level or double-prefixed form: not px's realm host name
	}
	rw.byArrayHost.Store(hm.ArrayHost, realmHost)
}

// mapConnHostsBack rewrites every learned array-level host name to px's realm host name in a
// connection-listing body, so px only ever sees its own names.
func (rw *Rewriter) mapConnHostsBack(body []byte) []byte {
	s := string(body)
	rw.byArrayHost.Range(func(k, v any) bool {
		s = strings.ReplaceAll(s, `"`+k.(string)+`"`, `"`+v.(string)+`"`)
		return true
	})
	return []byte(s)
}

var connCounter atomic.Uint64

// Intercept short-circuits requests the shim answers itself (PATCH host add_iqns) or that must
// be re-issued to the array-level host with the array token (POST/DELETE connections). Returns
// nil to let the request proxy through untouched.
func (rw *Rewriter) Intercept(r *http.Request, body []byte) *synth {
	if !rw.cfg.RewriteEnabled {
		return nil
	}
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/hosts"):
		return rw.getHostSynth(r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/hosts"):
		return rw.patchHostInitiators(r, body)
	case strings.HasSuffix(r.URL.Path, "/connections"):
		// GET/POST/DELETE connections that reference a realm host must be mapped onto the
		// array-level host (and the response mapped back), so px only ever sees its own name.
		return rw.connection(r, body)
	}
	return nil
}

// getHostSynth handles `GET /hosts?names=<realm>::<arrayHost>` — the double-prefixed form px
// produces during FADA attach after learning the array-level host name. That host does not
// exist on the array (400), so we synthesize it as an existing host owning the node IQN. Only
// the double-prefixed form is synthesized; real realm hosts and wildcard/list queries pass
// through (the latter handled by ModifyHostsResponse).
func (rw *Rewriter) getHostSynth(r *http.Request) *synth {
	names := r.URL.Query().Get("names")
	if names == "" || strings.ContainsAny(names, "*,") {
		return nil
	}
	hm := rw.resolve(names)
	if hm == nil {
		return nil
	}
	i := strings.Index(names, "::")
	if i < 0 || names[i+2:] != hm.ArrayHost {
		return nil // real realm host (e.g. <realm>::worker0-<uid>): pass through + inject
	}
	item := map[string]any{"name": names, "connection_count": 1, "host_group": map[string]any{"name": nil}}
	for _, in := range hm.Initiators() {
		item[in.Field] = in.Vals
	}
	resp, _ := json.Marshal(map[string]any{"items": []map[string]any{item}})
	logf("[rewrite] synthesized GET host (double-prefix) %s initiators=%v", names, hm.Initiators())
	return &synth{status: http.StatusOK, body: resp}
}

// patchHostInitiators fakes `PATCH /hosts?names=<realm-host>` add_iqns/add_wwns/add_nqns with a
// synthetic 200 (the initiators already live on the array-level host, so the real PATCH would
// 400). It echoes back whichever transports px asked to add.
func (rw *Rewriter) patchHostInitiators(r *http.Request, body []byte) *synth {
	name := r.URL.Query().Get("names")
	if !isRealmHost(name) {
		return nil
	}
	var b struct {
		AddIQNs []string `json:"add_iqns"`
		AddWWNs []string `json:"add_wwns"`
		AddNQNs []string `json:"add_nqns"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil
	}
	item := map[string]any{"name": name}
	for _, f := range []config.Initiator{
		{Field: "iqns", Vals: b.AddIQNs},
		{Field: "wwns", Vals: b.AddWWNs},
		{Field: "nqns", Vals: b.AddNQNs},
	} {
		if len(f.Vals) > 0 {
			item[f.Field] = f.Vals
		}
	}
	if len(item) == 1 { // only "name": nothing to add
		return nil
	}
	resp, _ := json.Marshal(map[string]any{"items": []map[string]any{item}})
	logf("[rewrite] faked PATCH add-initiators %s -> 200", name)
	return &synth{status: http.StatusOK, body: resp}
}

// connection makes GET/POST/DELETE /connections transparent when host_names is a realm host:
// it rewrites host_names onto the array-level host, issues the call with the array-level token
// (px's realm token cannot address that host), and rewrites the array host name back to the
// realm host name in the response so px only ever sees its own name. A POST that the array
// reports as already existing is treated as success (idempotent, for px retries).
func (rw *Rewriter) connection(r *http.Request, body []byte) *synth {
	q := r.URL.Query()
	hostNames := q.Get("host_names")
	if hostNames == "" || !isRealmHost(hostNames) {
		return nil // no realm host referenced: pass through
	}
	hm := rw.resolve(hostNames)
	if hm == nil {
		logf("[rewrite] connection: no mapping for realm host %q, passing through", hostNames)
		return nil
	}
	rw.learnRealmHost(hostNames) // remember px's name for this array host
	if rw.arr == nil {
		logf("[rewrite] connection: array token not configured, cannot rewrite %q", hostNames)
		return nil
	}
	q.Set("host_names", hm.ArrayHost)
	path := r.URL.Path + "?" + q.Encode()
	id := connCounter.Add(1)
	status, respBody, err := rw.arr.do(r.Method, path, nilIfEmpty(body))
	if err != nil {
		logf("[rewrite] connection#%d %s %s -> upstream error: %v", id, r.Method, hostNames, err)
		return &synth{status: http.StatusBadGateway, body: []byte(`{"errors":[{"message":"px-smt-multirealm-shim connection rewrite error"}]}`)}
	}
	// Idempotency: a duplicate POST connection ("already exists") is fine for px retries.
	if r.Method == http.MethodPost && status == http.StatusBadRequest && strings.Contains(strings.ToLower(string(respBody)), "already") {
		logf("[rewrite] connection#%d POST %s already connected -> treating as 200", id, hostNames)
		status = http.StatusOK
		respBody, _ = json.Marshal(map[string]any{"items": []map[string]any{{"host": map[string]string{"name": hostNames}, "volume": map[string]string{"name": q.Get("volume_names")}}}})
		return &synth{status: status, body: respBody}
	}
	// Map the array host name back to the realm host name px used, so px stays consistent.
	respBody = []byte(strings.ReplaceAll(string(respBody), `"`+hm.ArrayHost+`"`, `"`+hostNames+`"`))
	logf("[rewrite] connection#%d %s realm-host=%s -> array-host=%s vols=%s => %d",
		id, r.Method, hostNames, hm.ArrayHost, q.Get("volume_names"), status)
	return &synth{status: status, body: respBody}
}

// ModifyHostsResponse injects the mapped array-level initiators (IQNs/WWNs/NQNs) into realm hosts
// in GET /hosts responses, so px sees its realm host as already owning them (skipping the failing
// add_iqns/add_wwns/add_nqns). It rewrites resp.Body in place; a no-op for non-/hosts or non-GET
// responses.
func (rw *Rewriter) ModifyHostsResponse(resp *http.Response) {
	if !rw.cfg.RewriteEnabled || resp.Request == nil || resp.Request.Method != http.MethodGet {
		return
	}
	if !strings.HasSuffix(resp.Request.URL.Path, "/hosts") {
		return
	}
	// Purity returns 400 "No matching hosts found" when a names=<realm>::* filter matches zero
	// hosts, instead of an empty 200. px treats that 400 as fatal and loops forever instead of
	// creating its realm host. For a wildcard host-list query, rewrite it to an empty 200 (the
	// truthful answer for a list that matched nothing) so px falls through to create the host.
	if resp.StatusCode == http.StatusBadRequest {
		rw.emptyHostsIfNoMatch(resp)
		return
	}
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(strings.NewReader(""))
		return
	}
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	// Preserve unknown top-level fields by decoding into a generic map too.
	var generic map[string]any
	if json.Unmarshal(body, &generic) != nil || json.Unmarshal(body, &payload) != nil {
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		resp.ContentLength = int64(len(body))
		return
	}
	changed := false
	items, _ := generic["items"].([]any)
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if hm := rw.resolve(name); hm != nil {
			rw.learnRealmHost(name) // GET /hosts lists px's realm hosts — freshest source
			for _, in := range hm.Initiators() {
				if cur, _ := m[in.Field].([]any); len(cur) == 0 {
					m[in.Field] = in.Vals
					changed = true
				}
			}
		}
	}
	out := body
	if changed {
		if b, err := json.Marshal(generic); err == nil {
			out = b
			logf("[rewrite] injected initiators into GET %s response", resp.Request.URL.RequestURI())
		}
	}
	resp.Body = io.NopCloser(strings.NewReader(string(out)))
	resp.ContentLength = int64(len(out))
	resp.Header.Del("Content-Length")
}

// emptyHostsIfNoMatch converts a 400 "No matching hosts found" on a wildcard GET /hosts list into
// an empty 200, so px sees "zero realm hosts" and creates one instead of erroring in a loop. Only
// wildcard (names=…*) queries are touched; any other 400 body is passed through untouched.
func (rw *Rewriter) emptyHostsIfNoMatch(resp *http.Response) {
	names := resp.Request.URL.Query().Get("names")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(names, "*") && strings.Contains(strings.ToLower(string(body)), "no matching hosts") {
		const empty = `{"continuation_token":null,"items":[],"more_items_remaining":false,"total_item_count":null}`
		resp.StatusCode = http.StatusOK
		resp.Status = "200 OK"
		resp.Body = io.NopCloser(strings.NewReader(empty))
		resp.ContentLength = int64(len(empty))
		resp.Header.Del("Content-Length")
		resp.Header.Set("Content-Type", "application/json")
		logf("[rewrite] converted 400 no-matching-hosts -> empty 200 for %s", resp.Request.URL.RequestURI())
		return
	}
	resp.Body = io.NopCloser(strings.NewReader(string(body)))
	resp.ContentLength = int64(len(body))
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// writeSynth emits a shim-generated response and logs it in the <<< format.
func writeSynth(cfg *config.Config, id string, w http.ResponseWriter, r *http.Request, s *synth) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s.status)
	_, _ = io.WriteString(w, string(s.body))
	logf("[%s] <<< %s %s -> %d (shim)", id, r.Method, r.URL.Path, s.status)
	if cfg.LogBodies && len(s.body) > 0 {
		logf("[%s] <<< body: %s", id, renderBody(s.body, "application/json", cfg.MaxLogBodyBytes))
	}
}
