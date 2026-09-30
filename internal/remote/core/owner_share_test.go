package core_test

import "github.com/avivsinai/agent-message-queue/internal/remote/core"

// ownerShare is the source of the owner's Buzz share: only it may answer an
// interaction of a request it submitted (agent-message-queue-611.46).
var ownerShare = core.Source{Host: "local", Origin: map[string]string{"carrier": "buzz", "body": "body-1", "channel": "dm-1"}}
