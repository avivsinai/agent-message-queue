package core

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// A retry signed for a different share never runs under the stored record's
// binding: its dialogs would land in the wrong dialog directory.
func TestConflictWithComparesTheSignedBinding(t *testing.T) {
	notAfter := protocol.FormatTime(time.Now().UTC().Add(2 * time.Minute))
	rec := func(link bool, binding string) *requests.Record {
		r := &requests.Record{Snapshot: protocol.Snapshot{
			Schema: protocol.SchemaRequest, RequestID: "11111111-1111-4111-8111-1111111111e1",
			InputDigest: "d1", NotAfter: notAfter,
		}, GrantDigest: "g1", Binding: binding}
		if link {
			r.Origin = map[string]string{"carrier": CarrierLink}
		}
		return r
	}
	cmd := &protocol.Command{NotAfter: notAfter}
	for _, row := range []struct {
		name     string
		link     bool
		binding  string
		srcBind  string
		conflict bool
	}{{name: "same binding", link: true, binding: "sess-4", srcBind: "sess-4"},
		{name: "different binding", link: true, binding: "sess-4", srcBind: "sess-9", conflict: true},
		{name: "legacy record with no binding", link: true, binding: "", srcBind: "sess-9"},
		{name: "non-link record", link: false, binding: "sess-4", srcBind: "sess-9"}} {
		msg, conflict := conflictWith(rec(row.link, row.binding), cmd, "d1", "g1", row.srcBind)
		if conflict != row.conflict {
			t.Fatalf("%s: conflict = %v (%q), want %v", row.name, conflict, msg, row.conflict)
		}
		if row.conflict && msg == "" {
			t.Fatalf("%s: conflict with an empty message", row.name)
		}
	}
}
