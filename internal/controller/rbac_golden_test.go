package controller_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/tequila/tq-operator/internal/schemagen"
)

// The RBAC of rung observe, as the markers generate it (config/rbac/observe/role.yaml), is
// the hand-written RBAC of test/golden/rbac-observe.yaml: the same Roles, names, namespaces
// and rules — and nothing the envelope forbids.

type roleKey struct{ Kind, Name, Namespace string }

func loadRoles(t *testing.T, rel string) map[roleKey][]string {
	t.Helper()
	root, err := schemagen.ModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	out := map[roleKey][]string{}
	for _, doc := range bytes.Split(raw, []byte("\n---")) {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if obj.Kind == "" {
			continue
		}
		key := roleKey{obj.Kind, obj.Metadata.Name, obj.Metadata.Namespace}
		if _, dup := out[key]; dup {
			t.Fatalf("%s: %v twice", rel, key)
		}
		out[key] = flatten(obj.Rules)
	}
	return out
}

// flatten expands rules into sorted "<group>/<resource>:<verb>" triples (resourceNames and
// nonResourceURLs included, so nothing can hide in them).
func flatten(rules []rbacv1.PolicyRule) []string {
	var out []string
	for _, r := range rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					triple := fmt.Sprintf("%s/%s:%s", g, res, v)
					if len(r.ResourceNames) > 0 {
						triple += fmt.Sprintf("[%s]", strings.Join(r.ResourceNames, ","))
					}
					out = append(out, triple)
				}
			}
		}
		for _, u := range r.NonResourceURLs {
			for _, v := range r.Verbs {
				out = append(out, "nonResource "+u+":"+v)
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func TestGeneratedRBACIsTheEnvelope(t *testing.T) {
	generated := loadRoles(t, "config/rbac/observe/role.yaml")
	golden := loadRoles(t, "test/golden/rbac-observe.yaml")
	for key, rules := range golden {
		got, ok := generated[key]
		if !ok {
			t.Errorf("the markers generate no %v", key)
			continue
		}
		if !slices.Equal(got, rules) {
			t.Errorf("%v:\n generated %v\n golden    %v", key, got, rules)
		}
	}
	for key := range generated {
		if _, ok := golden[key]; !ok {
			t.Errorf("the markers generate %v, which the golden file does not have", key)
		}
	}
}

func TestRBACHoldsNothingTheEnvelopeForbids(t *testing.T) {
	writes := map[string]bool{
		"estate.tequila.dev/estates/status:update": true,
		"estate.tequila.dev/estates/status:patch":  true,
		"/events:create": true,
		"/events:patch":  true,
	}
	forbidden := []string{"secrets", "configmaps", "pods/exec", "pods/portforward", "pods/log", "pods/attach",
		"serviceaccounts", "serviceaccounts/token", "roles", "rolebindings", "clusterroles", "clusterrolebindings"}
	for key, rules := range loadRoles(t, "config/rbac/observe/role.yaml") {
		for _, r := range rules {
			group, rest, _ := strings.Cut(r, "/")
			resource, verb, _ := strings.Cut(rest, ":")
			if strings.Contains(r, "*") {
				t.Errorf("%v: a wildcard (%s)", key, r)
			}
			if slices.Contains(forbidden, resource) {
				t.Errorf("%v: %s is never granted", key, r)
			}
			if verb != "get" && verb != "list" && verb != "watch" && !writes[r] {
				t.Errorf("%v: %s — rung observe writes only its status and Events", key, r)
			}
			if key.Kind == "ClusterRole" && (group != "" || resource != "nodes") {
				t.Errorf("%v: %s — the only cluster-wide read is Nodes", key, r)
			}
			if writes[r] && key.Namespace != "tq-operator" {
				t.Errorf("%v: %s outside the operator's own namespace", key, r)
			}
		}
	}
}
