package azure

import (
	"context"
	"os"
	"strconv"
	"testing"
)

// TestLive calls a real organization with an az token. It only runs when
// ATN_LIVE_ORG, ATN_LIVE_PROJECT, ATN_LIVE_REPO and ATN_LIVE_PIPELINE are set,
// e.g. ATN_LIVE_ORG=myorg ATN_LIVE_PROJECT=p ATN_LIVE_REPO=r ATN_LIVE_PIPELINE=2
// go test ./internal/azure -run TestLive -v
func TestLive(t *testing.T) {
	org, project, repo := os.Getenv("ATN_LIVE_ORG"), os.Getenv("ATN_LIVE_PROJECT"), os.Getenv("ATN_LIVE_REPO")
	pipe, _ := strconv.Atoi(os.Getenv("ATN_LIVE_PIPELINE"))
	if org == "" || project == "" || repo == "" || pipe == 0 {
		t.Skip("set ATN_LIVE_ORG, ATN_LIVE_PROJECT, ATN_LIVE_REPO and ATN_LIVE_PIPELINE")
	}
	ctx := context.Background()
	c := NewClient(org, Chain{EnvToken{}, &AzCLI{}})
	c.Log = func(s string) { t.Log(s) }
	t.Logf("tenant %q", c.Tenant(ctx))
	u, err := c.Me(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("signed in as %s", u.Name)
	r, err := c.Repository(ctx, project, repo)
	if err != nil {
		t.Fatal(err)
	}
	defs, err := c.Definitions(ctx, project, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		t.Logf("definition %d %q %s", d.ID, d.Name, d.YAMLFilename)
	}
	res, err := c.Preview(ctx, project, pipe, PreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rejected=%v problems=%v finalYaml=%d lines", res.Rejected, res.Problems, len(SplitLines(res.FinalYAML)))
}
