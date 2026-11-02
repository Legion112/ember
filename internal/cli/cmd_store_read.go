package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/s3"
)

// The read half of `ember store`, and why it is three commands.
//
// `ember store` had four verbs -- create, rm, push, sync -- and none of them
// read. So ember could put forty gigabytes on a store and then say nothing
// about it: not the names, not the sizes, not whether the asset a run is about
// to need is there. Two things followed, and the second costs money.
//
// An interrupted multipart upload keeps every part that arrived and bills for
// them by the gigabyte-month. It is not an object, so it is in no listing; the
// abort that exists runs from a defer on a context Ctrl-C cannot cancel, which
// covers Ctrl-C and covers neither SIGKILL, nor a panic, nor a laptop closing,
// nor an OOM kill. Until these commands existed nothing in ember could find
// one. That was the only money leak in the repository with no detection path:
// `ember ls` reports nodes and stores and an orphaned upload is invisible to
// both.
//
// And a store's contents were knowable only from a rented node, because assets
// are verified over the SSH connection the workload uses. The cheapest moment
// to learn that a 14 GB expert is half a file was after provisioning, the
// image pull and the mount had all been paid for; one HEAD per asset answers
// it for nothing.

// defaultUploadAge is how old an incomplete upload must be before ember calls
// it abandoned.
//
// A multipart upload in progress is indistinguishable from one whose process
// died: both are "started, not completed". So anything that acts on them has
// to be bounded by age, or `ember store abort` kills the push running in
// another terminal -- the same trap as a leftover network inside its deadline,
// where `local.Create` makes the network before the container and a live run
// looks exactly like wreckage.
//
// A day, because the bound has to clear a legitimate upload. README measures
// this link at 67 Mbps, which is eighty-one minutes for a 40 GB model set, and
// a slower one or a resumed one can be hours. Nothing is lost by waiting:
// the parts bill by the month, so a day of delay costs about a thousandth of
// what they would cost unnoticed.
const defaultUploadAge = 24 * time.Hour

// runStoreLs says what is on a store, and what is being billed for that is not
// on it.
func runStoreLs(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("store ls", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	olderThan := fs.Duration("older-than", defaultUploadAge,
		"how old an incomplete upload must be to count as abandoned")
	fs.Usage = func() {
		outf(out, "ember store ls lists what is on a store.\n\n")
		outf(out, "Nothing is rented: the store answers over its own S3 endpoint, so this\ncosts nothing and can be run before deciding to rent anything.\n\n")
		outf(out, "It also lists incomplete multipart uploads, which is the part worth\nhaving. An upload that was interrupted keeps every part that arrived and\nbills for them by the gigabyte-month -- and it is not an object, so it\nappears in no object listing and `ember ls` cannot see it either. Exit 2\nmeans some have been there longer than --older-than, which is ember\ncalling them abandoned rather than in flight.\n\n")
		outf(out, "usage: ember store ls [<name>] [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() > 1 {
		return usageError{fmt.Errorf("ember store ls takes at most one store name and got %d argument(s) (%v); "+
			"flags come before the name", fs.NArg(), fs.Args())}
	}

	reg, _, cfg, err := app.RegistryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	name, err := storeName(fs.Arg(0), cfg, *cfgPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	c, store, err := storeOverS3(ctx, reg, cfg, name, "list", d)
	if err != nil {
		return err
	}
	bucket := string(store.Handle.ID)

	outf(out, "%s", store)
	if m := store.Monthly(); m > 0 {
		outf(out, " at %s", m)
	}
	outf(out, "\n")

	objects, err := c.ListObjects(ctx, bucket, "")
	if err != nil {
		return err
	}
	// Sorted here rather than trusted from the endpoint. S3 returns keys in
	// lexicographic order and this one does too, but a listing an operator
	// compares between runs must not change order because a page boundary
	// moved.
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })

	if len(objects) == 0 {
		outf(out, "\nthe store holds no objects.\n")
	} else {
		outf(out, "\nobjects\n")
		var total int64
		for _, o := range objects {
			total += o.Size
			outf(out, "  %10s  %-20s  %s\n", human(o.Size), stamp(o.LastModified), o.Key)
		}
		outf(out, "  %10s in %d object(s)\n", human(total), len(objects))
	}

	ups, err := c.ListIncompleteUploads(ctx, bucket)
	if err != nil {
		return err
	}
	if len(ups) == 0 {
		// Said out loud rather than omitted. The clean case is the one an
		// operator is checking for, and silence reads as "this command does
		// not look at that".
		outf(out, "\nno incomplete uploads.\n")
		return nil
	}

	now := time.Now()
	sort.Slice(ups, func(i, j int) bool { return ups[i].Initiated.Before(ups[j].Initiated) })

	outf(out, "\nincomplete uploads\n")
	var bytes int64
	abandoned := 0
	for _, u := range ups {
		bytes += u.Size
		age := u.Age(now)
		mark := "in flight?"
		if age >= *olderThan {
			abandoned++
			mark = "abandoned"
		}
		outf(out, "  %10s  %3d part(s)  %-20s  %-10s  %s\n",
			human(u.Size), u.Parts, stamp(u.Initiated), mark, u.Key)
	}

	// Priced from bytes and not from whole gibibytes, because an orphan under
	// a gibibyte would otherwise print as free -- and a leak that reports as
	// costing nothing is a leak nobody removes.
	monthly := store.Price.MonthlyBytes(bytes)
	outf(out, "  %10s in %d upload(s)", human(bytes), len(ups))
	if monthly > 0 {
		outf(out, ", costing %s/month on top of the store itself", monthly)
	}
	outf(out, "\n")
	outf(out, "These are not objects: they are in no object listing and `ember ls` cannot\nsee them either. They bill until something removes them.\n")

	if abandoned == 0 {
		outf(out, "None is older than %s, so ember is treating them as uploads in progress.\n", *olderThan)
		return nil
	}
	// Agreeing with the number, because the single-orphan case is the common
	// one and "1 have been there" is what a reader notices instead of the
	// charge.
	been := "have"
	if abandoned == 1 {
		been = "has"
	}
	outf(out, "%d %s been there longer than %s. Remove %s with:\n  ember store abort %s\n",
		abandoned, been, *olderThan, plural(abandoned, "it", "them"), name)
	return &FindingsError{N: abandoned}
}

// runStoreAbort removes incomplete uploads that have been abandoned.
//
// Bounded by age and not by "every incomplete upload", because the two are
// indistinguishable at the protocol level: aborting an upload in progress
// destroys an hour of somebody's home link and reports success. The bound is
// the same one `ls` uses to decide what to call abandoned, so the command
// removes exactly what the listing flagged.
func runStoreAbort(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("store abort", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	olderThan := fs.Duration("older-than", defaultUploadAge,
		"only abort uploads older than this")
	dryRun := fs.Bool("dry-run", false, "say what would be aborted and abort nothing")
	fs.Usage = func() {
		outf(out, "ember store abort removes incomplete multipart uploads.\n\n")
		outf(out, "An interrupted upload keeps every part that arrived and bills for them\nby the gigabyte-month. It is not an object, so nothing else in ember can\nsee it: `ember store ls` is what finds them and this is what removes them.\n\n")
		outf(out, "Only uploads older than --older-than are touched. An upload in progress\nand an abandoned one are the same thing to S3 -- both are started and not\ncompleted -- so without the bound this would kill a push running in\nanother terminal. Nothing is lost by waiting: the parts bill by the month.\n\n")
		outf(out, "usage: ember store abort [<name>] [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() > 1 {
		return usageError{fmt.Errorf("ember store abort takes at most one store name and got %d argument(s) (%v); "+
			"flags come before the name", fs.NArg(), fs.Args())}
	}
	if *olderThan < 0 {
		// Refused rather than clamped. A negative bound reads as "no bound"
		// and would abort every upload including one in flight, which is the
		// one outcome this flag exists to prevent -- the same reason a
		// negative preview interval is refused instead of being treated as
		// "off".
		return usageError{fmt.Errorf("--older-than %s is negative; it is a minimum age, and a negative one "+
			"would abort an upload that is still running", *olderThan)}
	}

	reg, _, cfg, err := app.RegistryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	name, err := storeName(fs.Arg(0), cfg, *cfgPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	c, store, err := storeOverS3(ctx, reg, cfg, name, "abort an upload on", d)
	if err != nil {
		return err
	}
	bucket := string(store.Handle.ID)

	ups, err := c.ListIncompleteUploads(ctx, bucket)
	if err != nil {
		return err
	}

	now := time.Now()
	var doomed []s3.IncompleteUpload
	skipped := 0
	for _, u := range ups {
		if u.Age(now) < *olderThan {
			skipped++
			continue
		}
		doomed = append(doomed, u)
	}

	if skipped > 0 {
		outf(out, "leaving %d upload(s) newer than %s alone; they may still be running.\n", skipped, *olderThan)
	}
	if len(doomed) == 0 {
		outf(out, "nothing to abort.\n")
		return nil
	}

	var bytes int64
	for _, u := range doomed {
		bytes += u.Size
		outf(out, "  %10s  %3d part(s)  %-20s  %s\n", human(u.Size), u.Parts, stamp(u.Initiated), u.Key)
	}
	if *dryRun {
		outf(out, "dry-run: would abort %d upload(s) holding %s; nothing was touched\n", len(doomed), human(bytes))
		return nil
	}

	// Every one is attempted even after a failure. The whole point is that
	// these accumulate, so one upload the endpoint refuses must not leave the
	// rest of them billing -- the same reason reap keeps going past a
	// provider it could not reach.
	removed, freed := 0, int64(0)
	var firstErr error
	for _, u := range doomed {
		if err := c.Abort(ctx, bucket, u.Key, u.UploadID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
		freed += u.Size
	}

	outf(out, "aborted %d of %d upload(s), freeing %s\n", removed, len(doomed), human(freed))
	if firstErr != nil {
		return firstErr
	}
	if removed < len(doomed) {
		return &FindingsError{N: len(doomed) - removed}
	}
	return nil
}

// runStoreVerify checks that the configured assets are on the store.
//
// Before a card is rented, which is the entire point. Assets are verified by
// digest on the node, over the SSH connection the workload uses -- so the
// cheapest moment to discover that a 14 GB expert was never uploaded is
// currently after provisioning, the image pull and the mount have all been
// paid for. One HEAD per asset answers it over the store's own endpoint for
// nothing.
//
// What it does not do is check the digest, and that is deliberate rather than
// unfinished. A digest means reading the object back: forty gigabytes through
// this process, over a home link, to re-derive a number the node is about to
// derive anyway at datacentre bandwidth. So this answers "is it there, and is
// it a plausible file" and says so in those words; the node still answers "is
// it the right bytes" before any workload starts.
func runStoreVerify(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("store verify", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	fs.Usage = func() {
		outf(out, "ember store verify checks that every configured asset is on the store.\n\n")
		outf(out, "One HEAD per asset over the store's own S3 endpoint. Nothing is rented\nand nothing is downloaded, so this is the cheap way to find out that a\nrun is about to fail -- today that is discovered on a node which has\nalready been provisioned, pulled and mounted, all of it billed.\n\n")
		outf(out, "It does not check digests. That would mean reading every object back\nthrough this process to re-derive a number the node derives anyway, at\ndatacentre bandwidth, before the workload starts. This answers whether\nthe files are there; the node answers whether they are the right bytes.\n\n")
		outf(out, "Exit 2 means at least one asset is missing or empty.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember store verify takes no arguments; it checks what %s lists", *cfgPath)}
	}

	// One config read, not two. RegistryAndStore hands the config back
	// precisely so a caller that needs it does not reload it -- two reads are
	// two chances for the copies to disagree about which store this is, and
	// here the disagreement would be checking one store's assets against
	// another store's contents.
	reg, _, cfg, err := app.RegistryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	assets, err := cfg.ParseAssets()
	if err != nil {
		return err
	}
	if len(assets) == 0 {
		return usageError{fmt.Errorf("no assets are listed in %s, so there is nothing to check", *cfgPath)}
	}

	ctx := context.Background()
	c, store, err := storeOverS3(ctx, reg, cfg, cfg.Store.Name, "check", d)
	if err != nil {
		return err
	}
	bucket := string(store.Handle.ID)

	findings := 0
	for _, a := range assets {
		o, found, err := c.Head(ctx, bucket, a.As)
		if err != nil {
			// Not counted as a missing asset. "I could not ask" and "it is
			// not there" are different answers, and the second would send an
			// operator to re-upload forty gigabytes over a credential
			// problem.
			return err
		}
		switch {
		case !found:
			outf(out, "missing   %-40s fetch it with `ember store sync`, or push it\n", a.As)
			findings++
		case o.Size == 0:
			// A present but empty object is what a PUT that failed after
			// creating the key leaves behind. Under `verify: present` the
			// node would find it, call it there, and load nothing.
			outf(out, "empty     %-40s it is on the store at 0 bytes, so it uploaded as nothing\n", a.As)
			findings++
		default:
			outf(out, "present   %-40s %10s  %s\n", a.As, human(o.Size), stamp(o.LastModified))
		}
	}

	if findings == 0 {
		outf(out, "\nall %d asset(s) are on %s. Their digests are checked on the node, before the workload.\n",
			len(assets), store.Name)
		return nil
	}
	outf(out, "\n%d of %d asset(s) are not usable. A run would fail on the node, after paying for it.\n",
		findings, len(assets))
	return &FindingsError{N: findings}
}

// storeName is the store to act on: the argument, or the configured one.
//
// The argument wins so a second store can be inspected without editing a
// config, and the config is the default so the ordinary case is one word. An
// empty config and no argument is a usage error rather than a guess -- there
// is no "the store" when nothing has named one.
func storeName(arg string, cfg config.Config, cfgPath string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if !cfg.HasStore() {
		return "", usageError{fmt.Errorf("no store is named in %s and none was given; "+
			"name one as `ember store ls <name>` or add a store block", cfgPath)}
	}
	return cfg.Store.Name, nil
}

// plural picks a word to agree with n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// stamp formats a listing timestamp, or says it was not reported.
//
// A zero time is printed as a dash rather than as Go's year-1 default, which
// in a column of real dates reads as a file from before the epoch.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// human is the same unit formatting internal/s3 uses on an upload.
//
// Duplicated rather than exported from there, because it is a presentation
// choice of this command and not a property of the protocol -- and an
// exported helper would be an invitation for the two to drift into one
// format that suits neither.
func human(n int64) string {
	// SIGNED, because one caller's figure is legitimately negative: `ember
	// history --memory` prints the headroom a node had under its memory
	// limit, and a node that could not hold the weights is the interesting
	// answer. Without this it rendered as "-25769803776 B" -- unreadable in
	// exactly the row an operator is looking for, and found by printing it
	// rather than by a test.
	//
	// The magnitude is taken in uint64 so math.MinInt64 cannot overflow back
	// to itself, which as a recursive "-" + human(-n) would not terminate. No
	// byte count reaches it; a formatter that can loop is still worse than
	// one that cannot.
	mag, sign := uint64(n), "" //nolint:gosec // G115: a negative n is converted deliberately and negated below
	if n < 0 {
		mag, sign = -mag, "-"
	}
	switch {
	case mag >= 1<<30:
		return fmt.Sprintf("%s%.1f GiB", sign, float64(mag)/float64(uint64(1)<<30))
	case mag >= 1<<20:
		return fmt.Sprintf("%s%.1f MiB", sign, float64(mag)/float64(uint64(1)<<20))
	case mag >= 1<<10:
		return fmt.Sprintf("%s%.1f KiB", sign, float64(mag)/float64(uint64(1)<<10))
	default:
		return fmt.Sprintf("%s%d B", sign, mag)
	}
}
