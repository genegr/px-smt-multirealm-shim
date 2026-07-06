// Package proxy implements the phase-1 transparent logging reverse proxy that sits between
// Portworx and the FlashArray. Every request is forwarded byte-faithfully; the request and
// response (including bodies) are logged for analysis. No rewriting happens here yet.
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"

	"github.com/genegr/px-smt-multirealm-shim/internal/config"
)

type ctxKey int

const (
	reqIDKey ctxKey = 0
	hostKey  ctxKey = 1 // original incoming Host FQDN (before Director rewrites it)
)

// New builds the http.Handler for the shim: an httputil.ReverseProxy wired to the upstream
// FlashArray, wrapped with request/response logging.
func New(cfg *config.Config) (http.Handler, error) {
	rewriter := NewRewriter(cfg)
	rp := &httputil.ReverseProxy{
		Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: cfg.InsecureUpstream}, //nolint:gosec // FA cert is typically unverified in these deployments
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 5 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		Director: func(req *http.Request) {
			req.URL.Scheme = cfg.UpstreamURL.Scheme
			req.URL.Host = cfg.UpstreamURL.Host
			// The array expects to be addressed as itself, not as the shim's FQDN.
			if cfg.UpstreamHost != "" {
				req.Host = cfg.UpstreamHost
			} else {
				req.Host = cfg.UpstreamURL.Host
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			fqdn, _ := resp.Request.Context().Value(hostKey).(string)
			rewriter.ModifyResponse(fqdn, resp) // GET /hosts IQN injection + per-realm array identity
			logResponse(cfg, resp)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			id, _ := r.Context().Value(reqIDKey).(string)
			logf("[%s] UPSTREAM ERROR %s %s: %v", id, r.Method, r.URL.Path, err)
			http.Error(w, "px-smt-multirealm-shim upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := nextID()
		fqdn := r.Host // the realm FQDN px addressed us as; captured before the Director rewrites it
		ctx := context.WithValue(r.Context(), reqIDKey, id)
		ctx = context.WithValue(ctx, hostKey, fqdn)
		r = r.WithContext(ctx)
		body := drainBody(r)
		logRequest(cfg, id, r, body)
		if s := rewriter.Intercept(r, body); s != nil {
			writeSynth(cfg, id, w, r, s)
			return
		}
		rewriter.RewriteRequest(fqdn, r) // map synthetic array id/name back to real in the query
		rp.ServeHTTP(w, r)
	}), nil
}

// drainBody reads the full request body into memory and restores it so the proxy can still
// forward it. FlashArray REST bodies are small JSON, so buffering is fine.
func drainBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return body
}

var counter atomic.Uint64

func nextID() string {
	return fmt.Sprintf("r%06d", counter.Add(1))
}

// logRequest logs the incoming request. The body has already been read and restored by the
// caller (drainBody), so it is passed in here rather than consumed again.
func logRequest(cfg *config.Config, id string, r *http.Request, body []byte) {
	// The incoming Host (FQDN/SNI) is the realm selector — always log it.
	logf("[%s] >>> %s %s  Host=%s  RemoteAddr=%s", id, r.Method, r.URL.RequestURI(), r.Host, r.RemoteAddr)
	logHeaders(id, ">>>", r.Header)
	if cfg.LogBodies && len(body) > 0 {
		logf("[%s] >>> body: %s", id, renderBody(body, r.Header.Get("Content-Type"), cfg.MaxLogBodyBytes))
	}
}

// logResponse logs the upstream response and restores its body for the client.
func logResponse(cfg *config.Config, resp *http.Response) {
	id, _ := resp.Request.Context().Value(reqIDKey).(string)
	logf("[%s] <<< %s %s -> %s", id, resp.Request.Method, resp.Request.URL.Path, resp.Status)
	logHeaders(id, "<<<", resp.Header)

	if !cfg.LogBodies || resp.Body == nil {
		return
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		logf("[%s] <<< body read error: %v", id, err)
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length") // let the transport recompute for the buffered body
	if len(body) > 0 {
		logf("[%s] <<< body: %s", id, renderBody(body, resp.Header.Get("Content-Type"), cfg.MaxLogBodyBytes))
	}
}
