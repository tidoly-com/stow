package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// home plays tidoly-home: it signs what it is asked for with a CA of its own, after one look that finds
// it pending — or fails it, when told to.
type home struct {
	t      *testing.T
	mu     sync.Mutex
	asked  int
	looks  int
	pemOut string
	fail   string
	life   time.Duration
}

func (h *home) call(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if path != "/stow/certificate" {
		return 404, nil, nil
	}
	if method == "POST" {
		var req struct{ CSR string }
		json.Unmarshal(body, &req)
		block, _ := pem.Decode([]byte(req.CSR))
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil || csr.CheckSignature() != nil {
			return 400, []byte("not a CSR"), nil
		}
		if strings.Contains(string(body), "PRIVATE KEY") {
			h.t.Error("the private key was sent")
		}
		h.asked++
		h.looks = 0
		h.pemOut = h.sign(csr)
		return 202, nil, nil
	}
	h.looks++
	switch {
	case h.looks == 1:
		return 200, []byte(`{"status":"PENDING"}`), nil
	case h.fail != "":
		b, _ := json.Marshal(map[string]string{"status": "FAILED", "error": h.fail})
		return 200, b, nil
	}
	b, _ := json.Marshal(map[string]string{"status": "VALID", "certificate": h.pemOut})
	return 200, b, nil
}

func (h *home) sign(csr *x509.CertificateRequest) string {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	ca, _ = x509.ParseCertificate(caDER)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: csr.Subject, DNSNames: csr.DNSNames,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(h.life), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, leaf, ca, csr.PublicKey, caKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

func keep(t *testing.T, k *Keeper, host string) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Keep(ctx, host); close(done) }()
	return func() { cancel(); <-done }
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("waited in vain: %s", what)
}

// A certificate is asked for, waited for while pending, kept with the key made here, and served; after a
// restart the one kept is used without asking again.
func TestACertificateIsAskedForKeptAndServed(t *testing.T) {
	dir := t.TempDir()
	h := &home{t: t, life: 90 * 24 * time.Hour}
	ready := make(chan struct{}, 1)
	k := &Keeper{Dir: dir, Home: h.call, Poll: time.Millisecond, Retry: time.Hour, Ready: func() { ready <- struct{}{} }}
	if k.Have("mom.stow.tidoly.app") {
		t.Fatal("a certificate before asking")
	}
	stop := keep(t, k, "mom.stow.tidoly.app")
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("no certificate came")
	}
	stop()
	if !k.Have("mom.stow.tidoly.app") || k.Have("other.stow.tidoly.app") || k.State() != "" {
		t.Fatalf("have: %v, state %q", k.Have("mom.stow.tidoly.app"), k.State())
	}
	cert, err := k.Certificate(&tls.ClientHelloInfo{ServerName: "mom.stow.tidoly.app"})
	if err != nil || cert.Leaf.DNSNames[0] != "mom.stow.tidoly.app" {
		t.Fatalf("served: %v", err)
	}
	if st, err := os.Stat(k.keyPath()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("the key file: %v %v", st.Mode(), err)
	}
	if _, err := os.Stat(k.pendingPath()); err == nil {
		t.Error("the pending key is still there")
	}

	told := make(chan struct{}, 1)
	again := &Keeper{Dir: dir, Home: h.call, Poll: time.Millisecond, Retry: time.Hour, Ready: func() { told <- struct{}{} }}
	stop = keep(t, again, "mom.stow.tidoly.app")
	select { // the tunnel must hear of it, or https stays shut after a restart
	case <-told:
	case <-time.After(5 * time.Second):
		t.Fatal("the certificate kept was not told")
	}
	stop()
	if !again.Have("mom.stow.tidoly.app") {
		t.Error("the kept certificate is not held")
	}
	if h.asked != 1 {
		t.Errorf("asked %d times, want once", h.asked)
	}
}

// One kept that ends within 30 days is asked for again.
func TestACertificateEndingSoonIsRenewed(t *testing.T) {
	dir := t.TempDir()
	h := &home{t: t, life: 10 * 24 * time.Hour}
	k := &Keeper{Dir: dir, Home: h.call, Poll: time.Millisecond, Retry: time.Hour}
	stop := keep(t, k, "mom.stow.tidoly.app")
	waitFor(t, "the first", func() bool { return k.Have("mom.stow.tidoly.app") })
	stop()
	h.life = 90 * 24 * time.Hour
	again := &Keeper{Dir: dir, Home: h.call, Poll: time.Millisecond, Retry: time.Hour}
	stop = keep(t, again, "mom.stow.tidoly.app")
	waitFor(t, "a second request", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.asked == 2 })
	waitFor(t, "the new one", func() bool { return again.fresh("mom.stow.tidoly.app") })
	stop()
}

// A failure is told to the owner with home's own words, and nothing is kept.
func TestAFailureIsToldWithHomesWords(t *testing.T) {
	h := &home{t: t, life: 90 * 24 * time.Hour, fail: "DNS-01 challenge failed"}
	k := &Keeper{Dir: t.TempDir(), Home: h.call, Poll: time.Millisecond, Retry: time.Hour}
	stop := keep(t, k, "mom.stow.tidoly.app")
	waitFor(t, "the failure", func() bool { return strings.Contains(k.State(), "DNS-01 challenge failed") })
	stop()
	if k.Have("mom.stow.tidoly.app") {
		t.Error("a certificate after a failure")
	}
}
