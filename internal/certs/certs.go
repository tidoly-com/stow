// Package certs keeps this server's own certificate for its address outside, <slug>.stow.tidoly.app, so
// that https ends on this PC: tidoly's tunnel passes it through unopened (stow issue #4). The private key
// is made here and never leaves; tidoly-home gets only a request for the name (a CSR) and has it signed.
//
//	POST /stow/certificate  {"csr": "<PEM>"}  → 202; 409 while one is in progress, 429 past five a day
//	GET  /stow/certificate  → {"status": "PENDING" | "VALID" | "FAILED", "certificate": "<PEM chain>", "error": "..."}
//
// both with the server's key. A certificate is asked again 30 days before it ends.
package certs

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Call is a request to tidoly-home made as this server, with its key: the answer's status and body.
type Call func(ctx context.Context, method, path string, body []byte) (int, []byte, error)

// Keeper holds the certificate and asks for a new one when there is none or it is ending.
type Keeper struct {
	Dir   string // where the key and certificate are kept; the key file only this user reads
	Home  Call
	Ready func() // told when a certificate comes in or changes; may be nil

	// how long to wait between looks while one is pending, and before asking again after a failure; the
	// tests make them short
	Poll, Retry time.Duration

	mu    sync.Mutex
	cert  *tls.Certificate
	host  string
	state string
}

const renewBefore = 30 * 24 * time.Hour

const asking = "인증서를 받는 중입니다"

func (k *Keeper) keyPath() string     { return filepath.Join(k.Dir, "key.pem") }
func (k *Keeper) certPath() string    { return filepath.Join(k.Dir, "cert.pem") }
func (k *Keeper) pendingPath() string { return filepath.Join(k.Dir, "key-pending.pem") }

// Certificate is for a TLS server: the certificate kept, whatever name is asked.
func (k *Keeper) Certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cert == nil {
		return nil, errors.New("no certificate yet")
	}
	return k.cert, nil
}

// Have says whether there is a certificate for the given name that has not ended.
func (k *Keeper) Have(host string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cert != nil && host != "" && k.host == host && time.Now().Before(k.cert.Leaf.NotAfter)
}

// State is how getting the certificate goes, in words for the admin page: "" when there is one.
func (k *Keeper) State() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.state
}

// Asking says whether a certificate is being asked for right now: it comes within a minute or so.
func (k *Keeper) Asking() bool { return k.State() == asking }

func (k *Keeper) setState(s string) {
	k.mu.Lock()
	k.state = s
	k.mu.Unlock()
}

// Keep runs until ctx ends: it loads the certificate kept for host, and asks for one whenever there is none
// or it ends within 30 days. An empty host — not connected — keeps nothing.
func (k *Keeper) Keep(ctx context.Context, host string) {
	if host == "" {
		k.mu.Lock()
		k.cert, k.host, k.state = nil, "", ""
		k.mu.Unlock()
		return
	}
	os.MkdirAll(k.Dir, 0o700)
	if err := k.load(host); err == nil {
		if k.Ready != nil {
			k.Ready() // the one kept: the tunnel takes https again after a restart
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("certificate: the one kept cannot be used: %v", err)
	}
	for {
		wait := 12 * time.Hour // look again twice a day
		if !k.fresh(host) {
			if err := k.obtain(ctx, host); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("certificate: %v", err)
				k.setState("인증서를 받지 못했습니다 — " + err.Error())
				wait = k.Retry
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// fresh says whether the certificate held is for host and good for more than 30 days.
func (k *Keeper) fresh(host string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cert != nil && k.host == host && time.Until(k.cert.Leaf.NotAfter) > renewBefore
}

// load reads the key and certificate kept, if they are for host.
func (k *Keeper) load(host string) error {
	cert, err := tls.LoadX509KeyPair(k.certPath(), k.keyPath())
	if err != nil {
		return err
	}
	if err := cert.Leaf.VerifyHostname(host); err != nil {
		return err
	}
	k.mu.Lock()
	k.cert, k.host, k.state = &cert, host, ""
	k.mu.Unlock()
	return nil
}

// obtain asks home for a certificate for host and waits for it.
func (k *Keeper) obtain(ctx context.Context, host string) error {
	key, err := k.pendingKey()
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}}, key)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))})
	k.setState(asking)
	status, answer, err := k.Home(ctx, "POST", "/stow/certificate", body)
	switch {
	case err != nil:
		return err
	case status == http.StatusAccepted, status == http.StatusConflict: // asked now, or one asked earlier is in progress
	case status == http.StatusTooManyRequests:
		return errors.New("오늘 신청이 너무 많았습니다. 내일 다시 신청합니다")
	case status == http.StatusUnauthorized:
		return errors.New("tidoly 가 이 서버의 열쇠를 받지 않습니다")
	case status == http.StatusServiceUnavailable:
		return errors.New("tidoly 의 인증서 발급이 닫혀 있습니다")
	default:
		return fmt.Errorf("tidoly answered %d: %s", status, short(answer))
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(k.Poll):
		}
		status, answer, err := k.Home(ctx, "GET", "/stow/certificate", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("tidoly answered %d: %s", status, short(answer))
		}
		var got struct {
			Status      string `json:"status"`
			Certificate string `json:"certificate"`
			Error       string `json:"error"`
		}
		if err := json.Unmarshal(answer, &got); err != nil {
			return fmt.Errorf("tidoly's answer: %v", err)
		}
		switch got.Status {
		case "PENDING":
			continue
		case "FAILED":
			return fmt.Errorf("tidoly 가 발급에 실패했습니다: %s", got.Error)
		case "VALID":
			if err := k.take(host, key, []byte(got.Certificate)); err != nil {
				return err
			}
			log.Printf("certificate: for %s until %s", host, k.until())
			if k.Ready != nil {
				k.Ready()
			}
			return nil
		default:
			return fmt.Errorf("tidoly's answer has the status %q", got.Status)
		}
	}
}

// take keeps a certificate that answers the pending key and the name, and makes the pending key the key.
func (k *Keeper) take(host string, key *ecdsa.PrivateKey, chain []byte) error {
	keyPEM, err := os.ReadFile(k.pendingPath())
	if err != nil {
		return err
	}
	cert, err := tls.X509KeyPair(chain, keyPEM)
	if err != nil {
		return fmt.Errorf("the certificate from tidoly does not go with this PC's key: %v", err)
	}
	if err := cert.Leaf.VerifyHostname(host); err != nil {
		return err
	}
	if !cert.Leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool }).Equal(key.Public()) {
		return errors.New("the certificate from tidoly is for another key")
	}
	if err := os.WriteFile(k.certPath(), chain, 0o644); err != nil {
		return err
	}
	if err := os.Rename(k.pendingPath(), k.keyPath()); err != nil {
		return err
	}
	k.mu.Lock()
	k.cert, k.host, k.state = &cert, host, ""
	k.mu.Unlock()
	return nil
}

// pendingKey is the key a certificate is being asked for: the one made for an earlier request still
// unanswered, or a new one. It is written before it is used, so a certificate that comes after a restart
// finds its key.
func (k *Keeper) pendingKey() (*ecdsa.PrivateKey, error) {
	if raw, err := os.ReadFile(k.pendingPath()); err == nil {
		if block, _ := pem.Decode(raw); block != nil {
			if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return key, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(k.pendingPath(), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (k *Keeper) until() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cert == nil {
		return ""
	}
	return k.cert.Leaf.NotAfter.Format(time.DateOnly)
}

func short(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
