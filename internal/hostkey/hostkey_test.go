package hostkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/repotree"
)

func pub(t *testing.T) ssh.PublicKey {
	t.Helper()
	p, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(p)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var remote = &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 22}

// TestTrustIsSealed states the rule this package exists for: Trust has exactly
// one method and it is unexported, so ssh.InsecureIgnoreHostKey — or any other
// permissive callback — cannot be adapted into it from outside.
func TestTrustIsSealed(t *testing.T) {
	typ := reflect.TypeOf((*Trust)(nil)).Elem()
	if n := typ.NumMethod(); n != 1 {
		t.Fatalf("Trust has %d methods, want exactly 1", n)
	}
	// reflect reports only exported methods in NumMethod for interfaces when
	// they are exported; an unexported method still counts but has a PkgPath.
	m := typ.Method(0)
	if m.PkgPath == "" {
		t.Fatalf("Trust.%s is exported, so anything outside this package can satisfy Trust and the seal is gone", m.Name)
	}
}

// TestTrustHasOneImplementationAndItIsPin is the half of the seal nothing
// checked.
//
// The posture rests on two sentences in fingerprint.go: Trust's one method is
// unexported, which TestTrustIsSealed above holds, AND "this package contains
// no permissive implementation", which nothing held -- and which was NOT TRUE
// for the life of the package. The deleted Store's trust-on-first-use
// accepted whatever key it was shown first; the only thing keeping it off the
// path that rents machines was provision.trustFor's return type, added by
// #117 after a one-line reinstatement of the original defect left a hundred
// labs and the whole unit suite green (#111). So the second sentence is
// checked here, and the pair is this count plus
// TestPinned_AcceptsTheKeyItHoldsAndRefusesEveryOther: how many
// implementations there are, and that the one there is refuses a key it was
// not given. A count alone is consistent with one permissive implementation.
//
// It is a parse and not reflection, because reflection cannot enumerate the
// types a package declares -- only ask a value it already holds. The subject
// is every type that *could* be handed to Callback, including one nothing
// constructs yet.
//
// A RECEIVER IT CANNOT NAME IS A FATAL, and the first version of this guard
// skipped one instead -- which is the whole reason that rule exists.
// TestFlagTablesClassifyEveryFlagSetMethod puts it best: "the alternative to
// failing on a method it cannot classify is reading straight past a flag,
// which is one more gate that cannot fail in a repository that has already
// found four." A method receiver in Go is T, *T, T[P] or *T[P]; this unwraps
// the star and names the identifier, so a GENERIC receiver -- *ast.IndexExpr
// or *ast.IndexListExpr -- took the skip. Measured on the version that
// skipped: a permissive generic check in trust.go, an exported constructor
// for it, and internal/app's dialNode handing it to session.Dial left
// `go build`, `go test ./...`, `go vet` and `golangci-lint` ALL at exit 0,
// with ember dialling every node it rents with no host key verification at
// all -- and TestTrustIsSealed, TestNoInsecureIgnoreHostKey,
// TestSessionCarriesAPinAndNotAnArbitraryTrust and TestNoProductionDeadCode
// green beside it, because the types were untouched and the constructor had a
// caller. A guard that can be walked around is worse than none, because it
// reads as protection.
//
// What it gives up, stated rather than left to be discovered:
//
//   - A method named check that is not Trust's at all is counted. Accepted
//     rather than narrowed, because a second meaning of `check` in a package
//     this small is itself worth a rename, and the alternative -- matching the
//     parameter list as well -- is a guard that reads straight past an
//     implementation whose author spelled net.Addr through an alias.
//   - A PERMISSIVE check IN A _test.go FILE OF THIS PACKAGE IS NOT REPORTED,
//     because the walk skips test files. AGENTS.md says "not behind a flag,
//     not in a test" and "no escape hatch for a seal, not even for a test",
//     so this is a real gap in the written rule and it is bounded rather than
//     closed: production cannot reference a _test.go identifier, and this
//     package's own tests cannot reach sshdial, so such a type can be handed
//     to nothing that dials. Including test files would mean this guard
//     reporting itself the moment a test needs a stand-in Trust, which is the
//     shape of a guard people learn to ignore.
//
// The file count is a floor for TestNoInsecureIgnoreHostKey's reason: a glob
// that matched nothing would report that nothing is wrong.
func TestTrustHasOneImplementationAndItIsPin(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	impls := map[string]string{}
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "check" || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			typ := fn.Recv.List[0].Type
			if star, isPtr := typ.(*ast.StarExpr); isPtr {
				typ = star.X
			}
			where := name + ":" + strconv.Itoa(fset.Position(fn.Pos()).Line)
			id, ok := typ.(*ast.Ident)
			if !ok {
				// A receiver this guard cannot name is a FATAL and never a
				// skip, which is TestFlagTablesClassifyEveryFlagSetMethod's
				// rule: the alternative to failing on what it cannot classify
				// is reading straight past the thing it is looking for. A
				// generic receiver is *ast.IndexExpr or *ast.IndexListExpr and
				// took the skip this replaced -- and a generic type is a live
				// Trust the moment it is instantiated, so with one wired into
				// internal/app's dialNode ember dialled every node with no
				// host key verification at all while this guard, go vet,
				// golangci-lint and the whole unit suite were exit 0.
				t.Fatalf("%s: check has a %T receiver, which this guard cannot name, so it cannot say whether "+
					"Trust has gained an implementation -- a generic receiver is this shape, and a generic type "+
					"is a Trust once instantiated. Read the note in trust.go, then teach this guard the shape "+
					"rather than letting it skip one", where, typ)
			}
			impls[id.Name] = where
		}
	}
	if parsed < 3 {
		t.Fatalf("parsed only %d non-test Go files in this package; the walk is broken, not the package", parsed)
	}

	if len(impls) != 1 || impls["Pin"] == "" {
		t.Errorf("Trust is implemented by %v, want exactly {Pin}; a second implementation here is a second answer to "+
			"\"may this host key be used\", and the one this package already had accepted a key it had never seen -- "+
			"read the note in trust.go before adding one", impls)
	}
}

// TestNoInsecureIgnoreHostKey walks the tree. The seal makes the insecure
// path untypeable; this catches anyone writing it anyway in some other shape.
//
// It matches on the AST rather than on the file's bytes. A text search flags
// this test's own name and the prose in fingerprint.go that explains the rule,
// which trains the reader to ignore the failure — the one outcome a guard test
// must never produce.
//
// It walks THIS checkout and prunes any other, for the reason
// internal/repotree carries: an agent worktree lives inside the tree it was
// cut from, so without the prune a permissive callback in somebody else's
// copy fails this suite -- and a syntax error in a file they were halfway
// through editing fails it with a parse error naming a path that is not in
// this repository at all.
//
// The file count is a floor and not a tally, because this walk's own failure
// mode is now visiting nothing: a prune that grew too wide would leave the
// guard reporting success for source it never read, which is the
// `AssertNoStray` shape. There are 313 of them, so the floor is well under it.
func TestNoInsecureIgnoreHostKey(t *testing.T) {
	root := filepath.Join("..", "..")
	parsed := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "bin" {
				return filepath.SkipDir
			}
			if repotree.Nested(root, path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		parsed++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Insecure"+"IgnoreHostKey" {
				return true
			}
			t.Errorf("%s:%d: %s is referenced; ember authenticates every host",
				path, fset.Position(sel.Pos()).Line, sel.Sel.Name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if parsed < 150 {
		t.Fatalf("parsed only %d Go files; the walk is broken, not the module -- a guard that read nothing reports that nothing is wrong", parsed)
	}
}

func TestPinned_RefusesAnUnsetFingerprint(t *testing.T) {
	if _, err := Pinned(Fingerprint{}); err == nil {
		t.Fatal("Pinned built a trust from nothing; \"pinned\" would then mean \"ignored\"")
	}
}

func TestPinned_AcceptsTheKeyItHoldsAndRefusesEveryOther(t *testing.T) {
	want := pub(t)
	tr, err := Pinned(Of(want))
	if err != nil {
		t.Fatal(err)
	}

	if err := Callback(tr, "n1")("host", remote, want); err != nil {
		t.Fatalf("the pinned key was refused: %v", err)
	}

	other := pub(t)
	err = Callback(tr, "n1")("host", remote, other)
	if err == nil {
		t.Fatal("a different host key was accepted")
	}
	var mm *MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("want a MismatchError, got %T: %v", err, err)
	}
	for _, want := range []string{"n1", "203.0.113.7", "ember down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mismatch message should contain %q: %s", want, err)
		}
	}
}

func TestMismatchError_SurvivesAMissingRemote(t *testing.T) {
	e := &MismatchError{NodeID: "n", Want: Fingerprint{}, Got: Fingerprint{}}
	if !strings.Contains(e.Error(), "unknown address") {
		t.Errorf("got %s", e.Error())
	}
}

func TestFingerprint(t *testing.T) {
	k := pub(t)
	fp := Of(k)

	if fp.IsZero() {
		t.Fatal("a computed fingerprint must not be zero")
	}
	if fp.Algo() != k.Type() {
		t.Errorf("Algo() = %q, want %q", fp.Algo(), k.Type())
	}

	// The spelling is asserted against an independent recomputation rather
	// than through a round trip, because ParseFingerprint went with the pin
	// file (#120) and this string is what an operator reads out of a
	// MismatchError and compares against `ssh-keygen -lf`. A String whose
	// shape nothing checks is a String that drifts.
	sum := sha256.Sum256(k.Marshal())
	if got, want := fp.String(), k.Type()+" SHA256:"+base64.RawStdEncoding.EncodeToString(sum[:]); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	if fp.Equal(Of(pub(t))) {
		t.Error("different keys compared equal")
	}
	// A zero fingerprint must never compare equal to anything, including
	// another zero one: otherwise two unpopulated provider fields would
	// "match".
	if (Fingerprint{}).Equal(Fingerprint{}) || fp.Equal(Fingerprint{}) || (Fingerprint{}).Equal(fp) {
		t.Error("a zero fingerprint compared equal to something")
	}
	if got := (Fingerprint{}).String(); got != "<unset>" {
		t.Errorf("zero String() = %q", got)
	}
}

func TestFingerprint_DifferentAlgosNeverMatch(t *testing.T) {
	a := Of(pub(t))
	b := a
	b.algo = "ssh-rsa"
	if a.Equal(b) {
		t.Error("the same digest under a different algorithm must not match")
	}
}

func TestMint_ProducesAUsableInjectableKey(t *testing.T) {
	p, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	if p.Private.IsZero() {
		t.Fatal("no private half to inject")
	}
	// The private half must be parseable by the thing that will consume it:
	// sshd reading /etc/ssh/ssh_host_ed25519_key.
	signer, err := ssh.ParsePrivateKey(p.Private.Bytes())
	if err != nil {
		t.Fatalf("sshd would reject the minted key: %v", err)
	}
	if !Of(signer.PublicKey()).Equal(p.Fingerprint()) {
		t.Fatal("the pair's halves do not match, so the pin would never verify")
	}
	// And it must not print itself.
	if strings.Contains(p.Private.String(), "PRIVATE KEY") {
		t.Error("the minted key printed itself")
	}
}

func TestMint_IsFreshEveryTime(t *testing.T) {
	a, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint().Equal(b.Fingerprint()) {
		t.Fatal("two nodes were minted the same host key")
	}
}

// TestNodeKey_RoundTripsThroughAProviderLabel is the path a real node takes:
// ember mints a key, the provider records the public half as a label, and the
// next run reads it back to pin against. If that round trip loses anything,
// every connection fails with a mismatch and the node looks compromised.
func TestNodeKey_RoundTripsThroughAProviderLabel(t *testing.T) {
	p, err := Mint()
	if err != nil {
		t.Fatal(err)
	}

	recorded := p.NodeKey()
	if recorded.Source != node.HostKeyMinted {
		t.Errorf("Source = %v, want minted", recorded.Source)
	}
	if recorded.IsZero() {
		t.Fatal("NodeKey produced nothing to record")
	}

	got, err := FromNodeKey(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(p.Fingerprint()) {
		t.Fatal("the fingerprint did not survive the label; every connection would read as a mismatch")
	}
}

func TestFromNodeKey_Refuses(t *testing.T) {
	good, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	ok := good.NodeKey()

	bad := ok
	bad.Base64 = "!!!not base64!!!"
	mismatchedAlgo := ok
	mismatchedAlgo.Algo = "ssh-rsa"
	notAKey := ok
	notAKey.Base64 = "AAAA"

	for name, in := range map[string]node.HostKey{
		"empty":          {},
		"bad base64":     bad,
		"not a key":      notAKey,
		"algorithm lies": mismatchedAlgo,
	} {
		if fp, err := FromNodeKey(in); err == nil {
			t.Errorf("%s: FromNodeKey accepted it, got %v", name, fp)
		}
	}
}
