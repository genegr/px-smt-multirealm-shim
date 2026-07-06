package proxy

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
)

// arrayIdentity makes each realm FQDN look like a *distinct* FlashArray to px. All realm FQDNs
// resolve to the one shim → one physical array, so GET /arrays returns the same array name+id
// for every realm; px then de-duplicates the backends and mis-routes volumes across realms.
// The shim rewrites the array name+id per incoming FQDN — a unique synthetic identity — in
// responses (real→synth) and maps it back in requests (synth→real). This is what makes the
// multi-realm-on-one-array trick actually work end to end.
type arrayIdentity struct {
	rw    *Rewriter
	mu    sync.Mutex
	ready bool
	name  string // real array name, e.g. "flasharray01"
	id    string // real array id (UUID)
}

// ensure lazily discovers the real array name/id via the array-level session (GET /arrays).
func (a *arrayIdentity) ensure() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready || a.rw.arr == nil {
		return
	}
	status, body, err := a.rw.arr.do("GET", "/api/2.36/arrays", nil)
	if err != nil || status != 200 {
		return
	}
	var d struct {
		Items []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &d) == nil && len(d.Items) > 0 {
		a.name, a.id = d.Items[0].Name, d.Items[0].ID
		a.ready = true
		logf("[arrayident] real array name=%s id=%s", a.name, a.id)
	}
}

// synth returns the synthetic array name+id for a realm FQDN. The id keeps the real UUID's
// prefix and encodes an fnv hash of the FQDN in the final 12 hex digits, so it stays a valid,
// stable, per-realm-unique UUID. Returns ok=false until the real identity is known.
func (a *arrayIdentity) synth(fqdn string) (name, id string, ok bool) {
	a.ensure()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ready {
		return "", "", false
	}
	label := fqdn
	if i := strings.IndexByte(fqdn, '.'); i > 0 {
		label = fqdn[:i]
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(fqdn))
	// real id "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"; keep the first 24 chars (through the 4th
	// group + dash) and replace the 12-hex final group with the FQDN hash.
	prefix := a.id
	if len(prefix) >= 24 {
		prefix = prefix[:24]
	}
	return a.name + "-" + label, fmt.Sprintf("%s%012x", prefix, h.Sum64()&0xffffffffffff), true
}

// rewriteResponseBody replaces the real array name/id with this FQDN's synthetic values.
func (a *arrayIdentity) rewriteResponseBody(fqdn string, body []byte) []byte {
	name, id, ok := a.synth(fqdn)
	if !ok {
		return body
	}
	s := string(body)
	s = strings.ReplaceAll(s, `"`+a.id+`"`, `"`+id+`"`)
	s = strings.ReplaceAll(s, `"`+a.name+`"`, `"`+name+`"`)
	return []byte(s)
}

// rewriteRequestString maps this FQDN's synthetic array name/id back to the real ones (for
// query strings or request bodies px sends referencing the array it learned from GET /arrays).
func (a *arrayIdentity) rewriteRequestString(fqdn, s string) string {
	name, id, ok := a.synth(fqdn)
	if !ok || s == "" {
		return s
	}
	s = strings.ReplaceAll(s, id, a.id)
	s = strings.ReplaceAll(s, name, a.name)
	return s
}
