package main

import (
	"log/slog"
	"os"

	"github.com/Legion112/ember/internal/node"
)

// logKeys records the two keys embernode installed.
//
// FINGERPRINTS AND NEVER THE MATERIAL. node.Secret exists because "one
// slog.String("hostkey", string(pem)) at debug level writes a private key
// into the journal permanently, and nothing about that line looks wrong in
// review" -- and while this program printed to stderr alone, such a line went
// into a stream nothing in ember read. It now goes into a file ember copies
// onto the operator's disk beside the images, which is the same failure one
// hop further out and durable, so the two values go through the type that
// cannot print itself.
//
// IT IS ITS OWN FUNCTION SO THAT A TEST CAN HOLD IT. The rest of this program
// starts sshd and becomes PID 1, so the line that logs these cannot be
// reached by a unit test where it is written -- and internal/logx's walk
// catches `.Bytes()` inside a log call but cannot catch
// `os.Getenv(envHostKey)` passed straight in, which is the shorter and more
// natural spelling of the same mistake. keys_test.go is what refuses that
// one, with no Docker and no node.
//
// WHY LOG THEM AT ALL. "Did the node install the key ember thinks it
// injected" is the question a host-key mismatch raises, and a mismatch is a
// hard error with no --force: the fingerprint here is the only thing that can
// say whether the node had the right key or the wrong one, on a machine that
// no longer exists by the time anybody asks.
func logKeys(log *slog.Logger) {
	// SPELLED INLINE AT THE LOG CALL, AND THAT IS NOT INCIDENTAL.
	//
	// The first version built these in a keyAttrs() helper returning []any,
	// which reads better and defeated the walk: internal/logx matches on the
	// argument list of a log call, so an unwrapping laundered through a
	// helper that merely BUILDS the attributes is invisible to it. The
	// reinstatement pass found that -- the test went green with
	// `string(...Bytes())` in the helper -- which is the whole argument for
	// reinstating a defect rather than trusting the shape of the guard.
	log.Info("keys installed",
		"hostkey", node.NewSecret([]byte(os.Getenv(envHostKey))),
		"authorized_key", node.NewSecret([]byte(os.Getenv(envAuthorized))))
}
