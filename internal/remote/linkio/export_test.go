package linkio

import (
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// offerExpiry is when the current offer of s expires (zero when none).
func (c *Carrier) offerExpiry(s protocol.Snapshot) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if o := c.offers[s.RequestRef][s.Revision]; o != nil {
		return o.expires
	}
	return time.Time{}
}
