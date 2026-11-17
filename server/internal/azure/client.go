// Package azure talks to Azure DevOps for Phase 2 ("check in Azure DevOps",
// docs/design/phase2-design.md): authentication without personal access
// tokens, the pipeline definitions of a repository, the pipelines preview API
// (which compiles a pipeline like a queued run would, without creating one),
// its error messages, and snapshot commits pushed to custom refs.
//
// It knows nothing about the engine. Every request sends the headers that make
// a bearer token work on both Entra-backed and Microsoft-account-backed
// organizations (X-VSS-ForceMsaPassThrough), and that turn a failed sign-in
// into a 401 instead of a redirect to an HTML page (X-TFS-FedAuthRedirect).
package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Resource is the Azure DevOps application id, the audience of its tokens.
const Resource = "499b84ac-1321-427f-aa17-267ca6975798"

// APIVersion is the REST API version used for every call.
const APIVersion = "7.1"

// DefaultBaseURL hosts every organization. $ATN_AZURE_URL replaces it (tests).
const DefaultBaseURL = "https://dev.azure.com"

// BaseURL is DefaultBaseURL unless $ATN_AZURE_URL is set.
func BaseURL() string {
	if u := strings.TrimRight(os.Getenv("ATN_AZURE_URL"), "/"); u != "" {
		return u
	}
	return DefaultBaseURL
}

// Client calls one organization.
type Client struct {
	Base   string // DefaultBaseURL or a test server
	Org    string
	Tokens TokenSource
	HTTP   *http.Client
	Log    func(string)

	// MaxRetries on 429/503 (default 3); MaxWait caps one Retry-After.
	MaxRetries int
	MaxWait    time.Duration
}

// NewClient returns a client for org with the default base URL.
func NewClient(org string, tokens TokenSource) *Client {
	return &Client{Base: BaseURL(), Org: org, Tokens: tokens, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) logf(format string, args ...interface{}) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

// OrgURL is https://dev.azure.com/{org}.
func (c *Client) OrgURL() string {
	return strings.TrimRight(c.Base, "/") + "/" + url.PathEscape(c.Org)
}

// HTTPError is a failed call.
type HTTPError struct {
	Status  int
	TypeKey string // e.g. PipelineValidationException, InvalidGitVersionSpec
	Message string
}

func (e *HTTPError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("Azure DevOps answered %d: %s", e.Status, msg)
}

// IsAuth reports whether err is a sign-in or permission failure.
func IsAuth(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && (he.Status == 401 || he.Status == 403)
}

// ---------------------------------------------------------------------------
// tenant discovery
// ---------------------------------------------------------------------------

// MSATenant is what an organization backed by Microsoft accounts reports.
const MSATenant = "00000000-0000-0000-0000-000000000000"

var tenants = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// Tenant returns the Entra tenant that issues tokens for the organization:
// "" for one backed by Microsoft accounts (or when it can't tell), so the
// signed-in account's default tenant is used. It asks anonymously: the
// x-vss-resourcetenant header of _apis/connectionData.
func (c *Client) Tenant(ctx context.Context) string {
	key := c.Base + "|" + strings.ToLower(c.Org)
	tenants.Lock()
	t, ok := tenants.m[key]
	tenants.Unlock()
	if ok {
		return t
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.OrgURL()+"/_apis/connectionData", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-TFS-FedAuthRedirect", "Suppress")
	hc := *c.httpClient()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return "" // not cached: try again next time
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	t = strings.TrimSpace(resp.Header.Get("X-Vss-Resourcetenant"))
	if t == MSATenant || !looksLikeGUID(t) {
		t = ""
	}
	tenants.Lock()
	tenants.m[key] = t
	tenants.Unlock()
	return t
}

func looksLikeGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

// Token returns a token for this organization.
func (c *Client) Token(ctx context.Context) (string, error) {
	if c.Tokens == nil {
		return "", errors.New("no way to sign in to Azure DevOps")
	}
	return c.Tokens.Token(ctx, c.Tenant(ctx))
}

// ---------------------------------------------------------------------------
// requests
// ---------------------------------------------------------------------------

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// do sends a request to {org}/{path}. A 2xx answer is decoded into out (when
// not nil); anything else becomes an *HTTPError. 429 and 503 are retried after
// Retry-After. hdr, when not nil, receives the response headers.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out interface{}, hdr *http.Header) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	if query == nil {
		query = url.Values{}
	}
	if v, ok := query["api-version"]; !ok {
		query.Set("api-version", APIVersion)
	} else if len(v) == 0 || v[0] == "" {
		query.Del("api-version") // a resource that takes none (connectionData)
	}
	u := c.OrgURL() + "/" + strings.TrimLeft(path, "/") + "?" + query.Encode()
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	retries := c.MaxRetries
	if retries == 0 {
		retries = 3
	}
	maxWait := c.MaxWait
	if maxWait == 0 {
		maxWait = 60 * time.Second
	}
	hc := *c.httpClient()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-VSS-ForceMsaPassThrough", "true")
		req.Header.Set("X-TFS-FedAuthRedirect", "Suppress")
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("can't reach Azure DevOps: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if hdr != nil {
			*hdr = resp.Header
		}
		if cost := resp.Header.Get("X-RateLimit-Cost"); cost != "" {
			c.logf("%s %s: %d, rate-limit cost %s, remaining %s", method, path, resp.StatusCode, cost, resp.Header.Get("X-RateLimit-Remaining"))
		}
		if (resp.StatusCode == 429 || resp.StatusCode == 503) && attempt < retries {
			wait := retryAfter(resp.Header.Get("Retry-After"), attempt)
			if wait > maxWait {
				wait = maxWait
			}
			c.logf("%s %s: %d, retrying in %s", method, path, resp.StatusCode, wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		ctype := resp.Header.Get("Content-Type")
		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400, resp.StatusCode < 300 && strings.Contains(ctype, "text/html"):
			// a redirect to the sign-in page, or the page itself
			return &HTTPError{Status: 401, Message: "the organization sent a sign-in page instead of an answer"}
		case resp.StatusCode >= 400:
			return errorFrom(resp.StatusCode, data)
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("unexpected answer from Azure DevOps: %v", err)
			}
		}
		return nil
	}
}

func retryAfter(h string, attempt int) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		return 0
	}
	return time.Duration(1<<uint(attempt)) * time.Second
}

func errorFrom(status int, data []byte) *HTTPError {
	var body struct {
		Message string `json:"message"`
		TypeKey string `json:"typeKey"`
	}
	e := &HTTPError{Status: status}
	if json.Unmarshal(data, &body) == nil {
		e.Message, e.TypeKey = body.Message, body.TypeKey
	} else {
		e.Message = strings.TrimSpace(firstLine(string(data)))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300]
		}
	}
	return e
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// who am I
// ---------------------------------------------------------------------------

// User is the signed-in identity, as the organization sees it.
type User struct {
	ID   string `json:"id"`
	Name string `json:"providerDisplayName"`
}

// Me returns the signed-in user. It is also the cheapest way to check that a
// token works for the organization.
func (c *Client) Me(ctx context.Context) (User, error) {
	var out struct {
		AuthenticatedUser User `json:"authenticatedUser"`
	}
	if err := c.do(ctx, "GET", "_apis/connectionData", url.Values{"api-version": nil}, nil, &out, nil); err != nil {
		return User{}, err
	}
	return out.AuthenticatedUser, nil
}
