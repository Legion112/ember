package node

import "testing"

func TestParseProto(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Proto
		ok   bool
	}{
		{"tcp", ProtoTCP, true},
		{"TCP", ProtoTCP, true},
		{"udp", ProtoUDP, true},
		{"UDP", ProtoUDP, true},
		{"http", ProtoHTTP, true},
		{"HTTPS", ProtoHTTP, true},
		{"", ProtoUnknown, false},
		{"sctp", ProtoUnknown, false},
		{"tcp6", ProtoUnknown, false},
		{" tcp", ProtoUnknown, false},
	} {
		got, err := ParseProto(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("ParseProto(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
		}
		if got != tc.want {
			t.Errorf("ParseProto(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestUnknownProtoIsTheZeroValue: a Proto nobody set must not read as TCP,
// because "plain TCP on a public port" is the answer that gets waved through.
func TestUnknownProtoIsTheZeroValue(t *testing.T) {
	var p Proto
	if p != ProtoUnknown {
		t.Fatalf("the zero Proto is %v", p)
	}
}

func TestProtoString(t *testing.T) {
	for p, want := range map[Proto]string{
		ProtoUnknown: "unknown",
		ProtoTCP:     "tcp",
		ProtoUDP:     "udp",
		ProtoHTTP:    "http",
	} {
		if got := p.String(); got != want {
			t.Errorf("%d = %q, want %q", uint8(p), got, want)
		}
	}
	if got := Proto(200).String(); got != "proto(200)" {
		t.Errorf("an unnamed Proto printed %q", got)
	}
}

func TestExposedString(t *testing.T) {
	if got := (Exposed{Port: 22, Proto: ProtoTCP}).String(); got != "22/tcp" {
		t.Errorf("got %q", got)
	}
	got := Exposed{Port: 8188, Proto: ProtoHTTP, URL: "https://abc-8188.proxy.runpod.net"}.String()
	if got != "8188/http at https://abc-8188.proxy.runpod.net" {
		t.Errorf("got %q", got)
	}
}

// TestExposureDistinguishesNoneFromUnasked is the invariant the two-value
// return exists for. An earlier spelling built the slice with a single append,
// which returns nil for empty input: a provider reporting "I published
// nothing" -- the safest answer there is -- read as "never reported", and
// failed the assertion it should have passed.
func TestExposureDistinguishesNoneFromUnasked(t *testing.T) {
	var unasked Node
	if es, ok := unasked.Exposure(); ok {
		t.Errorf("a node nobody asked reported %v", es)
	}

	none := Node{}.WithExposed(nil)
	es, ok := none.Exposure()
	if !ok {
		t.Error("a provider that reported no public ports reads as unasked")
	}
	if len(es) != 0 {
		t.Errorf("got %v", es)
	}

	empty := Node{}.WithExposed([]Exposed{})
	if _, ok := empty.Exposure(); !ok {
		t.Error("an explicitly empty report reads as unasked")
	}
}

// TestWithExposedCopies: the caller keeps its slice, so a provider reusing a
// buffer must not be able to rewrite what the node already reported.
func TestWithExposedCopies(t *testing.T) {
	src := []Exposed{{Port: 22, Proto: ProtoTCP}}
	n := Node{}.WithExposed(src)
	src[0] = Exposed{Port: 8188, Proto: ProtoHTTP}

	es, _ := n.Exposure()
	if es[0].Port != 22 || es[0].Proto != ProtoTCP {
		t.Fatalf("the node's report changed under it: %v", es)
	}
}
