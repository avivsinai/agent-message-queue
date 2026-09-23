package relay

import "time"

// SetBackoffForTest shortens a client's reconnect backoff in tests.
func SetBackoffForTest(c *Client, min, max time.Duration) { c.minBackoff, c.maxBackoff = min, max }
