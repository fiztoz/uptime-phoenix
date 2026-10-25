package domain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"
)

func stageAdmissionDomainRequest() (ProbeBinaryStageAdmissionRequest, ed25519.PrivateKey, [32]byte) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{13}, ed25519.SeedSize))
	var public [32]byte
	copy(public[:], key[32:])
	intent := binaryStageDomainIntent()
	intent.Identity.Host.User = "root"
	request := ProbeBinaryStageAdmissionRequest{Intent: intent, Challenge: ProbeBinaryStageChallenge{
		AuthorityKeyID: ProbeBinaryStageAuthorityKeyID(public), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
	}}
	return request, key, public
}

func TestProbeBinaryStageAdmissionMessage(t *testing.T) {
	r, _, pub := stageAdmissionDomainRequest()
	keyID := sha256.Sum256(pub[:])
	marker := sha256.Sum256([]byte(r.Intent.Marker()))
	want := append([]byte("phoenix-stage-admission-v1\x00"), keyID[:]...)
	want = append(want, r.Challenge.Nonce[:]...)
	want = append(want, marker[:]...)
	if !r.Valid() || !bytes.Equal(r.SigningMessage(), want) || r.Challenge.AuthorityKeyID != hex.EncodeToString(keyID[:]) {
		t.Fatal("canonical signing bytes or key identity changed")
	}
	message := r.SigningMessage()
	message[0] ^= 1
	if !bytes.Equal(r.SigningMessage(), want) {
		t.Fatal("message storage was shared")
	}
	for name, change := range map[string]func(*ProbeBinaryStageAdmissionRequest){
		"nonroot":   func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Host.User = "deploy" },
		"nonce":     func(r *ProbeBinaryStageAdmissionRequest) { r.Challenge.Nonce = [32]byte{} },
		"upper key": func(r *ProbeBinaryStageAdmissionRequest) { r.Challenge.AuthorityKeyID = strings.Repeat("A", 64) },
		"short key": func(r *ProbeBinaryStageAdmissionRequest) { r.Challenge.AuthorityKeyID = "a" },
		"bad hex":   func(r *ProbeBinaryStageAdmissionRequest) { r.Challenge.AuthorityKeyID = strings.Repeat("g", 64) },
		"marker":    func(r *ProbeBinaryStageAdmissionRequest) { r.Challenge.MarkerSHA256 = strings.Repeat("0", 64) },
		"owner":     func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.OwnerID = "invalid" },
		"mode":      func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Mode = ProbeInstallDocker },
	} {
		t.Run(name, func(t *testing.T) {
			bad := r
			change(&bad)
			if bad.Valid() || bad.SigningMessage() != nil {
				t.Fatal("invalid request produced signing bytes")
			}
		})
	}
}

func TestProbeBinaryStageGrantBindsEveryField(t *testing.T) {
	r, key, pub := stageAdmissionDomainRequest()
	grant := ProbeBinaryStageGrant{Request: r}
	copy(grant.Signature[:], ed25519.Sign(key, r.SigningMessage()))
	if !grant.ValidFor(pub) || grant.ValidFor([32]byte{}) {
		t.Fatal("signature/public key verification failed")
	}
	for name, change := range map[string]func(*ProbeBinaryStageAdmissionRequest){
		"operation": func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.OperationID = r.Intent.OwnerID },
		"owner":     func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.OwnerID = r.Intent.AttemptID },
		"attempt":   func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.AttemptID = r.Intent.OwnerID },
		"address":   func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Host.Address = "other.test:22" },
		"user":      func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Host.User = "deploy" },
		"host key": func(r *ProbeBinaryStageAdmissionRequest) {
			r.Intent.Identity.HostKeyFingerprint = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA"
		},
		"client key": func(r *ProbeBinaryStageAdmissionRequest) {
			r.Intent.Identity.ClientKeyFingerprint = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA"
		},
		"actor":     func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.RequestedBy++ },
		"layout":    func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.LayoutVersion++ },
		"mode":      func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Mode = ProbeInstallDocker },
		"version":   func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Binary.Version = "1.2.4" },
		"arch":      func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Binary.Architecture = "arm64" },
		"digest":    func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Binary.SHA256 = strings.Repeat("b", 64) },
		"size":      func(r *ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Binary.Size++ },
		"nonce":     func(r *ProbeBinaryStageAdmissionRequest) { r.Challenge.Nonce[31]++ },
		"authority": func(r *ProbeBinaryStageAdmissionRequest) { r.Challenge.AuthorityKeyID = strings.Repeat("c", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := grant
			change(&bad.Request)
			bad.Request.Challenge.MarkerSHA256 = bad.Request.Intent.MarkerSHA256()
			if bad.ValidFor(pub) {
				t.Fatal("changed tuple retained authorization")
			}
		})
	}
	grant.Signature[0] ^= 1
	if grant.ValidFor(pub) {
		t.Fatal("corrupt signature verified")
	}
}

func TestProbeBinaryStageAdmissionTimeAndNoJSONTags(t *testing.T) {
	r, _, _ := stageAdmissionDomainRequest()
	stamp := time.Date(2026, 10, 23, 1, 2, 3, 123456000, time.UTC)
	if !(ProbeBinaryStageAdmission{Request: r, AdmittedAt: stamp}).Valid() {
		t.Fatal("UTC microsecond stamp refused")
	}
	for _, bad := range []time.Time{{}, stamp.Add(time.Nanosecond), stamp.In(time.FixedZone("offset", 7*3600))} {
		if (ProbeBinaryStageAdmission{Request: r, AdmittedAt: bad}).Valid() {
			t.Fatal("storage timestamp contract not enforced")
		}
	}
	for _, value := range []interface{}{r, r.Challenge, ProbeBinaryStageAdmission{}, ProbeBinaryStageGrant{}} {
		typ := reflect.TypeOf(value)
		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).Tag.Get("json") != "" {
				t.Fatal("domain type gained a wire tag")
			}
		}
	}
}
