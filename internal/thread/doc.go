// Package thread collects and aggregates messages across agent
// mailboxes by thread ID. It scans inbox and outbox directories,
// deduplicates by message ID, and returns entries sorted by timestamp.
package thread
