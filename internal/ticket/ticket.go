// Package ticket checks the entry ticket tidoly-home signs for one login to one slug (stow issue #3).
// This side holds only home's public key.
package ticket

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Claims struct {
	Issuer    string `json:"iss"`
	Slug      string `json:"aud"`
	Subject   string `json:"sub"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Challenge string `json:"cc"` // the app's PKCE challenge, so a ticket lifted on the way back opens nothing
	Expires   int64  `json:"exp"`
	ID        string `json:"jti"`
}

type Verifier struct {
	Key    ed25519.PublicKey
	Issuer string
	Slug   string
}

var b64 = base64.RawURLEncoding

// Verify checks everything about a ticket except whether it was used before; the caller spends Claims.ID.
func (v Verifier) Verify(ticket, codeVerifier string, now time.Time) (Claims, error) {
	var c Claims
	parts := strings.Split(ticket, ".")
	if len(parts) != 3 {
		return c, errors.New("not a ticket")
	}
	header, err1 := b64.DecodeString(parts[0])
	payload, err2 := b64.DecodeString(parts[1])
	sig, err3 := b64.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return c, errors.New("not a ticket")
	}
	var head struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if json.Unmarshal(header, &head) != nil || head.Alg != "EdDSA" {
		return c, errors.New("wrong algorithm")
	}
	if head.Typ != "stow-ticket" { // home signs other things with other keys; this keeps one from passing for another
		return c, errors.New("not a stow ticket")
	}
	if len(v.Key) != ed25519.PublicKeySize || !ed25519.Verify(v.Key, []byte(parts[0]+"."+parts[1]), sig) {
		return c, errors.New("bad signature")
	}
	if json.Unmarshal(payload, &c) != nil {
		return c, errors.New("not a ticket")
	}
	switch {
	case c.Issuer != v.Issuer:
		return c, errors.New("wrong issuer")
	case c.Slug != v.Slug:
		return c, errors.New("ticket is for another slug")
	case c.Expires < now.Unix():
		return c, errors.New("expired")
	case c.ID == "" || c.Subject == "":
		return c, errors.New("incomplete ticket")
	}
	sum := sha256.Sum256([]byte(codeVerifier))
	if subtle.ConstantTimeCompare([]byte(c.Challenge), []byte(b64.EncodeToString(sum[:]))) != 1 {
		return c, errors.New("PKCE verifier does not match")
	}
	return c, nil
}
