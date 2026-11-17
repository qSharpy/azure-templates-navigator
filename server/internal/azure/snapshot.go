package azure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Snapshots: Azure reads templates from git at a ref, so local work has to
// reach the server before it can be previewed. A snapshot is one commit whose
// parent is the base and whose tree is the base's with the changed YAML
// files replaced (the text the engine sees, unsaved editor buffers
// included). It is built with a temporary index, so the user's index, working
// tree, branches and stash are never touched, and only those files are
// uploaded. It is pushed to a custom ref (RefPrefix), which shows in no branch
// list and fires no CI trigger.

// RefPrefix is where snapshots are pushed.
const RefPrefix = "refs/atn/preview/"

// FileChange is a file of a snapshot, relative to the repository top
// (forward slashes).
type FileChange struct {
	Path    string
	Deleted bool
	Text    string
}

// Git runs git in one repository.
type Git struct {
	Path string // executable; "git" when empty
	Dir  string // repository top
}

func (g Git) run(ctx context.Context, env []string, stdin string, args ...string) (string, error) {
	path := g.Path
	if path == "" {
		path = "git"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = g.Dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C"), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], lastLines(msg, 3))
	}
	return strings.TrimSpace(out.String()), nil
}

func lastLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " / ")
}

// author is the identity of snapshot commits, so building one works without
// user.name / user.email.
var author = []string{
	"GIT_AUTHOR_NAME=Azure Templates Navigator", "GIT_AUTHOR_EMAIL=atn@localhost",
	"GIT_COMMITTER_NAME=Azure Templates Navigator", "GIT_COMMITTER_EMAIL=atn@localhost",
}

// Snapshot builds the snapshot commit of changes on top of base and returns
// its id. Nothing in the repository changes except new objects.
func (g Git) Snapshot(ctx context.Context, base string, changes []FileChange) (string, error) {
	tmp, err := os.MkdirTemp("", "atn-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := append([]string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}, author...)
	if _, err := g.run(ctx, env, "", "read-tree", base); err != nil {
		return "", err
	}
	for _, c := range changes {
		p := strings.TrimLeft(filepath.ToSlash(c.Path), "/")
		if p == "" || strings.Contains("/"+p+"/", "/../") {
			return "", fmt.Errorf("invalid path in snapshot: %q", c.Path)
		}
		if c.Deleted {
			if _, err := g.run(ctx, env, "", "update-index", "--force-remove", "--", p); err != nil {
				return "", err
			}
			continue
		}
		// --path applies the repository's clean filters (core.autocrlf), as a
		// commit would
		blob, err := g.run(ctx, env, c.Text, "hash-object", "-w", "--stdin", "--path="+p)
		if err != nil {
			return "", err
		}
		if _, err := g.run(ctx, env, "", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+p); err != nil {
			return "", err
		}
	}
	tree, err := g.run(ctx, env, "", "write-tree")
	if err != nil {
		return "", err
	}
	return g.run(ctx, env, "", "commit-tree", tree, "-p", base, "-m", "atn preview snapshot [skip ci]")
}

// OnRemote reports whether a commit is in some remote-tracking branch, i.e.
// most likely already on the server.
func (g Git) OnRemote(ctx context.Context, sha string) bool {
	out, err := g.run(ctx, nil, "", "branch", "-r", "--contains", sha)
	return err == nil && strings.TrimSpace(out) != ""
}

// Push force-pushes refspecs ("<sha>:<ref>", ":<ref>" to delete) to remoteURL.
// With a token, git authenticates with it (through the environment, never
// the command line) instead of the user's credential helper; if that fails
// and fallback is a remote name or URL, the push is tried again there with
// the user's own git credentials.
func (g Git) Push(ctx context.Context, remoteURL, token, fallback string, refspecs ...string) error {
	args := append([]string{"push", "--force", "--no-verify", "--quiet"}, append([]string{""}, refspecs...)...)
	var firstErr error
	if token != "" {
		args[4] = stripUser(remoteURL)
		_, err := g.run(ctx, tokenEnv(remoteURL, token), "", args...)
		if err == nil {
			return nil
		}
		firstErr = err
	}
	if fallback == "" {
		fallback = remoteURL
	}
	args[4] = fallback
	if _, err := g.run(ctx, nil, "", args...); err != nil {
		// the server's own reason (Azure: "TF401027: You need the Git
		// 'CreateBranch' permission…") says it best, once
		if why := RejectedReason(err); why != "" && (firstErr == nil || RejectedReason(firstErr) == why) {
			return fmt.Errorf("the server refused it: %s", why)
		}
		if firstErr != nil {
			return fmt.Errorf("%v (and with your own git credentials: %v)", firstErr, err)
		}
		return err
	}
	return nil
}

// RejectedReason is the server's reason in git push's
// "! [remote rejected] <sha> -> <ref> (<reason>)" line, or "".
func RejectedReason(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	i := strings.Index(s, "[remote rejected]")
	if i < 0 {
		return ""
	}
	s = s[i:]
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return ""
	}
	depth := 0
	for j := open; j < len(s); j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[open+1 : j])
			}
		}
	}
	return ""
}

// tokenEnv configures git through GIT_CONFIG_COUNT (git 2.31+): no credential
// helper, and the bearer token as an extra header for the server only.
func tokenEnv(remoteURL, token string) []string {
	scope := "http"
	if u, err := url.Parse(remoteURL); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		scope = "http." + u.Scheme + "://" + u.Host + "/"
	}
	kv := [][2]string{
		{"credential.helper", ""},
		{scope + ".extraHeader", "Authorization: Bearer " + token},
		{scope + ".extraHeader", "X-VSS-ForceMsaPassThrough: true"},
	}
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(kv))}
	for i, p := range kv {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]))
	}
	return env
}

// stripUser drops "user@" from an https URL (Azure puts the organization
// there), so git doesn't look for that user's password.
func stripUser(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || (u.Scheme != "https" && u.Scheme != "http") {
		return raw
	}
	u.User = nil
	return u.String()
}

var unsafeRef = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// RefName is the snapshot ref of a user (their id, shortened) and branch.
func RefName(userID, branch string) string {
	u := strings.ToLower(unsafeRef.ReplaceAllString(userID, ""))
	if len(u) > 8 {
		u = u[:8]
	}
	if u == "" {
		u = "user"
	}
	b := strings.Trim(unsafeRef.ReplaceAllString(branch, "-"), "-.")
	for strings.Contains(b, "..") {
		b = strings.ReplaceAll(b, "..", ".")
	}
	if strings.HasSuffix(b, ".lock") {
		b = strings.TrimSuffix(b, ".lock") + "-lock"
	}
	if b == "" {
		b = "detached"
	}
	return RefPrefix + u + "/" + b
}

// ErrNoBase is returned when a repository has no commit to build on.
var ErrNoBase = errors.New("the repository has no commits")
