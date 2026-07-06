package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
)

// sensitiveHeaders are redacted in logs but forwarded upstream unchanged.
var sensitiveHeaders = map[string]bool{
	"api-token":     true,
	"x-auth-token":  true,
	"authorization": true,
	"cookie":        true,
	"set-cookie":    true,
}

func logf(format string, args ...any) {
	log.Printf(format, args...)
}

func logHeaders(id, dir string, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		val := strings.Join(h[k], ", ")
		if sensitiveHeaders[strings.ToLower(k)] {
			val = redact(val)
		}
		logf("[%s] %s   %s: %s", id, dir, k, val)
	}
}

// redact keeps a short fingerprint (length + last 4 chars) so we can correlate tokens across
// requests without exposing them in logs.
func redact(v string) string {
	if v == "" {
		return ""
	}
	tail := v
	if len(v) > 4 {
		tail = v[len(v)-4:]
	}
	return fmt.Sprintf("<redacted len=%d …%s>", len(v), tail)
}

// renderBody returns a loggable representation of a body: JSON is pretty-printed; anything
// else is shown as-is. Output is truncated to max bytes with an explicit marker.
func renderBody(body []byte, contentType string, max int) string {
	out := body
	if strings.Contains(strings.ToLower(contentType), "json") {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err == nil {
			out = pretty.Bytes()
		}
	}
	if max > 0 && len(out) > max {
		return fmt.Sprintf("%s\n… [truncated %d of %d bytes]", out[:max], len(out)-max, len(out))
	}
	return string(out)
}
