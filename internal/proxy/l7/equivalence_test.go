package l7

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Sachinxmpl/loadgate/internal/config"
)

type capture struct {
	method  string
	path    string
	query   string
	reqHdr  http.Header
	reqBody []byte

	status   int
	respHdr  http.Header
	respBody []byte
}

// Records each request it receives into *slot (guarded by mu)
// Replies with fixed status/header/body
func captureBackend(t *testing.T, mu *sync.Mutex, slot *capture, status int, respBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		slot.method = r.Method
		slot.path = r.URL.Path
		slot.query = r.URL.RawQuery
		slot.reqHdr = r.Header.Clone()
		slot.reqBody = body
		mu.Unlock()

		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Backend", "seen")
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
}

// Warps httputil.ReverseProxy pointer at target, configured to match our proxy's policy (preserve client's Host)
func referenceProxy(t *testing.T, target string) *httptest.Server {
	t.Helper()

	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			host := pr.In.Host
			prior := pr.In.Header.Get("X-Forwarded-For")

			pr.SetURL(u)
			pr.Out.Host = host

			// Match our proxy's X-Forwarded-For policy: preserve the prior
			// value then append the client IP, rather than replacing it.
			pr.SetXForwarded()
			if prior != "" {
				pr.Out.Header.Set("X-Forwarded-For", prior+", "+pr.Out.Header.Get("X-Forwarded-For"))
			}
		},
	}

	return httptest.NewServer(rp)
}

// send req through frontURL, returning what the backend recorded and what the client received.
// resets slot first so capture is fresh
func sendAndCapture(t *testing.T, frontURL string, mu *sync.Mutex, slot *capture, build func() *http.Request) capture {
	t.Helper()
	mu.Lock()
	*slot = capture{}
	mu.Unlock()

	req := build()

	front, _ := url.Parse(frontURL)
	req.URL.Scheme, req.URL.Host = front.Scheme, front.Host
	req.RequestURI = ""

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		t.Fatalf("client Do : %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	mu.Lock()
	got := *slot
	mu.Unlock()
	got.status = resp.StatusCode
	got.respHdr = resp.Header.Clone()
	got.respBody = respBody
	return got
}

var comparedReqHeaders = []string{"X-Test", "Content-Type", "X-Forwarded-For"}

var mustBeStripped = []string{"Connection", "Keep-Alive", "X-Hop-Named"}

func compareForwarding(t *testing.T, name string, ours, ref capture, compareBody bool) {
	t.Helper()

	if ours.method != ref.method {
		t.Errorf("%s: method ours=%q ref=%q", name, ours.method, ref.method)
	}
	if ours.path != ref.path || ours.query != ref.query {
		t.Errorf("%s: url ours=%q?%q ref=%q?%q", name, ours.path, ours.query, ref.path, ref.query)
	}
	if compareBody && !bytes.Equal(ours.reqBody, ref.reqBody) {
		t.Errorf("%s: request body differs (%d vs %d bytes)", name, len(ours.reqBody), len(ref.reqBody))
	}

	for _, h := range comparedReqHeaders {
		o, r := headerOrEmpty(ours.reqHdr, h), headerOrEmpty(ref.reqHdr, h)
		if h == "X-Forwarded-For" {
			if firstXFF(o) != firstXFF(r) {
				t.Errorf("%s: X-Forwarded-For prior ours=%q ref=%q", name, o, r)
			}
			continue
		}
		if o != r {
			t.Errorf("%s: request header %s ours=%q ref=%q", name, h, o, r)
		}
	}

	for _, h := range mustBeStripped {
		if v := headerOrEmpty(ours.reqHdr, h); v != "" {
			t.Errorf("%s: hop-by-hop %s leaked through our proxy: %q", name, h, v)
		}
		if v := headerOrEmpty(ref.reqHdr, h); v != "" {
			t.Errorf("%s: hop-by-hop %s leaked through reference: %q", name, h, v)
		}
	}

	if ours.status != ref.status {
		t.Errorf("%s: status ours=%d ref=%d", name, ours.status, ref.status)
	}
	if compareBody && !bytes.Equal(ours.respBody, ref.respBody) {
		t.Errorf("%s: response body differs", name)
	}
	if headerOrEmpty(ours.respHdr, "X-Backend") != headerOrEmpty(ref.respHdr, "X-Backend") {
		t.Errorf("%s: X-Backend differs", name)
	}
}

func headerOrEmpty(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	return h.Get(key)
}

func firstXFF(v string) string {
	if v == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(v, ",")[0])
}

func TestEquivalence_WithReverseProxy(t *testing.T) {
	var mu sync.Mutex
	var slot capture

	backend := captureBackend(t, &mu, &slot, http.StatusOK, "backend-body-content")
	t.Cleanup(backend.Close)
	backendAddr := strings.TrimPrefix(backend.URL, "http://")

	our := newL7(t, &config.Config{
		Mode:   config.ModeL7,
		Listen: "127.0.0.1:0",
		// the test client is local; trusting it matches ReverseProxy, which keeps any prior X-Forwarded-For
		TrustedProxies: []config.CIDR{{Prefix: netip.MustParsePrefix("127.0.0.1/32")}},
		Routes:         []config.Route{{Match: config.RouteMatch{PathPrefix: "/"}, Pool: "p"}},
		Pools:          map[string][]config.Backend{"p": {{Addr: backendAddr, Weight: 1}}},
	})
	ourURL := "http://" + our.Addr().String()

	ref := referenceProxy(t, backend.URL)
	t.Cleanup(ref.Close)

	cases := []struct {
		name        string
		build       func() *http.Request
		compareBody bool
	}{
		{
			name: "normal GET",
			build: func() *http.Request {
				r, _ := http.NewRequest("GET", "/path/here?x=1&y=2", nil)
				r.Header.Set("X-Test", "value")
				return r
			},
			compareBody: true,
		},
		{
			name: "POST with body",
			build: func() *http.Request {
				r, _ := http.NewRequest("POST", "/submit", strings.NewReader("the request body"))
				r.Header.Set("Content-Type", "text/plain")
				return r
			},
			compareBody: true,
		},
		{
			name: "hop-by-hop stripped",
			build: func() *http.Request {
				r, _ := http.NewRequest("GET", "/", nil)
				r.Header.Set("Connection", "X-Hop-Named")
				r.Header.Set("X-Hop-Named", "leak-me")
				r.Header.Set("Keep-Alive", "timeout=5")
				return r
			},
			compareBody: true,
		},
		{
			name: "pre-existing X-Forwarded-For",
			build: func() *http.Request {
				r, _ := http.NewRequest("GET", "/", nil)
				r.Header.Set("X-Forwarded-For", "198.51.100.9")
				return r
			},
			compareBody: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ours := sendAndCapture(t, ourURL, &mu, &slot, tc.build)
			refCap := sendAndCapture(t, ref.URL, &mu, &slot, tc.build)
			compareForwarding(t, tc.name, ours, refCap, tc.compareBody)
		})
	}
}

func TestEquivalence_BackendErrorStatus(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		var mu sync.Mutex
		var slot capture
		backend := captureBackend(t, &mu, &slot, status, "err-body")
		backendAddr := strings.TrimPrefix(backend.URL, "http://")

		our := newL7(t, &config.Config{
			Mode:   config.ModeL7,
			Listen: "127.0.0.1:0",
			Health: config.Health{Passive: config.PassiveHealth{Fall: 1}},
			Routes: []config.Route{{Match: config.RouteMatch{PathPrefix: "/"}, Pool: "p"}},
			Pools:  map[string][]config.Backend{"p": {{Addr: backendAddr, Weight: 1}}},
		})

		resp := mustGet(t, our, "/")
		defer resp.Body.Close()
		if resp.StatusCode != status {
			t.Errorf("status %d passed through as %d", status, resp.StatusCode)
		}
		if !our.registry.Get(backendAddr).Admits() {
			t.Errorf("backend evicted for returning %d", status)
		}
		backend.Close()
	}
}
