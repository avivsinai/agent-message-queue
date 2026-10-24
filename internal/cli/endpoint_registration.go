package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/agentidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type endpointRole string

const (
	endpointSender    endpointRole = "sender"
	endpointRecipient endpointRole = "recipient"
)

// endpointQueue is the queue one send or reply endpoint lives in: the sender
// in its source queue, recipients in their destination queue. root is the
// delivery root whose mailboxes hold identity records.
type endpointQueue struct {
	decls agentDeclarations
	root  *fsq.DeliveryRoot
}

// endpointQueueFor reads the declarations that apply to root when configFS is
// its selected config authority: that roster, plus root's own roster when the
// selection is a shared base config.
func endpointQueueFor(root, configFS *fsq.DeliveryRoot) endpointQueue {
	q := endpointQueue{root: root, decls: agentDeclarations{readAgentRosterDeliveryRoot(configFS)}}
	if configFS != root {
		q.decls = append(q.decls, readAgentRosterDeliveryRoot(root))
	}
	return q
}

// openLocalEndpointQueue selects the config authority send uses for a local
// root and returns that root's endpoint queue.
func openLocalEndpointQueue(rootFS *fsq.DeliveryRoot, rootPath string, pin sessionPin, ignoreSessionPin bool) (endpointQueue, func(), error) {
	selection, err := openSourceConfigSelection(rootFS, rootPath, pin, ignoreSessionPin)
	if err != nil {
		return endpointQueue{}, func() {}, err
	}
	return endpointQueueFor(rootFS, selection.ConfigFS), selection.Close, nil
}

func (q endpointQueue) classify(handle string) agentRegistration {
	data, err := q.root.ReadRegularNoFollow(filepath.Join("agents", handle, agentidentity.FileName))
	var rec agentidentity.Record
	if err == nil {
		rec, err = agentidentity.Parse(data, handle)
	}
	return classifyAgentRegistration(q.decls, handle, rec, err)
}

// checkEndpointRegistration is the one endpoint policy send and reply share.
// A declared or observed handle passes. An unregistered one warns, or with
// strict refuses. A roster that exists but cannot be read leaves an
// undeclared handle unclassified: strict refuses, as it always has for an
// unreadable config. warn=false re-checks only for strict refusal, for the
// revalidation just before delivery. A queue with no roster at all declares
// nothing and is not judged.
func checkEndpointRegistration(q endpointQueue, role endpointRole, strict, warn bool, handles ...string) error {
	if !q.decls.anyPresent() {
		return nil
	}
	rosterErr := q.decls.err()
	for _, handle := range handles {
		if q.decls.declares(handle) {
			continue
		}
		if rosterErr != nil && strict {
			return rosterErr
		}
		reg := q.classify(handle)
		if reg.Status != registrationUnregistered {
			continue
		}
		if rosterErr != nil {
			if warn {
				_ = writeStderr("warning: cannot classify %s %q: %v\n", role, handle, rosterErr)
			}
			continue
		}
		msg := fmt.Sprintf("unregistered %s %q: it is in no roster and no agent has run under it", role, handle)
		if len(reg.Diagnostics) > 0 {
			msg = fmt.Sprintf("unregistered %s %q: it is in no roster and its identity record cannot be trusted: %v", role, handle, errors.Join(reg.Diagnostics...))
		}
		if strict {
			return errors.New(msg)
		}
		if warn {
			_ = writeStderr("warning: %s; check the handle with 'amq who'\n", msg)
		}
	}
	return nil
}
