package tlsutil

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/chxragsaxena/pullwise-knowledge/internal/devcert"
)

func certs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := devcert.Generate(dir, []string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mtlsServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	cfg, err := ServerConfig(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"), filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello " + r.TLS.PeerCertificates[0].Subject.CommonName))
	}))
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string, cfg *tls.Config) (string, error) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	buf := make([]byte, 100)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n]), nil
}

func TestAClientWithTheRightCertificateGetsThrough(t *testing.T) {
	dir := certs(t)
	srv := mtlsServer(t, dir)
	cfg, err := ClientConfig(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client-key.pem"), filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := get(t, srv.URL, cfg)
	if err != nil || got != "hello pullwise-worker" {
		t.Fatalf("got %q, %v; want the server to see the client's certificate identity", got, err)
	}
}

func TestAClientWithoutACertificateIsRefused(t *testing.T) {
	dir := certs(t)
	srv := mtlsServer(t, dir)
	cfg, err := ClientConfig("", "", filepath.Join(dir, "ca.pem")) // no client certificate
	if err != nil {
		t.Fatal(err)
	}
	if got, err := get(t, srv.URL, cfg); err == nil {
		t.Fatalf("a client with no certificate got %q", got)
	}
}

func TestAClientCertificateFromAnotherCAIsRefused(t *testing.T) {
	dir, stranger := certs(t), certs(t) // a second, unrelated CA
	srv := mtlsServer(t, dir)
	cfg, err := ClientConfig(filepath.Join(stranger, "client.pem"), filepath.Join(stranger, "client-key.pem"), filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := get(t, srv.URL, cfg); err == nil {
		t.Fatalf("a certificate from an untrusted CA got %q", got)
	}
}

func TestAClientRefusesAServerFromAnotherCA(t *testing.T) {
	dir, stranger := certs(t), certs(t)
	srv := mtlsServer(t, dir)
	// Trusts the wrong CA, so it must not talk to this server.
	cfg, err := ClientConfig(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client-key.pem"), filepath.Join(stranger, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := get(t, srv.URL, cfg); err == nil {
		t.Fatalf("the client trusted a server it had no reason to trust: %q", got)
	}
}

func TestServerWithoutAClientCAStillEncryptsButDoesNotDemandACertificate(t *testing.T) {
	dir := certs(t)
	cfg, err := ServerConfig(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Fatal("plain TLS must not require client certificates")
	}
}

func TestBadFilesAreReported(t *testing.T) {
	if _, err := ServerConfig("/nope.pem", "/nope-key.pem", ""); err == nil {
		t.Error("a missing server certificate must be an error")
	}
	if _, err := ClientConfig("", "", "/nope.pem"); err == nil {
		t.Error("a missing CA must be an error")
	}
}
