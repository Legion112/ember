// Package nodemem records what a rented node says about its memory.
//
// IT EXISTS BECAUSE OF 81 SECONDS AND $0.056 PER IMAGE THAT NOBODY COULD
// EXPLAIN. The FLUX.2 measurement in AGENTS.md rented the same card in the
// same datacentre eight times with the same prompts and seeds, and the `cmds`
// arm split in two: on a node that kept the 56 GiB of weights in page cache
// each step took 145.3s, and on one that did not, 226.6s, because every step
// re-read the whole model off the network store at a measured 868 MiB/s. All
// eight nodes reported 620-708 GiB of free RAM, and the caching run was the
// last of the three rather than the first, so neither host memory nor warm-up
// explains it.
//
// The leading guess written down there is the pod's own memory cgroup rather
// than host RAM, because `free` inside a container reports the HOST's -- and
// the file that would settle it, /sys/fs/cgroup/memory.max, is two reads on a
// node that is already rented and billing. Nothing read it. So "the next
// rental settles it" was true only if somebody remembered to type one command
// at the right moment, before the run that would have been the evidence.
//
// WHAT THIS IS NOT. It does not decide anything and it cannot fail a run:
// these are reads of four files and a stat of the weights, and a node that
// will not answer them is a node whose generation should still happen. The
// issue that asked for this was explicit that what to DO with the numbers is
// a separate decision, to be made once there are some -- a warning naming the
// expected re-read cost per step if the limit is reliably below the asset set,
// and a corrected note in AGENTS.md if it is not. Both need a rental this
// package has not had yet.
//
// BOTH VIEWS, ALWAYS. The cgroup's limit and the host's MemTotal are recorded
// side by side because the whole point of the measurement is that they
// disagree and that only one of them bounds the page cache. Recording the one
// ember currently believes would be the `load_elapsed_seconds` mistake again:
// a single figure, believed, that was wrong by two orders of magnitude.
package nodemem

import (
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/workload"
)

// Report is what one node said about its memory.
//
// Bytes throughout, although /proc/meminfo reports kibibytes: one unit above
// this package, so nothing downstream has to remember which fields were
// scaled. Zero means "not reported", which is a different claim from zero
// bytes and is why node.Memory says so rather than computing over it.
type Report struct {
	// CgroupVersion is 2, 1, or 0 when neither hierarchy could be read.
	CgroupVersion int

	// CgroupMax is the container's own memory limit, and zero means there is
	// none -- cgroup v2 writes the literal "max" and v1 writes a sentinel
	// near the top of int64.
	CgroupMax int64

	// CgroupCurrent is what the container is using now, page cache included.
	CgroupCurrent int64

	// HostTotal and HostCached are /proc/meminfo's MemTotal and Cached: the
	// view `free` gives inside a container, which is the host's and is what
	// made the original measurement look like it had ruled memory out.
	HostTotal  int64
	HostCached int64

	// Assets is the size of the weights this run is about to read, summed
	// over the asset set rather than measured on the store: a store can hold
	// other runs' models, and the question is whether THIS model fits.
	Assets int64
}

// THE DERIVED ANSWERS ARE node.Memory's AND THERE IS NO SECOND COPY HERE.
// This package had a Limit and a Headroom of its own, four lines each, and
// nothing ever called either: the pair an operator reaches is on the stored
// row, through `ember history --memory`, and these two were the same rule on
// the other side of a SQLite round trip. They agreed with the live pair for
// the life of the feature because one of them never ran -- and their tests
// were split, so each copy proved the half the other did not, which is how
// `go test ./...` stayed green with half the live rule deleted.
//
// So whoever writes the verdict this package defers -- a warning naming the
// expected re-read cost per step for a node that cannot hold the weights --
// builds a node.Memory{CgroupLimit: r.CgroupMax, HostTotal: r.HostTotal,
// Assets: r.Assets} and asks it, rather than adding the subtraction back
// here. A comment where somebody would add the second copy, as httpx's
// no-retry note is.

// IsZero reports whether nothing at all was read.
func (r Report) IsZero() bool { return r == Report{} }

// unlimited is where a cgroup v1 limit stops being a limit.
//
// The v1 hierarchy has no "max": an unlimited memory controller reports
// PAGE_COUNTER_MAX, which is 9223372036854771712 on a 4 KiB-page kernel --
// int64's top with the low page bits cleared. The exact value is kernel
// internals, so the test is an order of magnitude rather than equality: no
// machine has an exbibyte of RAM, and anything claiming to is saying "no
// limit" in the only way v1 can.
const unlimited = int64(1) << 62

// Script is the probe, and it is deliberately one round trip.
//
// Every value is a read: four files that may or may not exist, two lines of
// /proc/meminfo, and a stat per asset. Nothing is written and nothing can
// exit non-zero in a way that matters, because a node that will not answer
// is a node whose generation should still happen.
//
// THE ASSET PATHS ARE ARGUMENTS AND NOT SCRIPT TEXT, through "$@". A remote
// command is argv and every word is quoted -- so a path that reached the
// script body would be shell text assembled from a config file, which is the
// injection sshx.Quote exists to prevent. As positional parameters they
// cannot be anything but paths, and the quoting is sshx.Cmd's as it is for
// every other remote command here.
//
// The v1 paths are named rather than derived from /proc/self/cgroup, because
// a guess at where a hierarchy is mounted is a guess that reports "no limit"
// on the one kernel it is wrong about -- and "no limit" is the answer that
// makes a node look fine.
const Script = `
read_file() {
	if [ -r "$2" ]; then
		printf '%s %s\n' "$1" "$(cat "$2" 2>/dev/null)"
	fi
}
read_file cgroup2_max /sys/fs/cgroup/memory.max
read_file cgroup2_current /sys/fs/cgroup/memory.current
read_file cgroup1_max /sys/fs/cgroup/memory/memory.limit_in_bytes
read_file cgroup1_current /sys/fs/cgroup/memory/memory.usage_in_bytes
if [ -r /proc/meminfo ]; then
	awk '/^MemTotal:/ { print "mem_total_kb", $2 } /^Cached:/ { print "mem_cached_kb", $2 }' /proc/meminfo
fi
total=0
for f in "$@"; do
	size=$(stat -c %s "$f" 2>/dev/null) || size=0
	total=$((total + size))
done
printf 'assets %s\n' "$total"
`

// Command is the probe as argv, with one path per asset.
func Command(paths []string) []string {
	// $0 is named rather than left empty, so a `ps` on the node during a run
	// says which program is reading its cgroup files.
	cmd := []string{"sh", "-c", Script, "ember-nodemem"}
	return append(cmd, paths...)
}

// AssetPaths is where this run's weights will be, in the order the spec names
// them.
//
// Empty for a run with no assets, which is not an error: a baked image or a
// local container has nothing on a store, and the memory facts are still
// worth having -- the asset figure is simply unreported, which
// node.Memory.Headroom says rather than treating as zero bytes of weights.
func AssetPaths(mount node.Mount, assets []node.Asset) ([]string, error) {
	if mount.IsZero() || len(assets) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(assets))
	for _, a := range assets {
		v, err := a.Validate()
		if err != nil {
			return nil, err
		}
		out = append(out, path.Join(mount.Path(), v.As))
	}
	return out, nil
}

// Probe reads the node's memory facts over an existing connection.
//
// The output is parsed rather than grepped, as everything else here is, and
// an unknown key is ignored: this script and this parser travel together, but
// a node image with a wrapper that prints a banner must not turn a read into
// a failure.
func Probe(ctx context.Context, r sshx.Runner, paths []string) (Report, error) {
	var buf strings.Builder
	res, err := workload.Run(ctx, r, Command(paths), &limited{w: &buf, n: maxOutput})

	// THE SHAPE IS gpucheck.Wait's, and the first version of it was wrong in a
	// way coverage found: it tested err first and then res.ExitCode, which is
	// a branch nothing can reach, because workload.Run returns an error for a
	// non-zero exit as well as for a transport failure. The two have to be
	// told apart by the code, not by the error being nil.
	switch {
	case err == nil && res.ExitCode == 0:
		return Parse(buf.String()), nil
	case res.ExitCode > 0:
		// The node's own complaint, because "exit 127" and nothing else is a
		// diagnostic nobody can act on -- and this one is about a file that
		// was not where ember looked for it.
		return Report{}, fmt.Errorf("the node's memory probe exited %d: %s",
			res.ExitCode, strings.TrimSpace(buf.String()))
	default:
		return Report{}, fmt.Errorf("read the node's memory facts: %w", err)
	}
}

// maxOutput caps what is kept. The script prints seven short lines; anything
// printing more than this is a node doing something else, and holding it in
// memory to maybe record it would be the bug.
const maxOutput = 8 << 10

// Parse reads the probe's output.
//
// v2 wins over v1 where both answered, because a system running the unified
// hierarchy may still mount a v1 compatibility tree whose numbers describe
// nothing the container is actually bounded by.
func Parse(s string) Report {
	var (
		r              Report
		v1Max, v1Cur   int64
		v1Seen, v2Seen bool
		v2Max, v2Cur   int64
	)
	for _, line := range strings.Split(s, "\n") {
		// Cut before trimming, not after. The script prints "key value", and
		// an empty FILE produces "key " with nothing after the space --
		// trimming first collapses that to "key", the cut then fails, and a
		// hierarchy that was readable and empty becomes indistinguishable
		// from one that was not there. Those are different answers: the
		// second is the one that has to read as "nobody looked".
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "cgroup2_max":
			v2Seen = true
			v2Max = parseLimit(value)
		case "cgroup2_current":
			v2Seen = true
			v2Cur = parseBytes(value)
		case "cgroup1_max":
			v1Seen = true
			v1Max = parseLimit(value)
		case "cgroup1_current":
			v1Seen = true
			v1Cur = parseBytes(value)
		case "mem_total_kb":
			r.HostTotal = parseBytes(value) * 1024
		case "mem_cached_kb":
			r.HostCached = parseBytes(value) * 1024
		case "assets":
			r.Assets = parseBytes(value)
		}
	}
	switch {
	case v2Seen:
		r.CgroupVersion, r.CgroupMax, r.CgroupCurrent = 2, v2Max, v2Cur
	case v1Seen:
		r.CgroupVersion, r.CgroupMax, r.CgroupCurrent = 1, v1Max, v1Cur
	}
	return r
}

// parseLimit reads a memory limit, where "no limit" has three spellings.
//
// cgroup v2 writes the literal "max" -- observed, in
// testdata/container_unlimited.txt, not assumed; v1 writes a sentinel near the
// top of int64; and an unreadable file is an empty value. All three mean the
// same thing to a caller and all three are zero here -- which
// node.Memory.Ceiling then reads as "fall back to the host's total" rather
// than as a limit of nothing.
//
// The "max" case is deliberately redundant with parseBytes, which returns zero
// for anything that is not a number and would reach the same answer. It is
// written out because an intended behaviour implemented by a parse failure is
// an accident that happens to work, and the next person to touch parseBytes
// would have no way to know this depended on it. Reinstating its removal
// therefore changes no answer, which is why there is no test for it alone --
// the captured fixture is what proves the real spelling comes out as no limit.
func parseLimit(s string) int64 {
	if s == "" || s == "max" {
		return 0
	}
	n := parseBytes(s)
	if n >= unlimited {
		return 0
	}
	return n
}

// parseBytes reads a non-negative count, or zero.
//
// A value that is not a number is zero rather than an error, because the
// alternative is a probe that fails a run over a kernel printing something
// unexpected in a file ember only reads for information.
func parseBytes(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// limited is gpucheck's bounded writer, for the same reason it has one.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	if len(p) > l.n {
		_, err := l.w.Write(p[:l.n])
		l.n = 0
		return len(p), err
	}
	l.n -= len(p)
	_, err := l.w.Write(p)
	return len(p), err
}
