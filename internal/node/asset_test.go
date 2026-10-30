package node

import (
	"strings"
	"testing"
)

// goodDigest is a real SHA-256: the digest of the empty string.
const goodDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestAsset_Validate_Accepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Asset
		want Asset
	}{
		{
			name: "the ordinary case",
			in:   Asset{URL: "https://huggingface.co/x/y.gguf", SHA256: goodDigest, As: "sd15.gguf"},
			want: Asset{URL: "https://huggingface.co/x/y.gguf", SHA256: goodDigest, As: "sd15.gguf"},
		},
		{
			// Plain HTTP is allowed because the digest, not the transport, is
			// what makes the bytes trustworthy. An attacker on the path can
			// deny service; they cannot substitute content. The labs depend
			// on this: emberwl serves over HTTP inside a Docker network.
			name: "http, because the digest is the integrity control",
			in:   Asset{URL: "http://emberwl:8080/blob", SHA256: goodDigest, As: "blob.bin"},
			want: Asset{URL: "http://emberwl:8080/blob", SHA256: goodDigest, As: "blob.bin"},
		},
		{
			name: "an uppercase digest is normalised, not refused",
			in:   Asset{URL: "https://h/x", SHA256: strings.ToUpper(goodDigest), As: "a.gguf"},
			want: Asset{URL: "https://h/x", SHA256: goodDigest, As: "a.gguf"},
		},
		{
			name: "a sha256: prefix is accepted, as image digests are written",
			in:   Asset{URL: "https://h/x", SHA256: "sha256:" + goodDigest, As: "a.gguf"},
			want: Asset{URL: "https://h/x", SHA256: goodDigest, As: "a.gguf"},
		},
		{
			name: "surrounding space survives nothing",
			in:   Asset{URL: "  https://h/x  ", SHA256: "  " + goodDigest + "  ", As: "a.gguf"},
			want: Asset{URL: "https://h/x", SHA256: goodDigest, As: "a.gguf"},
		},
		{
			name: "a subdirectory on the store",
			in:   Asset{URL: "https://h/x", SHA256: goodDigest, As: "wan/high-noise.gguf"},
			want: Asset{URL: "https://h/x", SHA256: goodDigest, As: "wan/high-noise.gguf"},
		},
		{
			name: "a redundant name component is dropped, not refused",
			in:   Asset{URL: "https://h/x", SHA256: goodDigest, As: "./wan//a.gguf"},
			want: Asset{URL: "https://h/x", SHA256: goodDigest, As: "wan/a.gguf"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.Validate()
			if err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if got != tc.want {
				t.Errorf("Validate() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAsset_Validate_Refuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      Asset
		wantMsg string
	}{
		{"no name", Asset{URL: "https://h/x", SHA256: goodDigest}, "is empty"},
		{"absolute name", Asset{URL: "https://h/x", SHA256: goodDigest, As: "/etc/passwd"}, "absolute path"},
		{"escaping name", Asset{URL: "https://h/x", SHA256: goodDigest, As: "../authorized_keys"}, "escapes"},
		{"name of only separators", Asset{URL: "https://h/x", SHA256: goodDigest, As: "././"}, "names nothing"},
		{
			// An asset with no digest cannot be verified, and the store is
			// writable by every run that mounts it. Without the digest there
			// is nothing standing between one workload and the next one's
			// weights.
			name: "no digest", in: Asset{URL: "https://h/x", As: "a.gguf"}, wantMsg: "cannot be verified",
		},
		{"short digest", Asset{URL: "https://h/x", SHA256: "abc123", As: "a.gguf"}, "hex characters"},
		{"long digest", Asset{URL: "https://h/x", SHA256: goodDigest + "00", As: "a.gguf"}, "hex characters"},
		{"non-hex digest", Asset{URL: "https://h/x", SHA256: strings.Repeat("z", 64), As: "a.gguf"}, "not a hex digit"},
		{"no scheme", Asset{URL: "example.com/x", SHA256: goodDigest, As: "a.gguf"}, "no scheme"},
		{
			// An allow-list. file:// would have the node read its own disk
			// and report a verified download.
			name: "file scheme", in: Asset{URL: "file:///etc/passwd", SHA256: goodDigest, As: "a.gguf"}, wantMsg: "not fetchable",
		},
		{"ftp scheme", Asset{URL: "ftp://h/x", SHA256: goodDigest, As: "a.gguf"}, "not fetchable"},
		{"no host", Asset{URL: "https:///x", SHA256: goodDigest, As: "a.gguf"}, "no host"},
		{"unparseable url", Asset{URL: "https://%zz", SHA256: goodDigest, As: "a.gguf"}, "asset a.gguf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted %+v as %+v", tc.in, got)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.wantMsg)
			}
			if got != (Asset{}) {
				t.Errorf("a refused asset returned %+v, want the zero value", got)
			}
		})
	}
}

func TestAsset_String(t *testing.T) {
	a := Asset{As: "sd15.gguf", SHA256: goodDigest}
	if got, want := a.String(), "sd15.gguf (e3b0c4429 8fc"; strings.HasPrefix(got, want) {
		t.Fatalf("String() = %q, which should not contain a space in the digest", got)
	}
	if got, want := a.String(), "sd15.gguf (e3b0c4429"; !strings.HasPrefix(got, want) {
		t.Errorf("String() = %q, want it to start with %q", got, want)
	}
	// A short digest is printed whole rather than being given a misleading
	// ellipsis. Only Validate guarantees the length, and String is reached
	// from error paths where it has not run.
	if got, want := (Asset{As: "x", SHA256: "abc"}).String(), "x (abc)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestValidateAssets(t *testing.T) {
	ok := []Asset{
		{URL: "https://h/a", SHA256: goodDigest, As: "a.gguf"},
		{URL: "https://h/b", SHA256: strings.Repeat("b", 64), As: "b.gguf"},
	}
	got, err := ValidateAssets(ok)
	if err != nil {
		t.Fatalf("ValidateAssets() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d assets, want 2", len(got))
	}

	if _, err := ValidateAssets(nil); err != nil {
		t.Errorf("ValidateAssets(nil) = %v, want no error", err)
	}

	for _, tc := range []struct {
		name    string
		in      []Asset
		wantMsg string
	}{
		{
			name:    "an invalid member fails the set",
			in:      []Asset{ok[0], {URL: "https://h/c", SHA256: "nope", As: "c.gguf"}},
			wantMsg: "hex characters",
		},
		{
			name:    "the same asset twice",
			in:      []Asset{ok[0], ok[0]},
			wantMsg: "listed twice",
		},
		{
			// The one that matters: two assets that cannot both be satisfied.
			// Whichever fetch runs second overwrites a verified file or fails
			// against a digest that was never meant for it, and the message
			// blames the store for what the config asked for.
			name: "one name, two digests",
			in: []Asset{
				{URL: "https://h/a", SHA256: goodDigest, As: "a.gguf"},
				{URL: "https://h/b", SHA256: strings.Repeat("b", 64), As: "a.gguf"},
			},
			wantMsg: "different digests",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateAssets(tc.in)
			if err == nil {
				t.Fatalf("ValidateAssets() accepted %+v", got)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("ValidateAssets() = %q, want it to mention %q", err, tc.wantMsg)
			}
			if got != nil {
				t.Errorf("a refused set returned %+v, want nil", got)
			}
		})
	}
}

func TestSafeRelPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "plain", in: "a.gguf", want: "a.gguf"},
		{name: "nested", in: "wan/a.gguf", want: "wan/a.gguf"},
		{name: "doubled separators mean nothing", in: "wan//a.gguf", want: "wan/a.gguf"},
		{name: "a leading dot is not a rewrite of intent", in: "./a.gguf", want: "a.gguf"},
		{name: "trailing separator", in: "wan/", want: "wan"},
		{name: "empty", in: "", wantErr: "is empty"},
		{name: "space only", in: "   ", wantErr: "is empty"},
		{name: "absolute", in: "/etc/passwd", wantErr: "absolute path"},
		{
			// The case the whole function exists for. path.Clean("/../x")
			// returns "/x" -- it absorbs the ".." because nothing is above
			// root -- so a check that cleaned first could never fire.
			name: "escaping", in: "../authorized_keys", wantErr: "escapes",
		},
		{name: "escaping from inside", in: "wan/../../x", wantErr: "escapes"},
		{name: "a backslash is a separator too", in: `..\authorized_keys`, wantErr: "escapes"},
		{name: "absolute with backslashes", in: `\etc\passwd`, wantErr: "absolute path"},
		{name: "nothing but separators is absolute first", in: "///", wantErr: "absolute path"},
		{name: "separators with nothing between them", in: "a/..", wantErr: "escapes"},
		{name: "nothing but dots", in: "./././", wantErr: "names nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SafeRelPath(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("SafeRelPath(%q) = %q, want an error", tc.in, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("SafeRelPath(%q) = %q, want it to mention %q", tc.in, err, tc.wantErr)
				}
				if got != "" {
					t.Errorf("a refused name returned %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SafeRelPath(%q) = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("SafeRelPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSafeRelPath_NeverEscapes(t *testing.T) {
	// A property over the hostile shapes, so a future edit that reintroduces
	// cleaning-before-checking fails here rather than in a lab. Anything
	// accepted must stay inside: no leading separator and no ".." left in it.
	for _, in := range []string{
		"a.gguf", "a/b/c.gguf", "./a", "a//b", `a\b`, "...", "..a", "a..",
		"....//a", "a/./b", "a/...", ". /a",
	} {
		got, err := SafeRelPath(in)
		if err != nil {
			continue
		}
		if strings.HasPrefix(got, "/") {
			t.Errorf("SafeRelPath(%q) = %q, which is absolute", in, got)
		}
		for _, part := range strings.Split(got, "/") {
			if part == ".." {
				t.Errorf("SafeRelPath(%q) = %q, which still escapes", in, got)
			}
		}
	}
}
