package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRegressionMalformedDownloadURLRedaction(t *testing.T) {
	for _, stage := range []string{"download-link", "redirect-location"} {
		t.Run(stage, func(t *testing.T) {
			const secret = "SYNTHETIC_SIGNED_DOWNLOAD_SECRET"
			malformed := "https://download.invalid/bad%ZZ?sig=" + secret
			next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", malformed)
				w.WriteHeader(http.StatusFound)
			}))
			defer next.Close()
			link := malformed
			if stage == "redirect-location" {
				link = next.URL + "/file?sig=" + secret
			}
			mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				inner, _ := json.Marshal(map[string]any{"items": []map[string]string{{"https_download_url": link}}})
				outer, _ := json.Marshal(map[string]any{"result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(inner)}}}})
				w.Write(outer)
			}))
			defer mcp.Close()
			c := NewWeiyunClient("synthetic")
			c.URL = mcp.URL
			_, err := c.download(context.Background(), wyFile{Name: "vault-key.json"})
			if err == nil {
				t.Fatal("fixture expected URL error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("URL parsing error still exposes signed download credential")
			}
		})
	}
}

func TestSafeNetworkErrorKeepsClassification(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, &net.DNSError{Name: "SYNTHETIC_SECRET", Err: "lookup failed", IsTimeout: true}} {
		wrapped := &url.Error{Op: "Get", URL: "https://download.invalid/private?sig=SYNTHETIC_SECRET", Err: cause}
		safe := netError(wrapped)
		if !errors.Is(safe, cause) || strings.Contains(safe.Error(), "SYNTHETIC_SECRET") {
			t.Fatalf("cause or redaction lost: %v", safe)
		}
		var network net.Error
		if !errors.As(safe, &network) || network.Timeout() != wrapped.Timeout() {
			t.Fatalf("timeout classification lost: %v", safe)
		}
	}
	c := NewWeiyunClient("synthetic")
	c.URL = "https://mcp.invalid/bad%ZZ?token=SYNTHETIC_ENDPOINT_SECRET"
	err := c.call(context.Background(), "weiyun.list", nil, nil)
	if err == nil || strings.Contains(err.Error(), "SYNTHETIC_ENDPOINT_SECRET") {
		t.Fatal("malformed MCP endpoint was not safely reported")
	}
}

// Positive control: the direct MCP endpoint is never reached at a second host.
func TestRegressionMCPRedirectControl(t *testing.T) {
	seen := make(chan bool, 1)
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen <- true; io.WriteString(w, `{}`) }))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(dst.URL, "127.0.0.1", "localhost", 1), 307)
	}))
	defer src.Close()
	c := NewWeiyunClient("synthetic")
	c.URL = src.URL
	if err := c.call(context.Background(), "weiyun.list", nil, nil); err == nil {
		t.Fatal("redirect accepted")
	}
	select {
	case <-seen:
		t.Fatal("cross-host target reached")
	default:
	}
}
