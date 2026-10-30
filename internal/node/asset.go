package node

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Asset is one file that must be on the store, verified, before a workload
// runs.
//
// The digest is mandatory and it is the point. A store is writable by every
// node that mounts it, which makes it a channel between runs that are
// otherwise isolated: one workload can leave something behind that the next
// one loads as its model. RunPod offers no read-only mount, so the write
// cannot be prevented and is detected instead -- every asset is checked
// against its digest before any GPU time is spent on it.
//
// That same digest is why the transport may be plain HTTP. TLS would
// authenticate the server; the digest authenticates the bytes, which is the
// stronger claim and the one that matters when the file is forty gigabytes of
// somebody else's weights. An attacker on the path can deny service -- the
// fetch fails closed and nothing is written -- but cannot substitute content.
type Asset struct {
	// URL is where to fetch it if it is not already on the store.
	URL string

	// SHA256 is the hex digest of the file, lowercase.
	SHA256 string

	// As is the path on the store, relative to the mount.
	As string
}

func (a Asset) String() string { return a.As + " (" + shortDigest(a.SHA256) + ")" }

func shortDigest(d string) string {
	if len(d) <= 12 {
		return d
	}
	return d[:12] + "…"
}

// sha256HexLen is the length of a hex-encoded SHA-256 digest.
const sha256HexLen = 64

// Validate checks the asset and returns a normalised copy.
//
// It returns the normalised value rather than mutating, so a caller cannot
// validate and then use the un-normalised original -- which is how a digest
// comparison fails on case alone.
func (a Asset) Validate() (Asset, error) {
	name, err := SafeRelPath(a.As)
	if err != nil {
		return Asset{}, fmt.Errorf("asset name %q %w", a.As, err)
	}
	a.As = name

	digest, err := normalizeDigest(a.SHA256)
	if err != nil {
		return Asset{}, fmt.Errorf("asset %s: %w", name, err)
	}
	a.SHA256 = digest

	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil {
		return Asset{}, fmt.Errorf("asset %s: url %q: %w", name, a.URL, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return Asset{}, fmt.Errorf("asset %s: url %q has no scheme", name, a.URL)
	default:
		// An allow-list, not a deny-list. file:// would make the node read its
		// own disk and call it a verified download, and anything else is a
		// scheme whose fetching semantics nobody here has thought about.
		return Asset{}, fmt.Errorf("asset %s: url scheme %q is not fetchable; use http or https", name, u.Scheme)
	}
	if u.Host == "" {
		return Asset{}, fmt.Errorf("asset %s: url %q has no host", name, a.URL)
	}
	a.URL = u.String()
	return a, nil
}

// normalizeDigest accepts either case and returns lowercase hex.
//
// Both cases are accepted because sha256sum writes lowercase and plenty of
// web pages publish uppercase, and refusing one of them teaches nothing. The
// comparison happens on the normalised form so that a digest which is correct
// but shouted does not fail as a mismatch -- a mismatch means "these bytes are
// not the bytes you asked for", and spending that message on a typo makes it
// mean less when it is real.
func normalizeDigest(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", errors.New("sha256 is empty: an asset with no digest cannot be verified, and an unverifiable asset is one this will not fetch")
	}
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) != sha256HexLen {
		return "", fmt.Errorf("sha256 %q is %d characters; a SHA-256 digest is %d hex characters", s, len(s), sha256HexLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("sha256 %q contains %q, which is not a hex digit", s, string(c))
		}
	}
	return s, nil
}

// ValidateAssets validates each asset and refuses two that land on one name.
//
// The duplicate check is not tidiness. Two assets with the same As and
// different digests describe a file that cannot satisfy both, so whichever
// fetch runs second either overwrites a verified file or fails against a
// digest that was never meant for it -- and the resulting message blames the
// store for what the config asked for.
func ValidateAssets(in []Asset) ([]Asset, error) {
	out := make([]Asset, 0, len(in))
	seen := make(map[string]string, len(in))
	for _, a := range in {
		v, err := a.Validate()
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[v.As]; dup {
			if prev == v.SHA256 {
				return nil, fmt.Errorf("asset %s is listed twice", v.As)
			}
			return nil, fmt.Errorf("asset %s is listed twice with different digests (%s and %s); only one file can be at that name", v.As, shortDigest(prev), shortDigest(v.SHA256))
		}
		seen[v.As] = v.SHA256
		out = append(out, v)
	}
	return out, nil
}

// SafeRelPath checks a relative path component by component and returns it
// joined, or refuses it.
//
// The components are checked before any cleaning, which is the whole point.
// path.Clean("/../a.gguf") returns "/a.gguf": it *absorbs* the ".." because
// nothing is above root. An earlier version of transfer.safeName cleaned first
// and then looked for a leading "..", so the check could never fire -- every
// hostile name was silently rewritten into a valid one, nothing escaped
// because os.Root still held, and ember reported success for a file called
// something else.
//
// It lives in node, which imports nothing, so the one check can be shared by
// everything that needs it rather than written twice and fixed once.
func SafeRelPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("is empty")
	}

	// A backslash is treated as a separator too, so a Windows-style name
	// cannot smuggle a component past the loop below.
	slashed := strings.ReplaceAll(name, `\`, "/")

	if strings.HasPrefix(slashed, "/") {
		return "", errors.New("is an absolute path")
	}

	kept := make([]string, 0, strings.Count(slashed, "/")+1)
	for _, p := range strings.Split(slashed, "/") {
		switch p {
		case "":
			// A doubled or trailing separator carries no meaning.
		case ".":
			// Harmless, and dropping it is not a rewrite of intent.
		case "..":
			return "", errors.New("escapes its directory")
		default:
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return "", errors.New("names nothing")
	}
	return path.Join(kept...), nil
}
