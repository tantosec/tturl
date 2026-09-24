package tth2_test

import (
	"crypto/tls"
	"testing"

	"github.com/tantosec/tturl/tth2"
)

// newClient returns a Client whose Transport uses tlsCfg, with pooled
// connections released at test end. Dialling is lazy: the first SendBatch or
// RunTrials establishes the connection, so construction cannot fail and needs
// no address.
func newClient(tb testing.TB, tlsCfg *tls.Config) *tth2.Client {
	tb.Helper()
	tr := &tth2.Transport{TLSClientConfig: tlsCfg}
	tb.Cleanup(tr.CloseIdleConnections)
	return &tth2.Client{Transport: tr}
}

// newClientMaxConns is [newClient] with an explicit MaxConnsPerHost, for
// exercising RunTrials' parallel fan-out across pooled connections.
func newClientMaxConns(tb testing.TB, tlsCfg *tls.Config, maxConns int) *tth2.Client {
	tb.Helper()
	tr := &tth2.Transport{TLSClientConfig: tlsCfg, MaxConnsPerHost: maxConns}
	tb.Cleanup(tr.CloseIdleConnections)
	return &tth2.Client{Transport: tr}
}
