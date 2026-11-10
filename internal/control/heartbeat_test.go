package control

import (
	"encoding/json"
	"testing"
	"time"
)

// TestHeartbeat_StaleOutlastsAStalledWrite.
//
// THE RELATIONSHIP BETWEEN THREE CONSTANTS, AND IT IS THE ONE THING ABOUT THE
// HEARTBEAT THAT CAN BE WRONG WITHOUT ANYTHING FAILING. Sends on this channel
// are serialised and each is bounded at SendTimeout, so a beat queued behind a
// stalled write -- a 4 MiB preview frame up a slow uplink, which is the
// largest thing a worker sends -- arrives up to that late through no fault of
// the node. If HeartbeatStale sat inside that window, a busy rental would be
// reported as not answering, an operator would learn that the word means
// "busy" sometimes, and they would stop acting on it -- which is
// reconcile.KindStore's own argument about teaching people to ignore exit 2,
// reached through a different number.
//
// SO THE FLOOR IS SendTimeout PLUS ONE INTERVAL: the longest a beat can be
// held up, plus the beat that was due during the hold-up. Anything at or below
// that is a threshold that fires on a working node.
//
// AND THE CEILING IS SAID TOO, because a threshold can be wrong in the other
// direction and that direction costs money: a wedged rental is billing, and a
// bound an operator has to wait minutes for is one they will have paid for
// before they can see it. A minute is the figure -- that is twelve of the
// worker's own slowest polls, so a node that is still working has had every
// chance to say so.
func TestHeartbeat_StaleOutlastsAStalledWrite(t *testing.T) {
	floor := SendTimeout + HeartbeatEvery
	if HeartbeatStale <= floor {
		t.Errorf("HeartbeatStale is %v and a beat can legitimately be %v late "+
			"(SendTimeout %v behind a stalled write, plus the %v interval that fell inside it). "+
			"A threshold inside that window reports a BUSY rental as one that is not answering, "+
			"which is the direction this must not be wrong in.",
			HeartbeatStale, floor, SendTimeout, HeartbeatEvery)
	}
	if HeartbeatStale > time.Minute {
		t.Errorf("HeartbeatStale is %v. A wedged rental is billing the whole time it is wedged, "+
			"so a threshold an operator has to wait minutes for is one they pay for before they "+
			"can see it.", HeartbeatStale)
	}
	// The beat has to be frequent enough that the threshold is crossed by
	// several missed beats rather than by one. A single dropped frame is a
	// transport event; three in a row is a worker.
	if HeartbeatStale < 3*HeartbeatEvery {
		t.Errorf("HeartbeatStale (%v) is fewer than three beats at %v, so one lost frame "+
			"could cross it", HeartbeatStale, HeartbeatEvery)
	}
}

// TestHeartbeat_CarriesNothingButItsKind.
//
// THE FRAME'S WHOLE CONTENT IS THE INSTANT EMBER RECEIVED IT, and this is the
// exit code behind that sentence rather than a comment asserting it. The
// tempting payload is the job the worker has on the card: Index and Total are
// already on the struct, so it costs nothing to add and it would be a second
// answer to a question ember answers from its own bookkeeping -- ember pushed
// the batch. A page preferring the node's number would be believing a rented
// machine about ember's own accounting, and a beat with a payload invites the
// payload to be read as current when what is being reported is its age.
//
// ON THE BYTES AND NOT ON THE STRUCT, because `omitempty` is what makes this
// checkable at all: a field somebody sets to a non-zero value appears in the
// encoding, and this asserts the encoding is the kind and nothing else.
func TestHeartbeat_CarriesNothingButItsKind(t *testing.T) {
	body, err := json.Marshal(Frame{Kind: KindHeartbeat})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"kind":"heartbeat"}`
	if string(body) != want {
		t.Errorf("a heartbeat encodes as %s, want %s.\nThe whole content of this frame is when "+
			"ember received it. A field beside that instant is one somebody renders without "+
			"asking how old it is.", body, want)
	}
}

// TestHeartbeat_IsAVersionedChangeAndNotAnAdditiveOne.
//
// A NEW FRAME KIND IS NOT BACKWARD COMPATIBLE ON THIS CHANNEL, and the version
// is what makes the refusal cheap. Both of ember's frame loops treat a kind
// they do not know as a protocol disagreement and fail the rental -- correctly,
// because a frame nobody handles is a job ember would wait for forever -- so a
// node image that beats, driven by an ember that predates beats, fails its
// first batch AFTER the card is allocated and the weights are resident.
//
// This states the rule where a reader adding the next kind will trip over it:
// version 1 is the protocol without heartbeats and this build is not it.
func TestHeartbeat_IsAVersionedChangeAndNotAnAdditiveOne(t *testing.T) {
	if Version < 2 {
		t.Errorf("control.Version is %d. KindHeartbeat is a frame an older ember refuses in the "+
			"middle of a batch, so the build that introduced it had to state a new version -- "+
			"the handshake is the only moment that refusal costs nothing.", Version)
	}
}
