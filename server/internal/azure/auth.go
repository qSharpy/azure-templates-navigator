package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenSource gives bearer tokens for Azure DevOps. tenant is the Entra
// tenant that owns the organization, or "" to use the signed-in account's
// default (organizations backed by Microsoft accounts).
type TokenSource interface {
	Token(ctx context.Context, tenant string) (string, error)
}

// Chain tries each source in turn and returns the first token. The error is
// the last source's, which is the one the user can act on (az).
type Chain []TokenSource

func (c Chain) Token(ctx context.Context, tenant string) (string, error) {
	err := errors.New("no way to sign in to Azure DevOps")
	for _, s := range c {
		t, e := s.Token(ctx, tenant)
		if e == nil {
			return t, nil
		}
		err = e
	}
	return "", err
}

// EnvToken reads a bearer token from the environment: ATN_AZURE_TOKEN, else
// SYSTEM_ACCESSTOKEN (an Azure Pipelines job's token, for CI).
type EnvToken struct{}

func (EnvToken) Token(context.Context, string) (string, error) {
	for _, k := range []string{"ATN_AZURE_TOKEN", "SYSTEM_ACCESSTOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v, nil
		}
	}
	return "", errors.New("no token in ATN_AZURE_TOKEN or SYSTEM_ACCESSTOKEN")
}

// Static is a fixed token (tests).
type Static string

func (s Static) Token(context.Context, string) (string, error) { return string(s), nil }

// AzCLI gets tokens from the Azure CLI: `az account get-access-token`. They
// are cached per tenant until five minutes before they expire.
type AzCLI struct {
	Path string // executable; "az" when empty

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// LoginHint is the command that signs in to az for a tenant.
func LoginHint(tenant string) string {
	if tenant == "" {
		return "az login --allow-no-subscriptions"
	}
	return "az login --tenant " + tenant + " --allow-no-subscriptions"
}

func (a *AzCLI) Token(ctx context.Context, tenant string) (string, error) {
	a.mu.Lock()
	if c, ok := a.cache[tenant]; ok && time.Until(c.expires) > 5*time.Minute {
		a.mu.Unlock()
		return c.token, nil
	}
	a.mu.Unlock()

	path := a.Path
	if path == "" {
		path = "az"
	}
	args := []string{"account", "get-access-token", "--resource", Resource, "-o", "json"}
	if tenant != "" {
		args = append(args, "--tenant", tenant)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || os.IsNotExist(err) {
			return "", fmt.Errorf("the Azure CLI (az) is not installed or not on the PATH; install it, run `%s`, or set azure.azPath", LoginHint(tenant))
		}
		msg := strings.TrimSpace(stderr.String())
		if i := strings.Index(msg, "\n"); i > 0 {
			msg = msg[:i]
		}
		msg = strings.TrimPrefix(msg, "ERROR: ")
		return "", fmt.Errorf("az couldn't get a token (%s); run `%s`", msg, LoginHint(tenant))
	}
	var resp struct {
		AccessToken string      `json:"accessToken"`
		ExpiresOn   string      `json:"expiresOn"`
		ExpiresOnTS json.Number `json:"expires_on"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil || resp.AccessToken == "" {
		return "", fmt.Errorf("unexpected output from az account get-access-token")
	}
	exp := time.Now().Add(30 * time.Minute)
	if n, err := strconv.ParseInt(resp.ExpiresOnTS.String(), 10, 64); err == nil && n > 0 {
		exp = time.Unix(n, 0)
	} else if t, err := time.ParseInLocation("2006-01-02 15:04:05.000000", resp.ExpiresOn, time.Local); err == nil {
		exp = t
	}
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]cachedToken{}
	}
	a.cache[tenant] = cachedToken{resp.AccessToken, exp}
	a.mu.Unlock()
	return resp.AccessToken, nil
}

// TokenUser is who a token was issued to (upn, email or name claim), for
// error messages. It never returns the token itself.
func TokenUser(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]interface{}
	if json.Unmarshal(b, &claims) != nil {
		return ""
	}
	for _, k := range []string{"upn", "unique_name", "email", "name"} {
		if s, ok := claims[k].(string); ok && s != "" {
			s = strings.TrimPrefix(s, "live.com#")
			return s
		}
	}
	return ""
}
