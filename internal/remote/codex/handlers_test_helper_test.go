package codex

import "sync"

// swappableHandlers lets a test install or replace a callback after the client
// is already running. Production handlers are constructor arguments and never
// change (that is the point of agent-message-queue-611.22.25), but tests
// legitimately need to arm a handler mid-test — so they install ONE forwarding
// handler up front and swap the target behind a mutex instead of writing the
// client's fields while the read pump reads them.
type swappableHandlers struct {
	mu   sync.Mutex
	note func(Notification)
	req  func(ServerRequest)
}

func (h *swappableHandlers) handlers() Handlers {
	return Handlers{OnNotification: h.onNote, OnServerRequest: h.onReq}
}

func (h *swappableHandlers) onNote(n Notification) {
	h.mu.Lock()
	f := h.note
	h.mu.Unlock()
	if f != nil {
		f(n)
	}
}

func (h *swappableHandlers) onReq(r ServerRequest) {
	h.mu.Lock()
	f := h.req
	h.mu.Unlock()
	if f != nil {
		f(r)
	}
}

func (h *swappableHandlers) setNote(f func(Notification)) {
	h.mu.Lock()
	h.note = f
	h.mu.Unlock()
}

func (h *swappableHandlers) setReq(f func(ServerRequest)) {
	h.mu.Lock()
	h.req = f
	h.mu.Unlock()
}

// clientHandlers maps a test client to the holder installed at construction,
// so a test can swap behaviour without the production Client carrying a
// test-only field.
var (
	clientHandlersMu sync.Mutex
	clientHandlers   = map[*Client]*swappableHandlers{}
)

func attachTestHandlers(c *Client, h *swappableHandlers) {
	clientHandlersMu.Lock()
	clientHandlers[c] = h
	clientHandlersMu.Unlock()
}

func handlersOf(c *Client) *swappableHandlers {
	clientHandlersMu.Lock()
	defer clientHandlersMu.Unlock()
	return clientHandlers[c]
}
