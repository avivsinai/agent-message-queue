// Package linkcontract pins testdata/link: the golden amq.remote.link/1
// frames, the signed consent document and the test keys behind them. A
// server implementation mirrors those files byte for byte and runs the same
// checks; this package's tests are the Go side of that agreement.
package linkcontract
