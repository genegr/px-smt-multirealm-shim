package proxy

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// arraySession maintains an authenticated session to the real FlashArray using the shim's
// array-level admin token. It is used for rewritten calls (e.g. connecting a realm/pod volume
// to an array-level host) that px's realm-scoped token cannot make. Login is lazy and the
// x-auth-token is refreshed once on a 401.
type arraySession struct {
	base     *url.URL
	apiToken string
	client   *http.Client

	mu   sync.Mutex
	auth string
}

func newArraySession(base *url.URL, apiToken string, insecure bool) *arraySession {
	return &arraySession{
		base:     base,
		apiToken: apiToken,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}, //nolint:gosec
		},
	}
}

func (s *arraySession) login() error {
	req, _ := http.NewRequest(http.MethodPost, s.base.String()+"/api/2.36/login", nil)
	req.Header.Set("api-token", s.apiToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	tok := resp.Header.Get("x-auth-token")
	if tok == "" {
		return fmt.Errorf("array login failed: %s", resp.Status)
	}
	s.mu.Lock()
	s.auth = tok
	s.mu.Unlock()
	return nil
}

// validate performs a single login so a misconfigured array-admin token surfaces in the logs at
// startup, instead of silently on the first connection rewrite minutes later. The array-admin
// session is mandatory: only an array-level identity can connect a realm/pod volume to an
// array-level host, so a failure here means connection rewrites will not work.
func (s *arraySession) validate() error { return s.login() }

func (s *arraySession) token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth
}

// do issues method+path (path is the full "/api/<ver>/..." incl. query) with the array
// session token, logging in first if needed and retrying once on 401. Returns status+body.
func (s *arraySession) do(method, path string, body []byte) (int, []byte, error) {
	if s.token() == "" {
		if err := s.login(); err != nil {
			return 0, nil, err
		}
	}
	call := func() (*http.Response, error) {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, _ := http.NewRequest(method, s.base.String()+path, r)
		req.Header.Set("x-auth-token", s.token())
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return s.client.Do(req)
	}
	resp, err := call()
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		if err := s.login(); err != nil {
			return 0, nil, err
		}
		if resp, err = call(); err != nil {
			return 0, nil, err
		}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}
