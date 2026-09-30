package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/forgelab-me/wharf-agent/internal/identity"
)

func pinnedServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv, identity.Fingerprint(srv.Certificate().Raw)
}

func TestPinnedClientAcceptsTheExpectedCertificate(t *testing.T) {
	srv, fp := pinnedServer(t)
	resp, err := newPinnedClient(tls.Certificate{}, fp).Get(srv.URL)
	if err != nil {
		t.Fatalf("the pinned certificate must be accepted, even though no chain validates: %v", err)
	}
	resp.Body.Close()
}

func TestPinnedClientRefusesAnotherCertificate(t *testing.T) {
	srv, _ := pinnedServer(t)
	_, err := newPinnedClient(tls.Certificate{}, "sha256:"+strings.Repeat("0", 64)).Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("a certificate other than the pinned one must be refused: %v", err)
	}
}
