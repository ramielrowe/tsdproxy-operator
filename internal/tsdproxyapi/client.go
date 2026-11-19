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

// Package tsdproxyapi reads proxy status from a tsdproxy instance's REST API.
package tsdproxyapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// StateRunning is the state tsdproxy reports for a serving proxy.
const StateRunning = "Running"

// ErrNotFound means tsdproxy does not list the proxy. tsdproxy lists a proxy
// from the moment it loads it from the proxy list, and removes it again when
// the proxy fails to start, so this covers both "not loaded yet" and "failed
// and dropped".
var ErrNotFound = errors.New("proxy not found in tsdproxy")

// ProxyStatus is the subset of tsdproxy's proxy representation the operator
// reports.
type ProxyStatus struct {
	// Status is tsdproxy's proxy state, for example Running, Authenticating
	// or AuthFailed.
	Status string `json:"status"`
	// Health is the backend health: healthy, down or unknown.
	Health string `json:"health"`
	// URL is the tailnet URL, or the login URL while authenticating.
	URL string `json:"url"`
}

// Client reads proxy status from a tsdproxy instance.
type Client interface {
	// GetProxy returns the status of the proxy with the given tailnet
	// hostname, or ErrNotFound.
	GetProxy(ctx context.Context, baseURL, apiKey, hostname string) (*ProxyStatus, error)
}

const (
	requestTimeout  = 5 * time.Second
	maxResponseSize = 8 << 20

	// missRefetchAfter bounds how stale a cached list may be when it does
	// not contain the proxy asked for. A proxy tsdproxy just loaded should
	// show up quickly, but a burst of lookups for it must not turn into a
	// burst of requests.
	missRefetchAfter = 2 * time.Second
)

// HTTPClient is the Client talking to a real tsdproxy.
//
// It reads the whole proxy list (GET /api/v1/proxies) and answers lookups
// from it for CacheTTL. Every Proxy of an instance is polled separately, so
// without the cache an instance with N proxies would serve, and log, N
// requests per poll interval instead of one or two.
type HTTPClient struct {
	HTTP     *http.Client
	CacheTTL time.Duration

	now func() time.Time

	mu    sync.Mutex
	lists map[string]*cachedList
}

type cachedList struct {
	apiKey  string
	fetched time.Time
	proxies map[string]ProxyStatus
}

// NewHTTPClient returns a Client that reuses a fetched proxy list for
// cacheTTL. The request timeout is short: status is polled from the
// reconcile loop and must not hold the work queue.
func NewHTTPClient(cacheTTL time.Duration) *HTTPClient {
	return &HTTPClient{
		HTTP:     &http.Client{Timeout: requestTimeout},
		CacheTTL: cacheTTL,
		now:      time.Now,
		lists:    map[string]*cachedList{},
	}
}

// GetProxy implements Client.
func (c *HTTPClient) GetProxy(ctx context.Context, baseURL, apiKey, hostname string) (*ProxyStatus, error) {
	// One lock for all instances: lookups come from a single reconcile
	// worker, and holding it across the request is what collapses
	// concurrent lookups into one fetch.
	c.mu.Lock()
	defer c.mu.Unlock()

	if list, ok := c.lists[baseURL]; ok && list.apiKey == apiKey {
		age := c.now().Sub(list.fetched)
		status, found := list.proxies[hostname]
		switch {
		case found && age < c.CacheTTL:
			return &status, nil
		case !found && age < missRefetchAfter && age < c.CacheTTL:
			return nil, ErrNotFound
		}
	}

	proxies, err := c.fetch(ctx, baseURL, apiKey)
	if err != nil {
		delete(c.lists, baseURL)
		return nil, err
	}
	c.lists[baseURL] = &cachedList{apiKey: apiKey, fetched: c.now(), proxies: proxies}
	c.prune()

	status, found := proxies[hostname]
	if !found {
		return nil, ErrNotFound
	}
	return &status, nil
}

// prune drops lists of instances nobody asked about for a long time, such as
// deleted ones.
func (c *HTTPClient) prune() {
	horizon := 10*c.CacheTTL + time.Minute
	for baseURL, list := range c.lists {
		if c.now().Sub(list.fetched) > horizon {
			delete(c.lists, baseURL)
		}
	}
}

func (c *HTTPClient) fetch(ctx context.Context, baseURL, apiKey string) (map[string]ProxyStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/proxies", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tsdproxy API returned %s", resp.Status)
	}

	var list struct {
		Proxies []struct {
			Name string `json:"name"`
			ProxyStatus
		} `json:"proxies"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode tsdproxy response: %w", err)
	}

	proxies := make(map[string]ProxyStatus, len(list.Proxies))
	for _, p := range list.Proxies {
		proxies[p.Name] = p.ProxyStatus
	}
	return proxies, nil
}
