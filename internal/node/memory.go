package node

// Memory is what a node said about the bound its page cache lives under, and
// the weights that had to fit in it.
//
// IT IS ONE RULE IN ONE PLACE, AND IT WAS TWO FOR THE LIFE OF THE FEATURE.
// `nodemem.Report.Limit`/`Headroom` and `genstore.Entry.MemCeiling`/
// `MemHeadroom` were the same four lines on either side of a SQLite round
// trip, written in the same commit, and only the second pair was ever called:
// `ember history --memory` reaches `Entry.MemHeadroom`, and nothing anywhere
// reached `Limit` but the dead `Headroom` beside it. AGENTS.md named `Limit()`
// as where the dash-versus-zero decision lived, so a reader who changed it
// changed nothing an operator sees -- the `dataCenter`-against-`dataCenterId`
// shape, where two copies of one answer stay in step only because nobody
// exercises one of them.
//
// WHAT MADE IT MORE THAN TIDINESS IS THAT THE TESTS WERE SPLIT ACROSS THE TWO
// COPIES, each proving the half the other did not. The dead pair's test was
// the only thing anywhere asserting that a ceiling with no asset figure
// answers "unknowable"; the live pair's tests were the only thing asserting
// the fallback. Measured rather than argued: deleting `|| e.AssetBytes <= 0`
// from the live `MemHeadroom` leaves `go test ./...` entirely green, so half
// of the rule an operator reads was covered by a test of code that never ran.
//
// AND THE SECOND COPY CANNOT COME BACK QUIETLY, WHICH IS MEASURED RATHER THAN
// HOPED FOR. Re-adding the pair to nodemem.Report fails
// `TestNoProductionDeadCode` as a symbol nothing calls, and re-implementing
// the subtraction inside genstore.Entry fails it from the other side -- these
// methods lose their only production caller and are reported in turn. So the
// one implementation is held in place by the detector rather than by this
// comment.
//
// IT LIVES HERE BECAUSE THAT COSTS NO IMPORT. `internal/nodemem` (which
// produces the figures) and `internal/genstore` (which stores them and is
// where the operator reads them) both already import this package, and this
// package imports nothing from the module -- so one implementation needs no
// new edge in either direction. Having `genstore` import `nodemem` instead
// would drag `sshx`, `workload` and `x/crypto/ssh` into the store for four
// lines of arithmetic, which is the argument the `internal/node` seal is
// written about ("the moment it imports a sibling, the sibling's dependencies
// become its own") pointed the other way.
//
// THE FIELDS ARE NAMED BECAUSE TWO int64 PARAMETERS WOULD NOT BE.
// `Ceiling(limit, total)` and `Ceiling(total, limit)` both compile, and the
// swap is this rule exactly inverted -- a node bounded at 512 MiB on a host
// with 125.7 GiB would report the host's figure as its ceiling, which is the
// answer that makes a slow node look fine. Preferring an unrepresentable bug
// over a test for it, as everything else here does.
type Memory struct {
	// CgroupLimit is the container's own memory limit, and zero means there
	// is none rather than a limit of nothing: cgroup v2 writes the literal
	// "max" and v1 a sentinel near the top of int64, and nodemem's parser
	// turns every spelling of "unlimited" into zero.
	CgroupLimit int64

	// HostTotal is /proc/meminfo's MemTotal, which inside a container is the
	// HOST's -- the view that made the original measurement look like it had
	// ruled memory out, by a factor of 252 on a container bounded at 512 MiB.
	// Zero means nobody read it.
	HostTotal int64

	// Assets is the size of the weights this run is about to read, summed
	// over the asset set rather than measured on the store: a store can hold
	// other runs' models, and the question is whether THIS model fits. Zero
	// means nobody read it, which is why Headroom returns a bool.
	Assets int64
}

// Ceiling is the bound the page cache actually lives under.
//
// The cgroup's when there is one, the host's otherwise -- which is the whole
// hypothesis about the unexplained 81 seconds per image in one method. Zero
// when neither was reported, because a limit of zero bytes and an unanswered
// question are different things and only one of them means the weights cannot
// be held.
//
// ZERO IS "NO LIMIT" AND IT MUST NOT READ AS "A LIMIT OF NOTHING", which is
// what falling through to the host's total is for. The opposite reading --
// treating an unlimited cgroup as a container that cannot allocate anything --
// is the one that inverts every comparison made against it.
func (m Memory) Ceiling() int64 {
	if m.CgroupLimit > 0 {
		return m.CgroupLimit
	}
	return m.HostTotal
}

// Headroom is the ceiling less the weights, and whether it is knowable.
//
// Negative is a real answer and the interesting one: a node that could not
// hold the model re-read it every step, at 868 MiB/s off a network store in
// the one measurement there is. The bool is false when either figure is
// missing, because a headroom computed over a number nobody read is the kind
// that gets believed -- and a row of figures that reads as "this node was
// fine" is the misreading this whole surface exists to prevent.
func (m Memory) Headroom() (int64, bool) {
	c := m.Ceiling()
	if c <= 0 || m.Assets <= 0 {
		return 0, false
	}
	return c - m.Assets, true
}
