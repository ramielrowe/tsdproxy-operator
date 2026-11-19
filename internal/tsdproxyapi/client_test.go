/*
Copyright 2026 Andrew Melton.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tsdproxyapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A response in the shape tsdproxy's internal/api returns.
const oneRunningProxy = `{
  "proxies": [
    {
      "targetId": "jellyfin",
      "status": "Running",
      "health": "healthy",
      "url": "https://jellyfin.example.ts.net",
      "name": "jellyfin",
      "label": "jellyfin",
      "targetProvider": "kubernetes",
      "tailscale": {"ephemeral": false, "runWebClient": false},
      "ports": [{"name": "443/https", "proxyProtocol": "https", "proxyPort": 443}]
    }
  ]
}`

const twoProxies = `{
  "proxies": [
    {"name": "jellyfin", "status": "Running", "health": "healthy", "url": "https://jellyfin.example.ts.net"},
    {"name": "new", "status": "Authenticating", "health": "unknown", "url": "https://login.tailscale.com/a/1"}
  ]
}`

// fakeTSDProxy serves a swappable proxy list and counts requests.
type fakeTSDProxy struct {
	*httptest.Server
	body     atomic.Value
	requests atomic.Int32
}

func newFakeTSDProxy(t *testing.T, body string) *fakeTSDProxy {
	t.Helper()
	f := &fakeTSDProxy{}
	f.body.Store(body)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/api/v1/proxies" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(f.body.Load().(string)))
	}))
	t.Cleanup(f.Close)
	return f
}

// clock is a manually advanced time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestClient(ttl time.Duration) (*HTTPClient, *clock) {
	c := NewHTTPClient(ttl)
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c.now = clk.now
	return c, clk
}

func TestGetProxy(t *testing.T) {
	srv := newFakeTSDProxy(t, oneRunningProxy)
	c, _ := newTestClient(time.Minute)
	ctx := context.Background()

	got, err := c.GetProxy(ctx, srv.URL, "secret", "jellyfin")
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	want := ProxyStatus{Status: StateRunning, Health: "healthy", URL: "https://jellyfin.example.ts.net"}
	if *got != want {
		t.Errorf("GetProxy = %+v, want %+v", *got, want)
	}

	if _, err := c.GetProxy(ctx, srv.URL, "secret", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unlisted proxy: err = %v, want ErrNotFound", err)
	}
}

func TestGetProxyErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("wrong API key", func(t *testing.T) {
		srv := newFakeTSDProxy(t, oneRunningProxy)
		c, _ := newTestClient(time.Minute)
		if _, err := c.GetProxy(ctx, srv.URL, "wrong", "jellyfin"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want a non-NotFound error", err)
		}
	})

	t.Run("undecodable response", func(t *testing.T) {
		srv := newFakeTSDProxy(t, "<html>")
		c, _ := newTestClient(time.Minute)
		if _, err := c.GetProxy(ctx, srv.URL, "secret", "jellyfin"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want a decode error", err)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		srv := newFakeTSDProxy(t, oneRunningProxy)
		srv.Close()
		c, _ := newTestClient(time.Minute)
		if _, err := c.GetProxy(ctx, srv.URL, "secret", "jellyfin"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want a connection error", err)
		}
	})
}

// Every Proxy of an instance is looked up separately; they must share one
// request per TTL.
func TestLookupsShareOneRequestPerTTL(t *testing.T) {
	srv := newFakeTSDProxy(t, twoProxies)
	c, clk := newTestClient(15 * time.Second)
	ctx := context.Background()

	for range 5 {
		for _, name := range []string{"jellyfin", "new"} {
			if _, err := c.GetProxy(ctx, srv.URL, "secret", name); err != nil {
				t.Fatalf("GetProxy(%s): %v", name, err)
			}
		}
		clk.advance(time.Second)
	}
	if got := srv.requests.Load(); got != 1 {
		t.Errorf("%d requests within the TTL, want 1", got)
	}

	clk.advance(15 * time.Second)
	if _, err := c.GetProxy(ctx, srv.URL, "secret", "jellyfin"); err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if got := srv.requests.Load(); got != 2 {
		t.Errorf("%d requests after the TTL passed, want 2", got)
	}
}

// A cached list must not hide a proxy tsdproxy loaded after the fetch for a
// whole TTL, and repeated misses must not each cause a request.
func TestMissRefetchesAfterShortDelay(t *testing.T) {
	srv := newFakeTSDProxy(t, oneRunningProxy)
	c, clk := newTestClient(time.Minute)
	ctx := context.Background()

	for range 3 {
		if _, err := c.GetProxy(ctx, srv.URL, "secret", "new"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if got := srv.requests.Load(); got != 1 {
		t.Errorf("%d requests for repeated misses, want 1", got)
	}

	srv.body.Store(twoProxies)
	clk.advance(missRefetchAfter)
	got, err := c.GetProxy(ctx, srv.URL, "secret", "new")
	if err != nil {
		t.Fatalf("GetProxy after the list changed: %v", err)
	}
	if got.Status != "Authenticating" {
		t.Errorf("status = %q, want Authenticating", got.Status)
	}
	if n := srv.requests.Load(); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
}

func TestCacheIsPerInstanceAndPerAPIKey(t *testing.T) {
	a := newFakeTSDProxy(t, oneRunningProxy)
	b := newFakeTSDProxy(t, `{"proxies": []}`)
	c, _ := newTestClient(time.Minute)
	ctx := context.Background()

	if _, err := c.GetProxy(ctx, a.URL, "secret", "jellyfin"); err != nil {
		t.Fatalf("instance a: %v", err)
	}
	if _, err := c.GetProxy(ctx, b.URL, "secret", "jellyfin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("instance b answered from instance a's list: err = %v", err)
	}

	// A rotated key must be checked against tsdproxy, not the cache.
	if _, err := c.GetProxy(ctx, a.URL, "rotated", "jellyfin"); err == nil {
		t.Error("lookup with a different API key was answered from the cache")
	}
}

func TestFailedFetchDropsTheCache(t *testing.T) {
	srv := newFakeTSDProxy(t, oneRunningProxy)
	c, clk := newTestClient(10 * time.Second)
	ctx := context.Background()

	if _, err := c.GetProxy(ctx, srv.URL, "secret", "jellyfin"); err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	clk.advance(10 * time.Second)
	srv.body.Store("<html>")
	if _, err := c.GetProxy(ctx, srv.URL, "secret", "jellyfin"); err == nil {
		t.Fatal("stale list served after a failed fetch")
	}

	// The next lookup must go to tsdproxy again even without time passing.
	srv.body.Store(oneRunningProxy)
	if _, err := c.GetProxy(ctx, srv.URL, "secret", "jellyfin"); err != nil {
		t.Errorf("GetProxy after recovery: %v", err)
	}
}
