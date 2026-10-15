//go:build !unix

package main

import "errors"

// startEndpoint is unix-only; elsewhere the owner starts the endpoint.
var startEndpoint = func(root, _ string) error {
	return errors.New("no endpoint is running; start one with `amq-remote up --root " + root + "`")
}
