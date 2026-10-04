//go:build !unix

package main

import "time"

// psLocation mirrors the unix var; no ps on Windows, so it stays unused.
var psLocation = time.Local

// psListCLI mirrors the unix var; no ps on Windows.
var psListCLI = func() ([]byte, error) { return nil, nil }
