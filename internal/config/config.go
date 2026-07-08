// Package config holds px-smt-multirealm-shim runtime configuration, sourced from environment variables
// so it maps cleanly onto a Kubernetes Deployment (env / mounted secret).
package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved shim configuration.
type Config struct {
	// ListenAddr is the host:port the shim serves HTTPS on. Default ":9443".
	ListenAddr string

	// UpstreamURL is the real FlashArray management endpoint, e.g. https://flasharray.example.
	UpstreamURL *url.URL

	// UpstreamHost, when set, overrides the Host header / TLS SNI sent upstream. If empty,
	// the upstream URL host is used. (Kept distinct for the multi-realm phase, where the
	// incoming FQDN differs from what the array expects.)
	UpstreamHost string

	// InsecureUpstream skips TLS verification of the FlashArray cert. FADA/Pure CSI
	// typically does not verify the array cert; default true. Confirm during sniffing.
	InsecureUpstream bool

	// LogBodies enables logging of request/response bodies (JSON pretty-printed). Default true.
	LogBodies bool

	// MaxLogBodyBytes caps how much of each body is logged. Default 64 KiB.
	MaxLogBodyBytes int

	// CertFile / KeyFile point at a mounted server cert. If empty, a self-signed cert is
	// generated at startup (fine for phase-1 sniffing when PX does not verify).
	CertFile string
	KeyFile  string

	// CertSANs are DNS names to place in the generated self-signed cert (the realm FQDNs).
	CertSANs []string

	// RewriteEnabled turns on the FADA host-connection rewrites. When false the shim is a
	// pure logging pass-through. Default true.
	RewriteEnabled bool

	// ArrayToken is the array-level admin API token the shim uses for rewritten calls that
	// must address array-level hosts (which the realm-scoped px token cannot). Sourced from
	// SHIM_ARRAY_TOKEN (mounted secret).
	ArrayToken string

	// Hosts is the static map of pre-provisioned array-level hosts, loaded from the mounted
	// config file (SHIM_CONFIG_FILE). px creates realm hosts named "<realm>::<node>-<random>";
	// the shim matches each to a Host by the stable <node> prefix and rewrites onto ArrayHost.
	Hosts []HostMapping
}

// HostMapping ties a Kubernetes node to its pre-provisioned array-level host and the initiator
// identifiers that host owns, across all supported transports: iSCSI (IQNs), Fibre Channel
// (WWNs), and NVMe-TCP (NQNs). Each field is a comma-separated list, so a host may carry several
// identifiers per transport. A node typically uses one transport, but configuring more than one
// is allowed. Empty transports are simply ignored.
type HostMapping struct {
	Node      string `json:"node"`      // e.g. "worker0" — the stable prefix px uses
	ArrayHost string `json:"arrayHost"` // e.g. "ocp4-1-worker0" — the array-level host
	IQNs      string `json:"iqns"`      // comma-separated iSCSI IQNs owned by ArrayHost
	WWNs      string `json:"wwns"`      // comma-separated FC WWNs owned by ArrayHost
	NQNs      string `json:"nqns"`      // comma-separated NVMe-TCP NQNs owned by ArrayHost

	// LegacyIQN accepts the pre-multitransport single-IQN field ("iqn") and is folded into IQNs
	// if the plural field is unset, so an older config secret keeps working. Deprecated.
	LegacyIQN string `json:"iqn"`
}

// Initiator is one transport's identifiers as they appear on a FlashArray host object. Field is
// the array's host key: "iqns" (iSCSI), "wwns" (FC), or "nqns" (NVMe-TCP).
type Initiator struct {
	Field string
	Vals  []string
}

// Initiators returns the host's configured initiators grouped by transport, in a stable order
// (iSCSI, FC, NVMe). Transports with no configured identifiers are omitted. The legacy singular
// "iqn" is folded in when "iqns" is unset.
func (h HostMapping) Initiators() []Initiator {
	iqns := splitCSV(h.IQNs)
	if len(iqns) == 0 {
		iqns = splitCSV(h.LegacyIQN)
	}
	var out []Initiator
	for _, in := range []Initiator{
		{"iqns", iqns},
		{"wwns", splitCSV(h.WWNs)},
		{"nqns", splitCSV(h.NQNs)},
	} {
		if len(in.Vals) > 0 {
			out = append(out, in)
		}
	}
	return out
}

// sanitizeAPIToken removes every ASCII whitespace/control byte (space, tab, CR, LF, DEL, and other
// control chars) from a FlashArray API token, wherever it appears. Go's net/http rejects a request
// header whose value contains such bytes with "invalid header field value"; a token pasted from a
// file or created via `echo` often carries a trailing newline. Real tokens have no whitespace, so
// stripping is safe and leaves the token intact.
func sanitizeAPIToken(s string) string {
	return strings.Map(func(r rune) rune {
		if r <= 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// splitCSV splits a comma-separated list, trimming whitespace and dropping empty entries.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// shimFile is the on-disk JSON shape of SHIM_CONFIG_FILE (the mounted secret).
type shimFile struct {
	Hosts []HostMapping `json:"hosts"`
}

// FromEnv builds a Config from environment variables with sane defaults.
func FromEnv() (*Config, error) {
	c := &Config{
		ListenAddr:       envDefault("SHIM_LISTEN_ADDR", ":9443"),
		UpstreamHost:     os.Getenv("SHIM_UPSTREAM_HOST"),
		InsecureUpstream: envBool("SHIM_UPSTREAM_INSECURE", true),
		LogBodies:        envBool("SHIM_LOG_BODIES", true),
		MaxLogBodyBytes:  envInt("SHIM_MAX_LOG_BODY", 64*1024),
		CertFile:         os.Getenv("SHIM_CERT_FILE"),
		KeyFile:          os.Getenv("SHIM_KEY_FILE"),
		RewriteEnabled:   envBool("SHIM_REWRITE", true),
		// Sanitize: a secret created with a stray newline/CR/tab (even in the middle) would make Go
		// reject the "api-token" request header ("invalid header field value") on every array login.
		// A FlashArray API token has no whitespace, so we strip every control/whitespace byte.
		ArrayToken: sanitizeAPIToken(os.Getenv("SHIM_ARRAY_TOKEN")),
	}

	if path := os.Getenv("SHIM_CONFIG_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("SHIM_CONFIG_FILE %q: %w", path, err)
		}
		var sf shimFile
		if err := json.Unmarshal(raw, &sf); err != nil {
			return nil, fmt.Errorf("parsing SHIM_CONFIG_FILE %q: %w", path, err)
		}
		c.Hosts = sf.Hosts
	}

	rawUpstream := envDefault("SHIM_UPSTREAM_URL", "https://flasharray.example")
	u, err := url.Parse(rawUpstream)
	if err != nil {
		return nil, fmt.Errorf("SHIM_UPSTREAM_URL %q: %w", rawUpstream, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("SHIM_UPSTREAM_URL %q must be an absolute https URL", rawUpstream)
	}
	c.UpstreamURL = u

	if sans := os.Getenv("SHIM_CERT_SANS"); sans != "" {
		for _, s := range strings.Split(sans, ",") {
			if s = strings.TrimSpace(s); s != "" {
				c.CertSANs = append(c.CertSANs, s)
			}
		}
	}
	return c, nil
}

// LoadOrGenerateCert returns the TLS server certificate, either loaded from CertFile/KeyFile
// or freshly generated and self-signed (valid for the configured SANs plus localhost).
func (c *Config) LoadOrGenerateCert() (tls.Certificate, error) {
	if c.CertFile != "" && c.KeyFile != "" {
		return tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	}
	return generateSelfSigned(c.CertSANs)
}

func generateSelfSigned(sans []string) (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "px-shim"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              append([]string{"localhost", "px-shim"}, sans...),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return tls.X509KeyPair(certPEM, keyPEM)
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
