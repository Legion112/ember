package hostkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
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

// TestNoInsecureIgnoreHostKey walks the tree. The seal makes the insecure
// path untypeable; this catches anyone writing it anyway in some other shape.
//
// It matches on the AST rather than on the file's bytes. A text search flags
// this test's own name and the prose in fingerprint.go that explains the rule,
// which trains the reader to ignore the failure — the one outcome a guard test
// must never produce.
func TestNoInsecureIgnoreHostKey(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
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

	round, err := ParseFingerprint(fp.String())
	if err != nil {
		t.Fatalf("ParseFingerprint rejected what String produced: %v", err)
	}
	if !round.Equal(fp) {
		t.Error("String/ParseFingerprint did not round-trip")
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

func TestParseFingerprint_Refuses(t *testing.T) {
	good := Of(pub(t)).String()
	for name, in := range map[string]string{
		"empty":         "",
		"no algo":       "SHA256:abc",
		"no sha prefix": "ssh-ed25519 MD5:abc",
		"bad base64":    "ssh-ed25519 SHA256:!!!!",
		"short digest":  "ssh-ed25519 SHA256:" + strings.TrimPrefix(good[strings.Index(good, "SHA256:"):], "SHA256:")[:10],
	} {
		if got, err := ParseFingerprint(in); err == nil {
			t.Errorf("%s: ParseFingerprint(%q) = %v, want an error", name, in, got)
		}
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
