package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Legion112/ember/internal/config"
)

// Overrides are the values a caller REPLACES in a config, and the word is the
// whole of the type.
//
// `ember up --max-hourly-usd 999` RAISES the ceiling, because the operator
// typing it is the operator who set it. That is the opposite of
// queue.Narrow, which takes the smaller of the two because a queued request
// arrives from somewhere the operator is not -- and the two rules met in one
// program is exactly the kind of thing that gets copied to the wrong place.
// So there is ONE implementation of "a flag beats the file", here, and both
// callers of it are callers rather than copies: internal/cli's flag parser,
// and the privileged socket route, whose caller connect(2) has already proved
// is the operator.
//
// IT LIVES IN internal/app AND NOT IN internal/cli for that second caller.
// The logic was `cli.applyFlags`, unexported, so the socket route would have
// had to grow a second copy -- and a second copy of a money rule is the
// `queue.Cost` mistake with a ceiling in it. internal/webapi and
// internal/queue both already import this package; neither can import
// internal/cli, which is the shell.
type Overrides struct {
	// Provider, Image, GPU and MaxHourlyUSD are config scalars. An empty
	// string leaves the file's value alone, which is what "the flag was not
	// passed" means -- there is no way to blank a configured value from the
	// command line today and inventing one here would be a flag nobody asked
	// for.
	Provider     string
	Image        string
	GPU          string
	MaxHourlyUSD string

	// TTL replaces when positive. Zero leaves the config's, and a negative
	// one is refused by Validate rather than clamped: a negative deadline is
	// a figure nobody means, and the one thing ember must not do with a TTL
	// is guess.
	TTL time.Duration

	// Workload is which declared workload this run is for, and it is a
	// SELECTION rather than a replacement -- the one field on this type that
	// is not "a flag beats the file".
	//
	// It names an entry in the file's own `workloads:` list, and Apply hands
	// it to config.Select, which answers the configuration for that one
	// workload: the entry's `image:`, `images:`, `sd:` and `tts:` in place of
	// the top-level ones. So nothing here reaches past the operator's file --
	// a value that names no declared entry is refused rather than becoming an
	// image, which is why Apply can fail at all.
	//
	// EMPTY IS THE ONE WORKLOAD A FILE DESCRIBES, which is every file that
	// declares none. A file declaring more than one refuses an empty value,
	// because one run is one image and picking silently would mean watching
	// the wrong binary start.
	Workload string

	// Ports and Cmd replace the whole list when non-empty, never append.
	// Appending would mean a `--port` on the command line could not override
	// a configured one, only add to it, so the forward an operator was trying
	// to replace would still be open.
	Ports []string
	Cmd   []string
}

// Apply lets each set value beat the file.
//
// The `image` case takes the `images:` map with it, and that is the one
// non-obvious line: an operator pinning one image for one run is the first
// thing anybody reaches for when a run misbehaves, and leaving the map in
// place would mean -image silently did nothing on every config that has one.
// One field holds the answer, so there is no "which wins" rule at the
// decision site.
//
// IT CAN FAIL NOW, AND ONLY FOR THE SELECTION. `Workload` names an entry in
// the operator's own file and a name that is not there cannot be honoured, so
// this returns an error rather than silently leaving the top-level keys in
// place -- which would rent the wrong image. Every other field here replaces a
// scalar and cannot fail.
//
// THE SELECTION HAPPENS FIRST AND THE FLAGS BEAT IT, which is one order
// rather than two: Select reads the `workloads:` list, which no override
// touches, and then `-image` replaces whatever the entry named for the same
// reason it replaces whatever the file named. Doing it the other way round
// would mean the entry's image silently overwrote the flag, which is the
// defect `images:` already records for the map.
func (o Overrides) Apply(cfg *config.Config) error {
	selected, _, err := cfg.Select(o.Workload)
	if err != nil {
		return err
	}
	*cfg = selected

	if o.Provider != "" {
		cfg.Provider = o.Provider
	}
	if o.Image != "" {
		cfg.Image = o.Image
		cfg.Images = nil
	}
	if o.GPU != "" {
		cfg.GPU = o.GPU
	}
	if o.MaxHourlyUSD != "" {
		cfg.MaxHourlyUSD = o.MaxHourlyUSD
	}
	if o.TTL > 0 {
		cfg.TTL = o.TTL
	}
	if len(o.Ports) > 0 {
		cfg.Ports = o.Ports
	}
	if len(o.Cmd) > 0 {
		cfg.Cmd = o.Cmd
	}
	return nil
}

// RunAsk is one `ember up` command line as a value: the config file to read
// and what a flag replaces in it.
//
// IT EXISTS BECAUSE THE CHANNEL EXISTS. The shell path parses flags straight
// into an Overrides and never builds one of these; this is the shape that
// crosses the unix socket, which is why Validate's rules are about a value
// that arrived from somewhere rather than about a config.
//
// THE CONFIG PATH TRAVELS, AND THAT IS A DELIBERATE REVERSAL of a rule this
// repository wrote down: *"The config path comes from the `serve` flag and is
// never accepted over HTTP, or the browser would be naming which file sets
// its own limits."* That sentence is about a browser, and it is still true of
// the loopback port -- postRequest has no config field and cannot get one.
// This ask arrives on a unix socket at 0600, where connect(2) has already
// established that the caller is the operator's own account: the same
// authority as typing `ember up -config other.yaml` in their own shell, where
// nothing has ever stopped them. A channel the operating system authenticates
// is not a channel a page can reach, so the thing the rule was protecting is
// untouched. See internal/webapi/run.go, where the route is registered on the
// socket's mux alone and therefore does not exist on the port at all.
type RunAsk struct {
	Config    string
	Overrides Overrides
	Hold      time.Duration
}

// Validate refuses an ask that could only be a mistake.
//
// THE ABSOLUTE PATH IS THE ONE THAT MATTERS. `ember serve` runs from its own
// working directory, which is not the client's, so a relative `ember.yaml`
// would name a different file than the operator meant -- and the file in
// question sets the ceilings, the image and the store. Resolving it against
// the daemon's cwd would be a silently different configuration, which is the
// class of defect this repository keeps recording. The client calls
// filepath.Abs before it sends; this is what catches a hand-written curl.
func (a RunAsk) Validate() error {
	switch {
	case a.Config == "":
		return errors.New("a run needs a config file to read: " +
			"the image, the store, the ports and the sd.serve argv come from it, and this " +
			"channel carries only the values a flag replaces")
	case !filepath.IsAbs(a.Config):
		return fmt.Errorf("the config path %q is relative, and ember serve runs from its own "+
			"working directory rather than yours -- so it would read a different file than you "+
			"meant. Send an absolute path", a.Config)
	case a.Hold < 0:
		return fmt.Errorf("hold is %v, which is not a length of time anything can wait", a.Hold)
	case a.Overrides.TTL < 0:
		return fmt.Errorf("ttl is %v, and a node cannot be given a deadline in the past", a.Overrides.TTL)
	}
	return nil
}
