package exposure

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
)

func TestSSHPortIsTheOnlyPublicOne(t *testing.T) {
	s := SSHPort()
	if !s.IsPublic() || s.Port() != 22 || s.Proto() != node.ProtoTCP {
		t.Fatalf("SSHPort() = %v", s)
	}

	// The only other value of this type there is, asserted beside it: a
	// struct literal cannot name a port at all, so a request ember did not
	// build is zero and a provider refuses it.
	if (PortRequest{}).IsPublic() {
		t.Fatal("the zero request reports itself as public")
	}
}

// TestPortRequestCannotBeSpelledPublic is the security property of this type,
// stated as a test because the compiler already enforces it and there is
// nothing left to assert at runtime.
//
// Every field of PortRequest is unexported, so no package outside this one can
// write PortRequest{public: true} -- or any other literal. The reflection here
// is not the control; it is a guard against someone exporting a field later
// and nothing noticing, since that single edit would re-open the whole hole.
func TestPortRequestCannotBeSpelledPublic(t *testing.T) {
	rt := reflect.TypeOf(PortRequest{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.IsExported() {
			t.Errorf("PortRequest.%s is exported: any package can now build a public port request with a struct literal, and SSHPort stops being the only way", f.Name)
		}
	}
	if rt.NumField() == 0 {
		t.Fatal("PortRequest has no fields; this test is checking nothing")
	}
}

func TestZeroRequestIsRecognisable(t *testing.T) {
	if !(PortRequest{}).IsZero() {
		t.Error("the zero request does not report itself as one")
	}
	if SSHPort().IsZero() {
		t.Error("SSHPort() reports itself as a zero request")
	}
}

func TestPortRequestString(t *testing.T) {
	if got := SSHPort().String(); got != "22/tcp public" {
		t.Errorf("got %q", got)
	}
	// The "internal" arm is reachable only through the zero value now, and it
	// is asserted rather than deleted: the word is what an AssertSSHOnly
	// failure prints, so a String that said "public" for everything would
	// make that message agree with any provider at all.
	if got := (PortRequest{}).String(); got != "0/unknown internal" {
		t.Errorf("got %q", got)
	}
}

// TestRequestsIsSSHAndNothingElse: the set is built in one place so that
// "nothing but sshd is published" does not depend on each provider
// remembering it.
//
// THE COUNT IS THE ASSERTION AND NOT THE VISIBILITY. A set with a second
// member whose IsPublic is false would still satisfy "exactly one public
// port" -- and a provider that mapped it would be declaring a port to the
// API, which on RunPod is one field away from a proxy URL. So the length is
// checked first.
func TestRequestsIsSSHAndNothingElse(t *testing.T) {
	rs := Requests()
	if len(rs) != 1 {
		t.Fatalf("got %d requests: %v", len(rs), rs)
	}
	if !rs[0].IsPublic() || rs[0].Port() != 22 || rs[0].Proto() != node.ProtoTCP {
		t.Fatalf("got %v", rs[0])
	}
}

// TestNoPublicPortOutsideSSHPort walks this package's own sources and fails if
// anything other than SSHPort writes the public field.
//
// The field being unexported stops other packages. It does not stop a later
// edit inside this one from adding a second constructor that sets it, which
// would be the whole hole re-opened by someone who was being helpful. Stated
// as code for the same reason as TestNoListenOutsideLoopback.
func TestNoPublicPortOutsideSSHPort(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	checked, found := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				kv, ok := n.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				id, ok := kv.Key.(*ast.Ident)
				if !ok || id.Name != "public" {
					return true
				}
				lit, isLit := kv.Value.(*ast.Ident)
				if isLit && lit.Name == "false" {
					return true
				}
				found++
				if fn.Name.Name != "SSHPort" {
					t.Errorf("%s:%d: %s sets the public field; only SSHPort may",
						name, fset.Position(kv.Pos()).Line, fn.Name.Name)
				}
				return true
			})
		}
	}

	if checked == 0 {
		t.Fatal("parsed no non-test files; the walk is broken, not the package")
	}
	// A walk that matches nothing passes for the wrong reason, which is the
	// failure mode every source-walking test has.
	if found == 0 {
		t.Fatal("found no assignment to the public field at all; the matcher no longer matches SSHPort and would miss a new one")
	}
}
