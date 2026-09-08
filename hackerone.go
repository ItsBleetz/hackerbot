package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type requestLimiter struct {
	mu              sync.Mutex
	lastGeneral     time.Time
	lastReport      time.Time
	lastScope       time.Time
	generalInterval time.Duration
	reportInterval  time.Duration
	scopeInterval   time.Duration
}

func (l *requestLimiter) Wait(ctx context.Context, class requestClass) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	due := l.lastGeneral.Add(l.generalInterval)
	if class == reportRead && l.lastReport.Add(l.reportInterval).After(due) {
		due = l.lastReport.Add(l.reportInterval)
	}
	if class == scopeRead && l.lastScope.Add(l.scopeInterval).After(due) {
		due = l.lastScope.Add(l.scopeInterval)
	}
	wait := time.Until(due)
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	now := time.Now()
	l.lastGeneral = now
	if class == reportRead {
		l.lastReport = now
	}
	if class == scopeRead {
		l.lastScope = now
	}
	return nil
}

type H1Client struct {
	baseURL  string
	base     *url.URL
	username string
	token    string
	http     *http.Client
	limiter  requestLimiter
}

type requestClass uint8

const (
	generalRead requestClass = iota
	reportRead
	scopeRead
)

func newH1Client(cfg Config) *H1Client {
	base, _ := url.Parse(cfg.HackerOneBaseURL)
	client := &http.Client{Timeout: cfg.timeout}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.Method != http.MethodGet || base == nil || !sameOrigin(base, req.URL) {
			return fmt.Errorf("refuse HackerOne redirect outside the configured read-only API origin")
		}
		return nil
	}
	return &H1Client{
		baseURL:  cfg.HackerOneBaseURL,
		base:     base,
		username: cfg.HackerOneUsername,
		token:    cfg.HackerOneAPIToken,
		http:     client,
		limiter: requestLimiter{
			generalInterval: cfg.requestDelay,
			reportInterval:  cfg.reportDelay,
			scopeInterval:   cfg.scopeDelay,
		},
	}
}

func (c *H1Client) request(ctx context.Context, target string, class requestClass) ([]byte, error) {
	resolved, err := c.resolveTarget(target)
	if err != nil {
		return nil, err
	}
	target = resolved.String()
	for attempt := 0; attempt < 6; attempt++ {
		if err := c.limiter.Wait(ctx, class); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(c.username, c.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "hackerbot/"+version)

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt == 5 {
				return nil, fmt.Errorf("GET %s: %w", target, err)
			}
			if err := waitContext(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
				return nil, err
			}
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", target, readErr)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			if attempt == 5 {
				return nil, apiStatusError(target, resp.StatusCode, body)
			}
			delay := retryDelay(resp.Header.Get("Retry-After"), attempt)
			if err := waitContext(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		return nil, apiStatusError(target, resp.StatusCode, body)
	}
	return nil, errors.New("request retries exhausted")
}

func (c *H1Client) resolveTarget(target string) (*url.URL, error) {
	if c.base == nil || c.base.Scheme == "" || c.base.Host == "" {
		return nil, fmt.Errorf("invalid HackerOne API base URL")
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parse HackerOne API URL: %w", err)
	}
	if !parsed.IsAbs() {
		parsed = c.base.ResolveReference(parsed)
	}
	if !sameOrigin(c.base, parsed) {
		return nil, fmt.Errorf("refuse HackerOne request outside configured API origin")
	}
	if !strings.HasPrefix(parsed.EscapedPath(), "/v1/hackers/") {
		return nil, fmt.Errorf("refuse HackerOne request outside the /v1/hackers Hacker API")
	}
	return parsed, nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func apiStatusError(target string, status int, body []byte) error {
	text := strings.TrimSpace(string(body))
	if len(text) > 800 {
		text = text[:800] + "..."
	}
	return fmt.Errorf("GET %s returned HTTP %d: %s", target, status, text)
}

func retryDelay(header string, attempt int) time.Duration {
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return time.Duration(1<<attempt) * time.Second
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type listEnvelope struct {
	Data  []json.RawMessage          `json:"data"`
	Links map[string]json.RawMessage `json:"links"`
}

func (c *H1Client) list(ctx context.Context, path string, class requestClass) ([]json.RawMessage, error) {
	target := path
	var all []json.RawMessage
	for target != "" {
		body, err := c.request(ctx, target, class)
		if err != nil {
			return nil, err
		}
		var page listEnvelope
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode %s: %w", target, err)
		}
		for _, item := range page.Data {
			all = append(all, canonicalRaw(item))
		}
		target = linkValue(page.Links["next"])
	}
	return all, nil
}

func (c *H1Client) listLimited(ctx context.Context, path string, class requestClass, limit int) ([]json.RawMessage, error) {
	if limit <= 0 {
		return nil, nil
	}
	body, err := c.request(ctx, path, class)
	if err != nil {
		return nil, err
	}
	var page listEnvelope
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if len(page.Data) > limit {
		page.Data = page.Data[:limit]
	}
	result := make([]json.RawMessage, 0, len(page.Data))
	for _, item := range page.Data {
		result = append(result, canonicalRaw(item))
	}
	return result, nil
}

func linkValue(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func (c *H1Client) object(ctx context.Context, path string) (json.RawMessage, error) {
	body, err := c.request(ctx, path, generalRead)
	if err != nil {
		return nil, err
	}
	return decodeObject(body, path)
}

func (c *H1Client) Programs(ctx context.Context) ([]json.RawMessage, error) {
	return c.list(ctx, "/v1/hackers/programs?page%5Bsize%5D=100", generalRead)
}

func (c *H1Client) ProgramsLimited(ctx context.Context, limit int) ([]json.RawMessage, error) {
	return c.listLimited(ctx, "/v1/hackers/programs?page%5Bsize%5D="+strconv.Itoa(limit), generalRead, limit)
}

func (c *H1Client) Program(ctx context.Context, handle string) (json.RawMessage, error) {
	return c.object(ctx, "/v1/hackers/programs/"+url.PathEscape(handle))
}

func (c *H1Client) Scopes(ctx context.Context, handle string) ([]json.RawMessage, error) {
	path := "/v1/hackers/programs/" + url.PathEscape(handle) + "/structured_scopes?page%5Bsize%5D=100"
	return c.list(ctx, path, scopeRead)
}

func (c *H1Client) ScopeExclusions(ctx context.Context, handle string) ([]json.RawMessage, error) {
	path := "/v1/hackers/programs/" + url.PathEscape(handle) + "/scope_exclusions"
	return c.list(ctx, path, generalRead)
}

func (c *H1Client) Reports(ctx context.Context) ([]json.RawMessage, error) {
	return c.list(ctx, "/v1/hackers/me/reports?page%5Bsize%5D=100", reportRead)
}

func (c *H1Client) ReportsLimited(ctx context.Context, limit int) ([]json.RawMessage, error) {
	return c.listLimited(ctx, "/v1/hackers/me/reports?page%5Bsize%5D="+strconv.Itoa(limit), reportRead, limit)
}

func (c *H1Client) Report(ctx context.Context, id string) (json.RawMessage, error) {
	body, err := c.request(ctx, "/v1/hackers/reports/"+url.PathEscape(id), reportRead)
	if err != nil {
		return nil, err
	}
	return decodeObject(body, "/v1/hackers/reports/"+url.PathEscape(id))
}

func decodeObject(body []byte, path string) (json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if data, exists := root["data"]; exists {
		if len(data) == 0 || string(data) == "null" {
			return nil, fmt.Errorf("decode %s: data is empty", path)
		}
		return canonicalRaw(data), nil
	}
	if len(root["type"]) > 0 && len(root["attributes"]) > 0 {
		return canonicalRaw(body), nil
	}
	keys := make([]string, 0, len(root))
	for key := range root {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return nil, fmt.Errorf("decode %s: expected a data envelope or resource object; received keys %v", path, keys)
}
