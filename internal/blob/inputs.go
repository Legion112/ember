package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	// Registered for their DecodeConfig only: no pixels are decoded here.
	// THIS IS THE ALLOW-LIST and it is spelled as two imports rather than as
	// a list of strings, because a format ember does not import cannot be
	// accepted by accident -- image.DecodeConfig answers image.ErrFormat for
	// anything whose decoder is not registered, and there is no way to widen
	// the set without adding a line to this file. A string check against
	// `format` would be a second spelling of the same rule, and the two could
	// disagree.
	//
	// webp is deliberately absent although a browser's file picker offers it:
	// the standard library has no webp decoder, so accepting one would mean
	// either a new dependency or relaying bytes ember never looked at.
	_ "image/jpeg"
	_ "image/png"
)

// WHY INPUTS ARE A SEPARATE STORE FROM Dir, AND NOT A KEY PREFIX IN IT.
//
// Dir is rooted at the OUTPUT directory and its keys are
// "<request>/<stem>-NN.png": a key names the request that produced it, which
// is what lets `ember history rm` remove a request's bytes and
// `ember history du` measure them. An upload has no request. It exists
// BEFORE one -- a file is picked, and only then is a batch composed and
// queued -- so it cannot be keyed by a request that does not exist yet, and
// writing it under one that does would make it prunable as though it were a
// paid-for result. An input nobody generated with is bytes ember is storing
// for nobody; a result is bytes somebody was billed for. The two have
// different lifetimes, so they have different roots.
//
// IT IS CONTENT-ADDRESSED, and that answers three questions at once.
//
//   - The uploaded filename never reaches a path. It is not sanitised, not
//     checked against a list, not stored: it is DISCARDED, because the key is
//     derived from the bytes. A name that cannot reach a path join cannot be
//     a traversal, and this repository has already shipped the other bug once
//     -- internal/transfer cleaned a hostile member name instead of refusing
//     it, and path.Clean absorbed the traversal so the check could never
//     fire.
//   - "Can one request name another request's input?" is not an access
//     question here, because there is no such thing as whose input it is. The
//     name IS the content: naming an object requires holding its sha256,
//     which requires holding the bytes. Two jobs that upload the same picture
//     share one object and neither can learn anything from the other's. What
//     a request CANNOT do is name a digest and be served different bytes --
//     Load re-hashes what it read and refuses a mismatch, so the reference is
//     a claim about content that is checked rather than trusted.
//   - Every asset is verified by digest before any GPU time is spent on it,
//     which is already the rule for the weights on a store. An input is the
//     same kind of thing arriving by a different door, so it gets the same
//     treatment and the reference is the digest rather than something beside
//     it that could drift from it.
//
// CONTAINMENT IS ENFORCED TWICE, by two different mechanisms, which is the
// rule internal/transfer follows for tar members and Dir follows above:
// ParseRef refuses anything that is not sixty-four lowercase hex characters,
// and every read and write goes through an os.Root. A string check misses a
// spelling; a Root cannot tell the caller what the name was trying to do.

// InputsDirName is the one name for the staging directory.
//
// A CONSTANT AND NOT A LITERAL AT EACH CALL SITE, for the reason statestore's
// own DirName is one: the journal's "nodes" subdirectory was a literal in the
// CLI and a second literal in the lab harness, they drifted, and a lab
// asserting "a clean run leaves no records" passed while reading the wrong
// directory.
const InputsDirName = "inputs"

// MaxInputBytes is the largest upload this server will take.
//
// A REFUSAL AT THE BOUND, NEVER A TRUNCATION -- see ReadInput, which reads one
// byte past it so that "too large" and "exactly the bound" can be told apart.
// internal/s3's `do` shipped the other version: io.LimitReader truncated
// silently, and a cut-off listing page parsed as XML right up to where it did
// not, surfacing as a malformed-document error naming neither the size nor the
// cause.
//
// Eight mebibytes, and the number is about what ember RELAYS rather than about
// fidelity. The bytes are base64'd into a generation request that travels over
// the SSH channel to the node, so the wire cost is a third more than this
// again -- and sd-server resizes an init image to the generation's own width
// and height, measured against a live server: a 192x192 input asked for at
// 320x320 comes back 320x320 and nothing complains. So megapixels beyond the
// output are paid for twice, over the link and in the decode, and discarded.
const MaxInputBytes = 8 << 20

// MaxInputAxis bounds each side of the declared image.
//
// IT IS NOT A SIZE LIMIT; MaxInputBytes is. This is the decompression bomb,
// and it is load-bearing precisely BECAUSE ember never decodes the pixels: a
// two-kilobyte PNG can declare 100000x100000 in its header and pass every
// byte-count check there is, and the thing that would then allocate four
// bytes per pixel is sd-server, on a rented machine, after a card has been
// paid for. image.DecodeConfig reads the header and nothing else, so the
// declared geometry is available for free and the bound costs one comparison.
//
// 8192 is twice the largest generation dimension the captured capabilities
// publish (limits.max_width is 4096), which is the only published number
// anywhere near this and leaves room for an input larger than its output.
const MaxInputAxis = 8192

// refScheme prefixes a reference, so that a string in a stored job says what
// it is rather than being sixty-four characters of something.
const refScheme = "sha256:"

// Input is what Stage wrote, as a caller reports it back to a browser.
//
// The format and the geometry are here because the upload is the only moment
// ember knows them cheaply -- DecodeConfig has just read the header -- and a
// page that cannot say "that is a 1024x768 jpeg" is a page whose file picker
// gives no feedback at all. Nothing else reads them: the generation call needs
// only the bytes, because sd-server sniffs the format itself.
type Input struct {
	Ref    string
	Format string
	Width  int
	Height int
	Bytes  int
}

// ErrInputTooLarge is the bound being hit, as something a handler can turn
// into a 413 rather than a 400.
var ErrInputTooLarge = fmt.Errorf("the image is larger than ember will accept (%d bytes)", MaxInputBytes)

// ReadInput reads an upload, refusing anything past the bound.
//
// IT READS ONE BYTE PAST MaxInputBytes ON PURPOSE. io.LimitReader at the
// bound returns a short read that is indistinguishable from a body that
// happened to end there, so the caller cannot tell a refusal from a
// truncation -- and a truncated image is still an image to DecodeConfig if
// the header survived, which means ember would stage a file that is not the
// file somebody picked and the operator would find out as a generation that
// used the wrong pixels. Reading n+1 and comparing is what makes the two
// different answers.
func ReadInput(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the uploaded image: %w", err)
	}
	if len(b) > MaxInputBytes {
		return nil, ErrInputTooLarge
	}
	return b, nil
}

// ParseRef turns a reference into the key it names.
//
// REFUSED AND NEVER CLEANED, NEVER TRIMMED, NEVER LOWERCASED. Every one of
// those would be a repair, and a repair means two spellings of one reference
// -- so the digest in a stored job and the digest a client sent could differ
// while naming the same object, which is exactly how a check that compares
// them stops meaning anything. The set of valid references is the set Stage
// produces and nothing else.
func ParseRef(ref string) (string, error) {
	rest, ok := strings.CutPrefix(ref, refScheme)
	if !ok {
		return "", fmt.Errorf("image reference %q does not begin with %q; "+
			"a reference is the sha256 of the bytes, which is what POST /api/inputs returns",
			ref, refScheme)
	}
	if len(rest) != sha256.Size*2 {
		return "", fmt.Errorf("image reference %q carries %d characters of digest, want %d",
			ref, len(rest), sha256.Size*2)
	}
	// hex.DecodeString accepts upper case, so the length check above is not
	// the whole rule: "AB..." and "ab..." would be two references to one
	// object and only one of them would be the name Stage wrote.
	if strings.ToLower(rest) != rest {
		return "", fmt.Errorf("image reference %q is not lower case; the digest is "+
			"spelled one way so that two references cannot name one object", ref)
	}
	if _, err := hex.DecodeString(rest); err != nil {
		return "", fmt.Errorf("image reference %q is not hexadecimal: %w", ref, err)
	}
	return rest, nil
}

// Ref renders a digest as a reference.
func Ref(sum [sha256.Size]byte) string { return refScheme + hex.EncodeToString(sum[:]) }

// Inputs is the staging area: one directory, reached through an os.Root.
//
// A CONCRETE TYPE AND NOT AN INTERFACE, which is the opposite of Store above
// and deliberate. Store is an interface because a generated image is going to
// live in an S3 bucket one day -- different lifetime, different failure mode,
// and one seam to absorb it. A staged input is the other thing: it exists for
// the minutes between a file picker and an enqueue, on the machine running
// `ember serve`, and there is no second place for it to be. An interface here
// would be an interface introduced to raise a coverage number, which this
// repository already decided against.
type Inputs struct {
	root *os.Root
	name string
}

// OpenInputs opens or creates the staging directory and roots a store at it.
//
// 0700 as everywhere else: an uploaded image is the operator's and nobody
// else's business on a shared machine -- which is the same second-uid threat
// the serve credential is for, one layer down.
func OpenInputs(dir string) (*Inputs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the image staging directory %s: %w", dir, err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open the image staging directory %s: %w", dir, err)
	}
	return &Inputs{root: r, name: dir}, nil
}

// Close releases the root.
func (i *Inputs) Close() error { return i.root.Close() }

// Name is the directory this store is rooted at, for an error message.
func (i *Inputs) Name() string { return i.name }

// Stage checks that b is an image ember will relay and writes it under its
// own digest.
//
// THE ORDER IS THE CHEAP-REFUSAL ORDER the rest of this program uses: the
// bound, then the header, then the geometry, and only then anything that
// touches the disk. Nothing is written for a request that was going to be
// refused.
//
// WHY EMBER VALIDATES AT ALL, when it is a relay. sd-server does refuse a
// payload that is not an image -- measured, a 400 with
// `{"error":"invalid generation parameters"}` -- but that refusal arrives
// after a card has been rented, the image pulled and the weights loaded, and
// it names the request rather than the file. Here it costs one header read
// and happens while the operator is still looking at the file picker. The
// geometry bound is not available from sd-server at any price: it would be an
// allocation on the node.
func (i *Inputs) Stage(b []byte) (Input, error) {
	if len(b) > MaxInputBytes {
		return Input{}, ErrInputTooLarge
	}
	if len(b) == 0 {
		// A zero-byte upload is what a failed read leaves, and `verify:
		// present` on a store taught this repository that an empty file
		// reported as present loads as a model. Named rather than left to
		// DecodeConfig, whose answer for no bytes is "unknown format".
		return Input{}, errors.New("the upload carried no bytes")
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return Input{}, fmt.Errorf("the upload is not a png or a jpeg (%d bytes): %w", len(b), err)
	}
	// BOTH ENDS IN ONE CONDITION. The zero end is not reachable through
	// either registered decoder -- png and jpeg each refuse a zero axis
	// before this sees it -- and it is here because the alternative is a
	// second branch no test can drive, which is a branch nobody can be sure
	// of. One statement, so it is one rule rather than a rule with a dead one
	// beside it.
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxInputAxis || cfg.Height > MaxInputAxis {
		return Input{}, fmt.Errorf("the %s declares %dx%d and ember accepts 1 to %d on each "+
			"axis; the bytes are relayed to a generation server that decodes them on a rented "+
			"machine, so a small file declaring a huge image is an allocation ember would be "+
			"paying for", format, cfg.Width, cfg.Height, MaxInputAxis)
	}

	sum := sha256.Sum256(b)
	key := hex.EncodeToString(sum[:])
	if err := i.write(key, b); err != nil {
		return Input{}, err
	}
	return Input{
		Ref: refScheme + key, Format: format,
		Width: cfg.Width, Height: cfg.Height, Bytes: len(b),
	}, nil
}

// write puts the bytes under key, through the Root.
//
// FLAT, WITH NO FANOUT DIRECTORY. Two hex characters of prefix is the obvious
// shape and it would buy nothing here: Sweep bounds this directory to what
// one session uploaded and has to read it in one call, and a fanout would add
// empty parents for the sweep to prune -- the thing Dir.pruneDirs exists for,
// reintroduced for a directory that holds tens of files.
//
// 0600, as every other file ember writes.
func (i *Inputs) write(key string, b []byte) error {
	if err := i.root.WriteFile(key, b, 0o600); err != nil {
		return fmt.Errorf("write %s in %s: %w", key, i.name, err)
	}
	return nil
}

// Load reads the bytes a reference names and checks they are those bytes.
//
// THE RE-HASH IS THE POINT AND IT IS NOT BELT AND BRACES. A content-addressed
// reference is a claim: "the object called this is these bytes". Serving the
// file without checking would make the claim unverified, and the failure it
// admits is the one that matters -- anything that could write into this
// directory could leave different pixels under a digest a queued request
// already names, and the generation would use them. That is the store-as-a-
// channel-between-runs hazard one layer out, where assets are verified by
// digest before any GPU time is spent on them, and it is the same answer: a
// mismatch is reported, never repaired, because overwriting destroys the
// evidence and ember does not know what wrote it.
//
// It is also what makes "a request naming another request's input" harmless
// rather than merely unlikely: the reference cannot be pointed at bytes it
// does not describe.
//
// IT IS THE ONLY WAY TO ASK, and there is deliberately no cheaper
// presence-only check beside it. One was written -- a `Verify` that loaded
// and threw the bytes away -- and the two callers that would have used it
// both need the bytes anyway: `jobsFromRequest` because ValidateStatic
// builds the request body, and the enqueue handler for the same reason. It
// was production code only tests called, which is the thing
// TestNoProductionDeadCode exists to catch, and it caught it.
func (i *Inputs) Load(ref string) ([]byte, error) {
	key, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	f, err := i.root.Open(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s in %s: %w", ref, i.name, err)
	}
	defer func() { _ = f.Close() }()

	// Bounded by the same reader the upload went through, so a file that grew
	// after it was staged is refused rather than read: the digest would fail
	// anyway, and reading an unbounded amount first to find that out is the
	// thing the bound exists to prevent.
	b, err := ReadInput(f)
	if err != nil {
		return nil, fmt.Errorf("read %s in %s: %w", ref, i.name, err)
	}
	if got := Ref(sha256.Sum256(b)); got != ref {
		return nil, fmt.Errorf("the staged image %s hashes to %s: the bytes under that "+
			"reference are not the bytes it names. ember will not generate from them and does "+
			"not overwrite them, because what wrote them is not knowable from here; remove %s "+
			"and upload the image again", ref, got, i.name)
	}
	return b, nil
}

// Swept is what a sweep freed.
type Swept struct {
	Files int
	Bytes int64
}

// Sweep removes staged inputs that nothing is waiting on.
//
// WHAT CLEANS UP, AND WHY IT IS THIS AND NOT `ember history prune`. The
// history commands remove a request's rows and the images that request was
// billed for; a staged input is neither. It may belong to no request at all
// -- a file picked and then thought better of -- and the request it does
// belong to may be finished, in which case the input was an argument to
// something already paid for rather than a result worth keeping. So the rule
// is the staging area's own and does not reach into the history's: remove
// what no unfinished request names, once it is old enough that nothing can
// still be about to use it.
//
// TWO BOUNDS, AND BOTH ARE NEEDED.
//
// `keep` is the references of every request that has not finished. Without
// it a queued batch would lose the image it was queued with, and the failure
// would land after a card was rented.
//
// `before` is the age bound, and it is the rule `ember store abort` already
// follows: an upload in progress and an abandoned one are the same thing to
// the thing storing it. A file picked a second ago is not in any request yet
// -- the operator is still composing -- so a sweep with no age bound would
// delete exactly the input that is about to be used, and the symptom would be
// a file picker that works except when it does not. A deadline in the past is
// how `reap` bounds its own authority over nodes, for the same reason.
//
// Errors on an individual file are returned, not swallowed: unlike
// Dir.pruneDirs -- where a directory that would not go is the ordinary case
// -- a staged input that cannot be removed is bytes the operator is keeping
// without knowing it, which is the leak this function exists to end.
func (i *Inputs) Sweep(before time.Time, keep []string) (Swept, error) {
	names, err := i.list()
	if err != nil {
		return Swept{}, err
	}

	// The keys, not the references: what is on disk is the digest, and
	// comparing a filename against a "sha256:..." string would match nothing
	// and sweep everything a queued request was waiting on. Parsed rather
	// than trimmed, so a malformed entry in a stored job cannot silently
	// become a key that keeps nothing.
	keys := make([]string, 0, len(keep))
	for _, ref := range keep {
		if key, perr := ParseRef(ref); perr == nil {
			keys = append(keys, key)
		}
	}

	var out Swept
	for _, f := range names {
		if slices.Contains(keys, f.name) {
			continue
		}
		if !f.mod.Before(before) {
			continue
		}
		// os.ErrNotExist is tolerated for the reason Dir.Delete tolerates it:
		// the goal is that the file is not there, and it is not. Two uploads
		// can be in flight at once, and each sweeps -- so one of them
		// removing a file the other had just listed is ordinary rather than a
		// fault, and the bytes are still counted once because only one of the
		// two will have seen it before it went.
		if rerr := i.root.Remove(f.name); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return out, fmt.Errorf("remove the staged image %s in %s: %w", f.name, i.name, rerr)
		}
		out.Files++
		out.Bytes += f.size
	}
	return out, nil
}

// staged is one file the sweep may act on.
//
// THE AGE AND THE SIZE COME OFF THE DIRECTORY LISTING rather than from a
// second Stat per entry. Not for the syscall: it is that a Stat after the
// listing is a second answer about the same file, and the window between them
// is exactly where a concurrent upload's own sweep lives -- so the two could
// disagree and the one that decided to remove would be reading a different
// file's age from the one it listed.
type staged struct {
	name string
	mod  time.Time
	size int64
}

// list is the staged files, and only the ones Stage could have written.
//
// ANYTHING ELSE IN THE DIRECTORY IS LEFT ALONE. A subdirectory, a dotfile, an
// editor's backup, a file an operator put there: none of them is ember's to
// remove, and a sweep that took them would be a recursive delete driven by a
// directory name -- which is the hazard Store.Delete's own contract says it
// must not become. A name that is ALMOST a digest is left alone too, because
// the set of things this store wrote is the set ParseRef accepts and nothing
// wider.
func (i *Inputs) list() ([]staged, error) {
	d, err := i.root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", i.name, err)
	}
	defer func() { _ = d.Close() }()

	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", i.name, err)
	}
	out := make([]staged, 0, len(ents))
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		if _, perr := ParseRef(refScheme + e.Name()); perr != nil {
			continue
		}
		fi, ferr := e.Info()
		if ferr != nil {
			// A file that went away between the readdir and this call, which
			// a concurrent upload's sweep can do. Skipped rather than
			// reported: there is nothing left to remove and nothing to
			// count.
			continue
		}
		out = append(out, staged{name: e.Name(), mod: fi.ModTime(), size: fi.Size()})
	}
	return out, nil
}
