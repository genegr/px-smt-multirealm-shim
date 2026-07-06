package proxy

import (
	"strings"
	"testing"

	"github.com/genegr/px-smt-multirealm-shim/internal/config"
)

func newTestRewriter() *Rewriter {
	return NewRewriter(&config.Config{
		RewriteEnabled: true,
		Hosts: []config.HostMapping{
			{Node: "worker1", ArrayHost: "ocp4-1-worker1", IQN: "iqn.1994-05.com.redhat:aaa"},
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

// The double-prefixed form px produces is the symptom, not px's real name — never learn it.
func TestLearnIgnoresDoublePrefix(t *testing.T) {
	rw := newTestRewriter()
	rw.learnRealmHost("ocp4-1-realm1::ocp4-1-worker1")
	if _, ok := rw.byArrayHost.Load("ocp4-1-worker1"); ok {
		t.Fatal("learned the double-prefixed form; would echo px's own bug back to it")
	}
}
