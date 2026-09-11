package session

// WHAT A ROUTER SAYS IT IS REACHES THE STORE, FROM A SESSION.
//
// ── THE DEFECT ──────────────────────────────────────────────────────────────
//
// Only the Devices pool reported identity, and the pool excludes every router
// with a live session. Held sessions keep the whole fleet live, so nothing
// reported for any router, and Settings → Devices went on showing the RouterOS
// version each router had two upgrades earlier. No test failed: the pool's own
// hook was pinned and correct, and simply never ran.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mikrodash/internal/collect"
	"mikrodash/internal/hub"
	"mikrodash/internal/routeros"
	"mikrodash/internal/store"
)

// resourceStub answers `/system/resource/print` and nothing else: the one read
// a first System tick makes once health is deferred, which is the prime's tick.
type resourceStub struct{ version, board string }

func (resourceStub) Connected() bool { return true }

func (r resourceStub) Do(c routeros.Cmd) ([]routeros.Reply, error) {
	if c.Path != "/system/resource/print" {
		return nil, nil
	}
	return []routeros.Reply{{
		"version": r.version, "board-name": r.board,
		"cpu-load": "1", "total-memory": "100", "free-memory": "50",
	}}, nil
}

// The session comes from `Acquire` against a real store, so this covers the
// binding (`identityFor` in the Session literal) as well as the collector. The
// router is unreachable on purpose: the reading comes from the stub.
func TestASessionReportsItsRoutersIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_SECRET", "test-secret")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("settings.json", `{}`)
	write("routers.json", `[{"id":"r1","label":"lab","host":"198.51.100.77","port":8728,
	  "username":"u","password":""}]`)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	m := NewManager(st, hub.New())
	defer m.Shutdown()
	type report struct {
		router string
		id     collect.Identity
	}
	var got []report
	m.SetOnIdentity(func(router string, id collect.Identity) { got = append(got, report{router, id}) })

	s, err := m.Acquire("r1")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Release("r1")

	c := s.newSystem(resourceStub{version: "7.24.2 (stable)", board: "hAP ax^3"}, hub.Relay{})
	c.DeferHealth()
	c.Tick()

	// The channel is dropped: the table wants a bare version. The serial is
	// empty on a first tick, and the store skips an empty field rather than
	// clearing it.
	want := report{"r1", collect.Identity{Model: "hAP ax^3", OSVersion: "7.24.2"}}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("the session's System collector reported %+v, want exactly [%+v]: "+
			"a router's upgrade would never reach its record", got, want)
	}
}

// EVERY System collector a session builds goes through newSystem, which is
// what installs the hook. A construction written as a bare `collect.NewSystem`
// would report nothing and fail nothing, which is the defect above in a new
// place. An AST rather than a grep, so this file's own prose cannot satisfy it.
func TestEverySessionSystemCollectorIsBuiltWithTheIdentityHook(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	built := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewSystem" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "collect" {
					return true
				}
				built++
				if fn.Name.Name != "newSystem" {
					t.Errorf("%s: %s builds a System collector with collect.NewSystem directly. "+
						"Use s.newSystem, or this router's identity is never reported.", path, fn.Name.Name)
				}
				return true
			})
		}
	}
	if built == 0 {
		t.Fatal("no collect.NewSystem call found in internal/session: this check is reading nothing")
	}
}
