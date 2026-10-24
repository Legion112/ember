package node

import (
	"strings"
	"testing"
)

func TestEndpoint_ValidateNamesWhatIsMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		ep   Endpoint
		want string
	}{
		"no host":    {Endpoint{Port: 22, User: "root"}, "host"},
		"blank host": {Endpoint{Host: "  ", Port: 22, User: "root"}, "host"},
		"no port":    {Endpoint{Host: "h", User: "root"}, "port"},
		"no user":    {Endpoint{Host: "h", Port: 22}, "user"},
		"blank user": {Endpoint{Host: "h", Port: 22, User: " "}, "user"},
	} {
		err := tc.ep.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q should mention %q", name, err, tc.want)
		}
	}
	if err := (Endpoint{Host: "h", Port: 22, User: "root"}).Validate(); err != nil {
		t.Errorf("a complete endpoint was refused: %v", err)
	}
}

func TestEndpoint_String(t *testing.T) {
	ep := Endpoint{Host: "1.2.3.4", Port: 2222, User: "root"}
	if got, want := ep.String(), "root@1.2.3.4:2222"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestHostKeySource_ZeroValueIsUnauthenticated: forgetting to populate the
// source must fail closed, not open.
func TestHostKeySource_ZeroValueIsUnauthenticated(t *testing.T) {
	var s HostKeySource
	if s != HostKeyUnset {
		t.Fatal("the zero HostKeySource must be HostKeyUnset")
	}
	if s.Authenticated() {
		t.Fatal("an unset host key source must not report as authenticated")
	}
}

func TestHostKeySource_Authenticated(t *testing.T) {
	for s, want := range map[HostKeySource]bool{
		HostKeyUnset:   false,
		HostKeyMinted:  true,
		HostKeyFromAPI: true,
		// Pinned means ember accepted a key it had never seen. The connection
		// that established the pin was not authenticated, and `ember status`
		// reports that rather than hiding it.
		HostKeyPinned: false,
	} {
		if got := s.Authenticated(); got != want {
			t.Errorf("%v.Authenticated() = %v, want %v", s, got, want)
		}
	}
}

func TestHostKeySource_String(t *testing.T) {
	if got := HostKeyMinted.String(); got != "minted" {
		t.Errorf("String() = %q", got)
	}
	if got := HostKeySource(9).String(); got != "hostkeysource(9)" {
		t.Errorf("unknown source String() = %q", got)
	}
}

func TestHostKey_IsZero(t *testing.T) {
	if !(HostKey{}).IsZero() {
		t.Error("the zero HostKey must report IsZero")
	}
	if (HostKey{Algo: "ssh-ed25519", Base64: "AAAA"}).IsZero() {
		t.Error("a populated HostKey must not report IsZero")
	}
	// Source alone is not key material.
	if !(HostKey{Source: HostKeyPinned}).IsZero() {
		t.Error("a HostKey with a source but no key is still empty")
	}
}
