package dockerengine

import (
	"strings"
	"testing"
)

// TestRefuseHostNetwork is the executable form of the one rule about Docker.
// --network host makes every listener inside the container a listener on the
// operator's machine, which walks straight past internal/loopback.
func TestRefuseHostNetwork(t *testing.T) {
	for _, ok := range []string{"", "bridge", "none"} {
		if err := RefuseHostNetwork(ok); err != nil {
			t.Errorf("RefuseHostNetwork(%q) = %v, want nil", ok, err)
		}
	}
	err := RefuseHostNetwork("host")
	if err == nil {
		t.Fatal("host networking was allowed")
	}
	if !strings.Contains(err.Error(), "loopback guard") {
		t.Errorf("the refusal should say what it protects, got %v", err)
	}
	// An allow-list: a mode this function has never heard of is refused,
	// because the next one Docker adds might also share the host namespace
	// and nobody would revisit the switch.
	if err := RefuseHostNetwork("container:other"); err == nil {
		t.Error("an unrecognised network mode was allowed")
	}
}
