package engine

import (
	"strings"
	"testing"
)

// Completing a template path also adds the template's required parameters,
// in a `parameters:` block that is a sibling of `template:`.
func TestTemplatePathCompletionAddsRequiredParameters(t *testing.T) {
	root, e := newWorkspace(t)
	write(t, root, "templates/deploy.yml", `
parameters:
  - name: env
    type: string
    values: [dev, prod]
  - name: dryRun
    type: boolean
  - name: steps
    type: stepList
  - name: region
    default: westeurope
steps: ${{ parameters.steps }}
`)
	p := write(t, root, "c.yml", "steps:\n  - template: \n")
	e.Rescan()

	find := func(items []CompletionItem, label string) CompletionItem {
		t.Helper()
		for _, it := range items {
			if it.Label == label {
				return it
			}
		}
		t.Fatalf("no %q in %+v", label, items)
		return CompletionItem{}
	}

	// list item: entries go under the key column (after "- ")
	it := find(e.Complete(p, 1, 14), "templates/deploy.yml")
	wantPlain := "templates/deploy.yml\n    parameters:\n      env: ''\n      dryRun: true\n      steps: []"
	if it.InsertText != wantPlain {
		t.Errorf("plain:\n%q\nwant:\n%q", it.InsertText, wantPlain)
	}
	// snippet: relative to the line's own indentation (2), which the client adds back
	wantSnippet := "templates/deploy.yml\n  parameters:\n    env: ${1|dev,prod|}\n    dryRun: ${2|true,false|}\n    steps: ${3:[]}$0"
	if it.Snippet != wantSnippet {
		t.Errorf("snippet:\n%q\nwant:\n%q", it.Snippet, wantSnippet)
	}
	if !strings.Contains(it.Detail, "adds 3 required") {
		t.Errorf("detail: %q", it.Detail)
	}

	// extends: `template:` is a plain key, so the block sits at its column
	e.OpenDocument(p, "extends:\n  template: \n")
	it = find(e.Complete(p, 1, 12), "templates/build.yml")
	if want := "templates/build.yml\n  parameters:\n    project: ''"; it.InsertText != want {
		t.Errorf("extends plain:\n%q\nwant:\n%q", it.InsertText, want)
	}
	// `template:` starts the line here, so the snippet's block starts at relative 0
	if want := "templates/build.yml\nparameters:\n  project: ${1:''}$0"; it.Snippet != want {
		t.Errorf("extends snippet:\n%q\nwant:\n%q", it.Snippet, want)
	}

	// deeper list item (the reported case): snippet indentation stays relative
	e.OpenDocument(p, "stages:\n  - stage: A\n    jobs:\n      - template: \n")
	it = find(e.Complete(p, 3, 18), "templates/build.yml")
	if want := "templates/build.yml\n        parameters:\n          project: ''"; it.InsertText != want {
		t.Errorf("nested plain:\n%q\nwant:\n%q", it.InsertText, want)
	}
	if want := "templates/build.yml\n  parameters:\n    project: ${1:''}$0"; it.Snippet != want {
		t.Errorf("nested snippet:\n%q\nwant:\n%q", it.Snippet, want)
	}
	e.OpenDocument(p, "extends:\n  template: \n")

	// a template with nothing required: path only
	write(t, root, "templates/none.yml", "parameters:\n  - name: a\n    default: x\nsteps: []\n")
	e.Rescan()
	it = find(e.Complete(p, 1, 12), "templates/none.yml")
	if it.InsertText != "templates/none.yml" || it.Snippet != "" {
		t.Errorf("no required parameters: %+v", it)
	}

	// the call already has parameters (after or before the template line)
	for _, doc := range []string{
		"steps:\n  - template: \n    parameters:\n      project: x\n",
		"steps:\n  - parameters:\n      project: x\n    template: \n",
	} {
		e.OpenDocument(p, doc)
		line := 1
		if strings.HasPrefix(doc, "steps:\n  - parameters") {
			line = 3
		}
		col := len(strings.Split(doc, "\n")[line])
		it = find(e.Complete(p, line, col), "templates/build.yml")
		if it.InsertText != "templates/build.yml" || it.Snippet != "" {
			t.Errorf("existing parameters (%q): %+v", doc, it)
		}
	}

	// setting off
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Completion.RequiredParameters = false
	e.SetSettings(s)
	e.OpenDocument(p, "steps:\n  - template: \n")
	it = find(e.Complete(p, 1, 14), "templates/build.yml")
	if it.InsertText != "templates/build.yml" || it.Snippet != "" {
		t.Errorf("setting off: %+v", it)
	}
}
