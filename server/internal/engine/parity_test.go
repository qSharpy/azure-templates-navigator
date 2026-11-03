package engine

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Guards against configuration drift: every VS Code setting (and therefore
// every IntelliJ setting, see SettingsParityTest.kt) must map to a field the
// server understands, and vice versa.
func TestSettingsParityWithVSCode(t *testing.T) {
	b, err := os.ReadFile("../../../clients/vscode/package.json")
	if err != nil {
		t.Skip("package.json not found")
	}
	var pkg struct {
		Contributes struct {
			Configuration struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"configuration"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	var vs []string
	for k := range pkg.Contributes.Configuration.Properties {
		k = strings.TrimPrefix(k, "azure-templates-navigator.")
		if !strings.HasPrefix(k, "server.") {
			vs = append(vs, k)
		}
	}
	server := jsonKeys(reflect.TypeOf(Settings{}), "")
	// server-only knobs that are intentionally not exposed in the IDEs
	var exposed []string
	for _, k := range server {
		if k != "repositories.fetchTimeoutSeconds" {
			exposed = append(exposed, k)
		}
	}
	sort.Strings(vs)
	if !reflect.DeepEqual(vs, exposed) {
		t.Errorf("settings drift\nvscode: %v\nserver: %v", vs, exposed)
	}
}

func jsonKeys(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		if f.Type.Kind() == reflect.Struct && f.Type.Name() != "Time" {
			out = append(out, jsonKeys(f.Type, key)...)
		} else {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}
