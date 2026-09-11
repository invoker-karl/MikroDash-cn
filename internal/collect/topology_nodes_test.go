package collect

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

// TestTopologyNodesAreTheDeclaredKinds holds cmd/tsgen's override for
// TopologyPayload.Nodes to what the code actually puts there.
//
// The field is `[]any`, so its Go type says nothing, and tsgen is TOLD which
// struct kinds it holds so it can generate `nodes` as their union. A told list
// is a claim: a fourth kind appended in topology.go would be a node the
// browser's type does not describe, and nothing would fail. So this builds the
// payload from the captured router — which has neighbours and clients as well
// as itself — and requires the kinds present to be exactly the declared ones,
// in both directions.
func TestTopologyNodesAreTheDeclaredKinds(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "tsgen", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	entry := regexp.MustCompile(`"collect\.TopologyPayload\.Nodes":\s*\{([^}]*)\}`).FindStringSubmatch(string(src))
	if entry == nil {
		t.Fatal("cmd/tsgen no longer overrides TopologyPayload.Nodes — this check is reading nothing")
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`"collect\.(\w+)"`).FindAllStringSubmatch(entry[1], -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("the override names no kinds")
	}

	captures, _ := filepath.Glob(filepath.Join(testdata, "fixtures", "*", "topology.json"))
	if len(captures) == 0 {
		t.Fatal("no topology capture to build the payload from")
	}
	found := map[string]bool{}
	for _, path := range captures {
		var f fixture
		readJSON(t, path, &f)
		p, _ := ported["topology"](newReplayReader(f), Emit{}).(*TopologyPayload)
		if p == nil {
			t.Fatalf("%s produced no topology payload", path)
		}
		for _, n := range p.Nodes {
			typ := reflect.TypeOf(n)
			for typ.Kind() == reflect.Pointer {
				typ = typ.Elem()
			}
			found[typ.Name()] = true
		}
	}

	for k := range found {
		if !declared[k] {
			t.Errorf("TopologyPayload.Nodes holds a %s, and cmd/tsgen's override does not declare it — "+
				"the browser's type for a node does not describe that kind", k)
		}
	}
	for k := range declared {
		if !found[k] {
			t.Errorf("cmd/tsgen declares %s as a node kind and the capture produced none — either the "+
				"union is wider than the code, or the capture no longer exercises that kind", k)
		}
	}
}
