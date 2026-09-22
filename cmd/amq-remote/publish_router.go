package main

import (
	"errors"
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// errCarrierUnavailable is returned for a record whose carrier is not
// running in this process. The endpoint then keeps the revision owed
// instead of advancing its published-revision marker.
var errCarrierUnavailable = errors.New("carrier unavailable")

type publishFunc = core.Publisher

// publishRouter is the one core.Publisher: each record's publication goes to
// the carrier named by its persisted origin (relay design §5). No origin is
// a local IPC or CLI record: callers poll or wait, and nothing is
// published. "amq" is the AMQ mailbox carrier and "buzz" the Buzz carrier.
// A named carrier that is not running, or an unknown one, is an error,
// never nil, so an offline carrier's result cannot be marked published.
// There is no broadcast across carriers.
func publishRouter(carriers map[string]publishFunc) publishFunc {
	return func(s protocol.Snapshot, origin map[string]string) error {
		name := origin["carrier"]
		if name == "" {
			return nil
		}
		pub := carriers[name]
		if pub == nil {
			return fmt.Errorf("%w: %q for request %s", errCarrierUnavailable, name, s.RequestRef)
		}
		return pub(s, origin)
	}
}
