package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"testing"
)

func sign(secret, body string, b64 bool) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	sum := m.Sum(nil)
	if b64 {
		return base64.StdEncoding.EncodeToString(sum)
	}
	return hex.EncodeToString(sum)
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	secret := "shhh"

	hdr := func(k, v string) http.Header {
		h := http.Header{}
		h.Set(k, v)
		return h
	}

	cases := []struct {
		name   string
		cfg    string
		header http.Header
		want   bool
	}{
		{"unconfigured empty", ``, hdr("X-Sig", "x"), false},
		{"empty json", `{}`, hdr("X-Sig", "x"), false},
		{"valid hex", `{"scheme":"hmac_sha256","secret":"shhh","header":"X-Sig","encoding":"hex"}`,
			hdr("X-Sig", sign(secret, string(body), false)), true},
		{"valid base64", `{"scheme":"hmac_sha256","secret":"shhh","header":"X-Sig","encoding":"base64"}`,
			hdr("X-Sig", sign(secret, string(body), true)), true},
		{"valid with prefix", `{"scheme":"hmac_sha256","secret":"shhh","header":"X-Sig","prefix":"sha256="}`,
			hdr("X-Sig", "sha256="+sign(secret, string(body), false)), true},
		{"wrong secret", `{"scheme":"hmac_sha256","secret":"nope","header":"X-Sig"}`,
			hdr("X-Sig", sign(secret, string(body), false)), false},
		{"missing header", `{"scheme":"hmac_sha256","secret":"shhh","header":"X-Sig"}`,
			http.Header{}, false},
		{"unsupported scheme", `{"scheme":"md5","secret":"shhh","header":"X-Sig"}`,
			hdr("X-Sig", sign(secret, string(body), false)), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verifySignature([]byte(c.cfg), c.header, body); got != c.want {
				t.Errorf("verifySignature = %v, want %v", got, c.want)
			}
		})
	}
}
