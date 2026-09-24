//go:build !unix

package requests

func plantNamedPipe(string) error { return nil }
