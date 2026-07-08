package config

import "testing"

// A secret created with a trailing newline must not reach the "api-token" HTTP header verbatim
// (Go rejects it as an invalid header value), so FromEnv trims SHIM_ARRAY_TOKEN.
func TestArrayTokenSanitized(t *testing.T) {
	// Leading/trailing whitespace AND an embedded CR/LF must all be stripped.
	t.Setenv("SHIM_ARRAY_TOKEN", "  abc-123\r\n-token\n")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.ArrayToken != "abc-123-token" {
		t.Errorf("ArrayToken = %q, want sanitized %q", c.ArrayToken, "abc-123-token")
	}
}

// Comma-separated initiators parse and trim per transport; legacy singular iqn is honored.
func TestHostMappingInitiators(t *testing.T) {
	h := HostMapping{IQNs: "iqn.a, iqn.b", WWNs: " ", NQNs: "nqn.x"}
	got := h.Initiators()
	if len(got) != 2 { // iqns (2 vals) + nqns; wwns is whitespace-only → dropped
		t.Fatalf("expected 2 transports, got %d: %#v", len(got), got)
	}
	if got[0].Field != "iqns" || len(got[0].Vals) != 2 || got[0].Vals[1] != "iqn.b" {
		t.Errorf("iqns not parsed/trimmed: %#v", got[0])
	}
	if got[1].Field != "nqns" {
		t.Errorf("expected nqns second, got %q", got[1].Field)
	}

	legacy := HostMapping{LegacyIQN: "iqn.legacy"}.Initiators()
	if len(legacy) != 1 || legacy[0].Field != "iqns" || legacy[0].Vals[0] != "iqn.legacy" {
		t.Errorf("legacy iqn not folded: %#v", legacy)
	}
}
