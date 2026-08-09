// Package wakeprotocol owns small, stable text contracts shared by the AMQ
// wake producer and companion-process consumers.
package wakeprotocol

// AlreadyRunningPrefix identifies AMQ's ownership refusal. Human-readable
// context may follow the prefix, but consumers can safely classify the line.
const AlreadyRunningPrefix = "wake already running for "
