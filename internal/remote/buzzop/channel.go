package buzzop

// Key names one intention. The same Key always maps to the same signed event
// on a transport that signs events; a transport that cannot choose its event
// ignores it.
type Key string

// ChannelSpec is a private stream channel to create. ID "" lets the
// transport choose the channel id.
type ChannelSpec struct {
	Key             Key
	ID, Name, About string
}

// Created is the outcome of a channel create. ChannelID is set only when the
// outcome is Accepted.
type Created struct {
	Result
	ChannelID string
}

// Message is one channel message. Mention, when set, is the public key the
// message mentions; ReplyTo, when set, is the event it answers.
type Message struct {
	Key                                Key
	Channel, Content, Mention, ReplyTo string
}

// Reaction is one emoji on a message and the public keys that reacted with it.
type Reaction struct {
	Emoji   string
	PubKeys []string
}

// Tri is a fact that is known true, known false, or not known. The zero
// value is TriUnknown.
type Tri int

const (
	TriUnknown Tri = iota
	TriNo
	TriYes
)

// ChannelFacts is what the relay-signed state of one channel says. A fact
// whose event was not read stays TriUnknown, empty or nil.
type ChannelFacts struct {
	Channel  string
	Name     string
	About    string
	Creator  string
	Private  Tri
	Archived Tri
	// Roles maps each pubkey of the 39001 to its role.
	Roles map[string]string
	// Members are the pubkeys of the 39002.
	Members []string
	// MetaEvent, AdminsEvent and MembersEvent are the ids of the 39000,
	// 39001 and 39002 the facts were read from.
	MetaEvent    string
	AdminsEvent  string
	MembersEvent string
}
