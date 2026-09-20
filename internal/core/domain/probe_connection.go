package domain

import (
	"net/url"
	"strconv"
	"time"
)

// ProbeCredentialMetadata binds encrypted runtime credentials to a registered
// endpoint, pinned certificate, installation and stream. It is never a wire DTO.
type ProbeCredentialMetadata struct {
	HubID             string
	ProbeID           string
	StreamID          string
	EnrollmentID      string
	CredentialVersion int64
	Endpoint          string
	Fingerprint       string
}

// ValidProbeCredentialMetadata validates structural identity and transport scope;
// the management dialer separately enforces resolved destination policy.
func ValidProbeCredentialMetadata(m ProbeCredentialMetadata) bool {
	if !configUUID(m.HubID) || !configUUID(m.ProbeID) || !configUUID(m.StreamID) || !configUUID(m.EnrollmentID) || m.CredentialVersion <= 0 || len(m.Fingerprint) != 64 || !configHex(m.Fingerprint) || len(m.Endpoint) > 2048 {
		return false
	}
	u, err := url.Parse(m.Endpoint)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.Path != "/ws/probe/v1" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}

// ProbeConnection retains preparation before the first credential-bearing dial.
// Only protected credential bytes are recoverable in the hub database.
type ProbeConnection struct {
	ProbeCredentialMetadata
	ProtectedCredential []byte
	State               string // prepared or active
	PreparedAt          time.Time
	ActivatedAt         *time.Time
}

// ProbeSessionInput is confidential runtime input for one fenced hub connection.
// The token and decrypted document must never be logged or returned over HTTP.
type ProbeSessionInput struct {
	Connection     ProbeCredentialMetadata
	Token          string
	Generation     int64
	CommittedSeq   int64
	ConfigDocument []byte
}
