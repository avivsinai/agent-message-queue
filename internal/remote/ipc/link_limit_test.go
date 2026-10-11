package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
)

// Every operation other than link.v1 keeps the 1 MiB record limit, although
// the server reads up to the link limit before it knows the operation.
func TestNonLinkRecordOverOneMiBIsRefused(t *testing.T) {
	dir, err := os.MkdirTemp("", "ipcl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	srv, err := Listen(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	conn, err := net.Dial("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	pad := strings.Repeat("x", MaxRecordBytes)
	if _, err := conn.Write([]byte(`{"command":{"schema":"amq.remote.command/1","op":"session.list","pad":"` + pad + "\"}}\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil || resp.Error == nil || resp.Error.Code != "invalid" || !strings.Contains(resp.Error.Message, "exceeds") {
		t.Fatalf("response %s; want invalid: record exceeds", line)
	}
}
