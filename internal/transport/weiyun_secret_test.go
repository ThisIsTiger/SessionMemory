package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// All credentials and URLs in these tests are synthetic. No external requests.

func TestMCPRedirectDoesNotForwardCredential(t *testing.T) {
	const secret = "SYNTHETIC_MCP_CREDENTIAL"
	seen := make(chan string, 1)
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("WyHeader")
		io.WriteString(w, `{"result":{"content":[{"type":"text","text":"{}"}]}}`)
	}))
	defer destination.Close()
	// Different hostname as well as port: not merely a same-host redirect.
	target := strings.Replace(destination.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	c := NewWeiyunClient(secret)
	c.URL = origin.URL
	err := c.call(context.Background(), "weiyun.list", nil, nil)
	if err == nil {
		t.Fatal("a redirect to another origin must be refused")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the credential: %v", err)
	}
	select {
	case value := <-seen:
		t.Fatalf("the redirect target was contacted with %q", value)
	default:
	}
}

func TestMCPSameOriginRedirectStillWorks(t *testing.T) {
	var hits int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			if r.Header.Get("WyHeader") == "" {
				t.Error("same-origin redirect dropped the header")
			}
			hits++
			io.WriteString(w, `{"result":{"content":[{"type":"text","text":"{}"}]}}`)
			return
		}
		http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c := NewWeiyunClient("synthetic")
	c.URL = server.URL
	if err := c.call(context.Background(), "weiyun.list", nil, nil); err != nil || hits != 1 {
		t.Fatalf("hits=%d err=%v", hits, err)
	}
}

func TestDownloadMayFollowCDNRedirectWithoutToken(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("WyHeader") != "" {
			t.Error("the download request must not carry the MCP token")
		}
		io.WriteString(w, "payload")
	}))
	defer cdn.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(cdn.URL, "127.0.0.1", "localhost", 1), http.StatusFound)
	}))
	defer front.Close()
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"items": []map[string]string{{"https_download_url": front.URL + "/f?sig=x"}}})
		outer, _ := json.Marshal(map[string]any{"result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(inner)}}}})
		w.Write(outer)
	}))
	defer mcp.Close()
	c := NewWeiyunClient("synthetic")
	c.URL = mcp.URL
	data, err := c.download(context.Background(), wyFile{Name: "f", Size: 7})
	if err != nil || string(data) != "payload" {
		t.Fatalf("%q %v", data, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNetworkErrorRedactsSignedDownloadURL(t *testing.T) {
	const secret = "SYNTHETIC_DOWNLOAD_SIGNATURE"
	c := NewWeiyunClient("synthetic-mcp")
	cause := errors.New("synthetic network unavailable")
	c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			inner, _ := json.Marshal(map[string]any{"items": []map[string]string{{"https_download_url": "https://user:pw@download.invalid/key/path?sig=" + secret}}})
			outer, _ := json.Marshal(map[string]any{"result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(inner)}}}})
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(outer)))}, nil
		}
		return nil, cause
	})
	_, err := c.download(context.Background(), wyFile{Name: "vault-key.json"})
	if err == nil {
		t.Fatal("expected a network error")
	}
	for _, leak := range []string{secret, "pw@", "key/path"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("network error contains %q: %v", leak, err)
		}
	}
	if !strings.Contains(err.Error(), "download.invalid") || !errors.Is(err, cause) {
		t.Fatalf("the host and the error class must survive: %v", err)
	}
}

func TestServerErrorRedactsMCPTokenButKeepsQuotaClass(t *testing.T) {
	const secret = "SYNTHETIC_MCP_CREDENTIAL"
	for _, quota := range []bool{false, true} {
		c := NewWeiyunClient(secret)
		var reported error
		c.OnQuota = func(err error) { reported = err }
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			msg := "rejected " + r.Header.Get("WyHeader")
			if quota {
				msg += " 117401 daily quota"
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
		}))
		c.URL = server.URL
		err := c.call(context.Background(), "weiyun.list", nil, nil)
		server.Close()
		if err == nil {
			t.Fatal("expected server error")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("server error reflects the MCP credential (quota=%v): %v", quota, err)
		}
		if errors.Is(err, ErrQuota) != quota || (reported != nil) != quota {
			t.Fatalf("quota classification changed (quota=%v): %v", quota, err)
		}
	}
}
