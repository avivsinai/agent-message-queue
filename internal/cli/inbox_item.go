package cli

import "time"

type inboxItem struct {
	ID            string         `json:"id"`
	From          string         `json:"from"`
	To            []string       `json:"to"`
	Thread        string         `json:"thread"`
	Subject       string         `json:"subject"`
	Created       string         `json:"created"`
	Body          string         `json:"body,omitempty"`
	AckRequired   bool           `json:"ack_required"`
	Priority      string         `json:"priority,omitempty"`
	Kind          string         `json:"kind,omitempty"`
	Labels        []string       `json:"labels,omitempty"`
	Context       map[string]any `json:"context,omitempty"`
	MovedToCur    bool           `json:"moved_to_cur"`
	MovedToDLQ    bool           `json:"moved_to_dlq,omitempty"`
	Acked         bool           `json:"acked"`
	ParseError    string         `json:"parse_error,omitempty"`
	FailureReason string         `json:"-"`
	Filename      string         `json:"-"` // actual filename on disk
	SortKey       time.Time      `json:"-"`
}

func (i inboxItem) GetCreated() string {
	return i.Created
}

func (i inboxItem) GetID() string {
	return i.ID
}

func (i inboxItem) GetRawTime() time.Time {
	return i.SortKey
}

type drainItem = inboxItem

type monitorItem = inboxItem
