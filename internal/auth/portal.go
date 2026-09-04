package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidPortalToken = errors.New("auth: invalid portal token")
	ErrExpiredPortalToken = errors.New("auth: expired portal token")
)

// portalTypeTag domain-separates portal tokens from any other HMAC token
// signed with the same secret (notably session cookies). It must never
// collide with another payload's leading bytes.
const portalTypeTag byte = 0x02

// PortalSigner mints and verifies app-scoped, stateless portal tokens. Same
// HMAC-SHA256 construction as SessionSigner, reusing the same secret; the tag
// byte keeps the two token spaces disjoint.
type PortalSigner struct {
	Secret []byte
	TTL    time.Duration
}

// payload layout: tag[1] | app_id[16] | org_id[16] | epoch[8] | exp[8]
const portalPayloadLen = 1 + 16 + 16 + 8 + 8

// Mint returns a token scoped to appID within orgID at the given portal epoch,
// plus its absolute expiry.
func (s *PortalSigner) Mint(appID, orgID uuid.UUID, epoch int64) (string, time.Time) {
	exp := time.Now().Add(s.TTL)
	var payload [portalPayloadLen]byte
	payload[0] = portalTypeTag
	copy(payload[1:17], appID[:])
	copy(payload[17:33], orgID[:])
	binary.BigEndian.PutUint64(payload[33:41], uint64(epoch))
	binary.BigEndian.PutUint64(payload[41:49], uint64(exp.Unix()))

	mac := hmac.New(sha256.New, s.Secret)
	mac.Write(payload[:])
	sig := mac.Sum(nil)

	out := make([]byte, len(payload)+len(sig))
	copy(out, payload[:])
	copy(out[len(payload):], sig)
	return base64.RawURLEncoding.EncodeToString(out), exp
}

// Parse verifies a token and returns its app id, org id, and epoch. It fails
// closed on any decode/tag/signature error and on expiry. The epoch is
// compared against the DB by the caller (middleware), not here.
func (s *PortalSigner) Parse(token string) (uuid.UUID, uuid.UUID, int64, error) {
	buf, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return uuid.Nil, uuid.Nil, 0, ErrInvalidPortalToken
	}
	if len(buf) != portalPayloadLen+sha256.Size {
		return uuid.Nil, uuid.Nil, 0, ErrInvalidPortalToken
	}
	payload := buf[:portalPayloadLen]
	sig := buf[portalPayloadLen:]
	if payload[0] != portalTypeTag {
		return uuid.Nil, uuid.Nil, 0, ErrInvalidPortalToken
	}
	mac := hmac.New(sha256.New, s.Secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return uuid.Nil, uuid.Nil, 0, ErrInvalidPortalToken
	}
	exp := int64(binary.BigEndian.Uint64(payload[41:49]))
	if time.Now().Unix() > exp {
		return uuid.Nil, uuid.Nil, 0, ErrExpiredPortalToken
	}
	epoch := int64(binary.BigEndian.Uint64(payload[33:41]))
	var app, org uuid.UUID
	copy(app[:], payload[1:17])
	copy(org[:], payload[17:33])
	return app, org, epoch, nil
}
