package azure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/qSharpy/azure-templates-navigator/server/internal/azure/azuretest"
)

func fakeClient(t *testing.T) (*azuretest.Server, *Client) {
	t.Helper()
	s := azuretest.New()
	t.Cleanup(s.Close)
	c := NewClient("fabrikam", Static("tok"))
	c.Base = s.URL
	return s, c
}

func TestHeadersAndMe(t *testing.T) {
	s, c := fakeClient(t)
	u, err := c.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "Test User" || u.ID == "" {
		t.Fatalf("user = %+v", u)
	}
	call := s.Calls()[len(s.Calls())-1]
	for k, want := range map[string]string{
		"Authorization":             "Bearer tok",
		"X-Vss-Forcemsapassthrough": "true",
		"X-Tfs-Fedauthredirect":     "Suppress",
	} {
		if got := call.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if call.Query != "" {
		t.Errorf("connectionData takes no api-version, got %q", call.Query)
	}
	if _, err := c.Repository(context.Background(), "personal", "atn-consumer"); err != nil {
		t.Fatal(err)
	}
	if q := s.Calls()[len(s.Calls())-1].Query; !strings.Contains(q, "api-version=7.1") {
		t.Errorf("query %q has no api-version", q)
	}
}

func TestTenantDiscovery(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"00000000-0000-0000-0000-000000000000", ""},
		{"72f988bf-86f1-41af-91ab-2d7cd011db47", "72f988bf-86f1-41af-91ab-2d7cd011db47"},
		{"", ""},
	} {
		s, c := fakeClient(t)
		s.Tenant = tc.header
		if got := c.Tenant(context.Background()); got != tc.want {
			t.Errorf("tenant header %q: got %q, want %q", tc.header, got, tc.want)
		}
		// anonymous: no token was sent
		if a := s.Calls()[0].Header.Get("Authorization"); a != "" {
			t.Errorf("tenant discovery sent a token")
		}
	}
}

func TestUnauthorizedAndRedirect(t *testing.T) {
	s, c := fakeClient(t)
	s.Token = "other"
	_, err := c.Me(context.Background())
	if !IsAuth(err) || !strings.Contains(err.Error(), "TF400813") {
		t.Fatalf("err = %v", err)
	}
	// a redirect to the sign-in page (no pass-through header on an MSA org) is a 401
	he := (&HTTPError{Status: 401})
	if !IsAuth(he) {
		t.Fatal("401 is not auth")
	}
}

func TestRetryOn429(t *testing.T) {
	s, c := fakeClient(t)
	s.Throttle = 2
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Throttle = 5
	c.MaxRetries = 2
	if _, err := c.Me(context.Background()); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want 429", err)
	}
}

func TestDefinitionsPagingAndMatch(t *testing.T) {
	s, c := fakeClient(t)
	s.PageSize = 1
	s.Defs = append(s.Defs, azuretest.Def{ID: 7, Name: "pr", Project: "personal", YAMLFilename: "/Azure-Pipelines.yml", RepoID: s.Defs[0].RepoID})
	defs, err := c.Definitions(context.Background(), "personal", s.Defs[0].RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 3 {
		t.Fatalf("got %d definitions, want 3: %+v", len(defs), defs)
	}
	m := MatchDefinitions(defs, "azure-pipelines.yml")
	if len(m) != 2 || m[0].ID != 2 || m[1].ID != 7 {
		t.Fatalf("matches = %+v", m)
	}
	if m := MatchDefinitions(defs, "ci-default.yml"); len(m) != 1 || m[0].ID != 3 {
		t.Fatalf("ci-default matches = %+v", m)
	}
	r, err := c.Repository(context.Background(), "personal", "atn-consumer")
	if err != nil || r.ID != s.Defs[0].RepoID || !strings.Contains(r.RemoteURL, "/_git/atn-consumer") {
		t.Fatalf("repository = %+v, %v", r, err)
	}
}

func TestPreviewResults(t *testing.T) {
	s, c := fakeClient(t)
	answer := ""
	s.Preview = func(string, int, map[string]interface{}) (int, []byte) { return azuretest.PreviewFixture(answer) }
	ctx := context.Background()

	answer = "A_empty"
	r, err := c.Preview(ctx, "personal", 2, PreviewRequest{Repositories: map[string]RepoOverride{"self": {RefName: "refs/atn/preview/x", Version: "abc"}}})
	if err != nil || r.Rejected || !strings.Contains(r.FinalYAML, "tpl-version: v1") {
		t.Fatalf("A: %+v %v", r, err)
	}
	body := s.Previews()[0].Body
	want := map[string]interface{}{"previewRun": true, "resources": map[string]interface{}{"repositories": map[string]interface{}{"self": map[string]interface{}{"refName": "refs/atn/preview/x", "version": "abc"}}}}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %v", body)
	}

	answer = "T2_tpl_error_inside_template"
	r, err = c.Preview(ctx, "personal", 2, PreviewRequest{})
	if err != nil || !r.Rejected || len(r.Problems) != 1 || r.Problems[0].Alias != "templates" || r.Problems[0].Line != 15 {
		t.Fatalf("T2: %+v %v", r, err)
	}
	for _, name := range []string{"F_orphan_sha_on_main", "K_tpl_alias_missing_branch"} {
		answer = name
		if _, err := c.Preview(ctx, "personal", 2, PreviewRequest{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := c.Preview(ctx, "personal", 99, PreviewRequest{}); err == nil {
		t.Error("unknown pipeline: no error")
	}
}

func TestParseProblems(t *testing.T) {
	msg := func(name string) string {
		var d struct{ Message string }
		_ = json.Unmarshal(azuretest.Fixture("preview/"+name+".json"), &d)
		return d.Message
	}
	cases := map[string][]Problem{
		"T2_tpl_error_inside_template": {{AzurePath: "/templates/stages.yml@templates", Path: "templates/stages.yml", Alias: "templates", Line: 15, Col: 7, Message: "Unexpected value 'scriptt'"}},
		"L_override_bad_param":         {{AzurePath: "/azure-pipelines.yml", Path: "azure-pipelines.yml", Line: 25, Col: 13, Message: "Unexpected parameter 'colour'"}},
		"L2_override_bad_yaml": {
			{AzurePath: "/azure-pipelines.yml", Path: "azure-pipelines.yml", Line: 21, Col: 5, Message: "Unexpected value ':'"},
			{AzurePath: "/azure-pipelines.yml", Path: "azure-pipelines.yml", Line: 22, Col: 1, Message: "While parsing a node, did not find expected node content."},
		},
		"R4_dependsOn_missing": {{Message: "Stage local depends on unknown stage nosuchstage."}},
		"H_params_invalid":     {{Message: "The 'env' parameter value 'staging' is not a valid value."}},
		"K_tpl_alias_missing_branch": {{AzurePath: "/azure-pipelines.yml", Path: "azure-pipelines.yml",
			Message: "Could not get the latest source version for repository atn-templates hosted on Azure Repos using ref refs/heads/does-not-exist."}},
	}
	for name, want := range cases {
		if got := ParseProblems(msg(name)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
	if got := ParseProblems(msg("R1_resources_missing")); len(got) != 2 || got[0].AzurePath != "" || !strings.HasPrefix(got[1].Message, "A task is missing") {
		t.Errorf("R1: %+v", got)
	}
	if got := ParseProblems(msg("R8_runtime_expr_typo")); len(got) != 1 || !strings.HasPrefix(got[0].Message, "Unclosed function") {
		t.Errorf("R8: %+v", got)
	}
	// keys ignore positions
	a := ParseProblems("/a.yml (Line: 3, Col: 1): Unexpected value 'x'")[0]
	b := ParseProblems("/a.yml (Line: 9, Col: 4): Unexpected value 'x'")[0]
	if a.Key() != b.Key() {
		t.Error("keys differ by position")
	}
}

func TestAzCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	az := filepath.Join(dir, "az")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n" +
		"case \"$*\" in *nologin*) echo \"ERROR: Please run 'az login' to setup account.\" >&2; exit 1;; esac\n" +
		"echo '{\"accessToken\": \"eyJ0.eyJ1cG4iOiJ0ZXN0QGV4YW1wbGUuY29tIn0.sig\", \"expiresOn\": \"2099-01-01 00:00:00.000000\", \"expires_on\": 4070908800}'\n"
	if err := os.WriteFile(az, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &AzCLI{Path: az}
	ctx := context.Background()
	tok, err := a.Token(ctx, "")
	if err != nil || !strings.HasPrefix(tok, "eyJ0.") {
		t.Fatalf("token %q, %v", tok, err)
	}
	if TokenUser(tok) != "test@example.com" {
		t.Errorf("TokenUser = %q", TokenUser(tok))
	}
	_, _ = a.Token(ctx, "") // cached
	if _, err := a.Token(ctx, "72f988bf-86f1-41af-91ab-2d7cd011db47"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("az ran %d times, want 2 (cache): %q", len(lines), lines)
	}
	if strings.Contains(lines[0], "--tenant") || !strings.Contains(lines[1], "--tenant 72f988bf") || !strings.Contains(lines[0], "--resource "+Resource) {
		t.Errorf("args: %q", lines)
	}
	if _, err := a.Token(ctx, "nologin"); err == nil || !strings.Contains(err.Error(), "az login --tenant nologin") {
		t.Errorf("not signed in: %v", err)
	}
	if _, err := (&AzCLI{Path: filepath.Join(dir, "missing")}).Token(ctx, ""); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("missing az: %v", err)
	}
}

func TestEnvTokenFirst(t *testing.T) {
	t.Setenv("ATN_AZURE_TOKEN", "")
	t.Setenv("SYSTEM_ACCESSTOKEN", "job-token")
	tok, err := Chain{EnvToken{}, &AzCLI{Path: "/nonexistent/az"}}.Token(context.Background(), "")
	if err != nil || tok != "job-token" {
		t.Fatalf("%q %v", tok, err)
	}
	t.Setenv("SYSTEM_ACCESSTOKEN", "")
	if _, err := (Chain{EnvToken{}, &AzCLI{Path: "/nonexistent/az"}}).Token(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "az") {
		t.Fatalf("want the az error, got %v", err)
	}
}

func TestDiff(t *testing.T) {
	a := SplitLines("a\nb\nc\nd\ne\n")
	b := SplitLines("a\nc\nd\nX\ne\nf\n")
	ops := Diff(a, b)
	var ra, rb []string
	for _, o := range ops {
		if o.Kind != '+' {
			ra = append(ra, o.Text)
		}
		if o.Kind != '-' {
			rb = append(rb, o.Text)
		}
	}
	if !reflect.DeepEqual(ra, a) || !reflect.DeepEqual(rb, b) {
		t.Fatalf("ops don't rebuild the inputs: %+v", ops)
	}
	if add, rem := Count(ops); add != 2 || rem != 1 {
		t.Fatalf("count = +%d -%d", add, rem)
	}
	u := Unified(ops, "base", "change", 1)
	want := "--- base\n+++ change\n@@ -1,5 +1,6 @@\n a\n-b\n c\n d\n+X\n e\n+f\n"
	if u != want {
		t.Fatalf("unified:\n%s\nwant:\n%s", u, want)
	}
	if Unified(Diff(a, a), "x", "y", 3) != "" {
		t.Fatal("no changes, but a diff")
	}
	// the recorded A vs C expansions differ in exactly the template marker
	var x, y struct{ FinalYAML string }
	_ = json.Unmarshal(azuretest.Fixture("preview/A_empty.json"), &x)
	_ = json.Unmarshal(azuretest.Fixture("preview/C_tpl_alias_branch.json"), &y)
	if add, rem := Count(Diff(SplitLines(x.FinalYAML), SplitLines(y.FinalYAML))); add != 1 || rem != 1 {
		t.Fatalf("A vs C: +%d -%d", add, rem)
	}
	// completely different inputs
	if add, rem := Count(Diff([]string{"a", "b"}, []string{"c"})); add != 1 || rem != 2 {
		t.Fatalf("+%d -%d", add, rem)
	}
}

func TestRefName(t *testing.T) {
	for _, tc := range []struct{ user, branch, want string }{
		{"3f2a9c1e-7b4d-4e8a", "feature/x", "refs/atn/preview/3f2a9c1e/feature-x"},
		{"", "", "refs/atn/preview/user/detached"},
		{"ABCDEF0123", "a..b.lock", "refs/atn/preview/abcdef01/a.b-lock"},
	} {
		if got := RefName(tc.user, tc.branch); got != tc.want {
			t.Errorf("RefName(%q, %q) = %q, want %q", tc.user, tc.branch, got, tc.want)
		}
	}
}

func TestRejectedReason(t *testing.T) {
	err := errors.New("git push: To https://dev.azure.com/o/p/_git/r /  ! [remote rejected] 1208f93 -> refs/atn/preview/x/main " +
		"(TF401027: You need the Git 'CreateBranch' permission to perform this action. Details: identity 'u (x)', scope 'branch'.) / error: failed to push some refs")
	if got := RejectedReason(err); got != "TF401027: You need the Git 'CreateBranch' permission to perform this action. Details: identity 'u (x)', scope 'branch'." {
		t.Errorf("got %q", got)
	}
	if RejectedReason(errors.New("fatal: could not read Username")) != "" || RejectedReason(nil) != "" {
		t.Error("no reason expected")
	}
}
