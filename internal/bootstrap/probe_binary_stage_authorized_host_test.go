package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func authzHostPublicLine(t *testing.T) ([]byte, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := ssh.MarshalAuthorizedKey(key)
	return append(line[:len(line)-1], []byte(" root@host\n")...), ssh.FingerprintSHA256(key)
}

func TestProbeBinaryStageHostKeyFingerprintsParsesOnlyCleanPublicKeys(t *testing.T) {
	a, fpA := authzHostPublicLine(t)
	b, fpB := authzHostPublicLine(t)
	got, err := probeBinaryStageHostKeyFingerprints([][]byte{a, b})
	if err != nil || len(got) != 2 || got[0] != fpA || got[1] != fpB {
		t.Fatalf("fingerprints %v err=%v", got, err)
	}
	for name, bodies := range map[string][][]byte{
		"none":           nil,
		"empty body":     {{}},
		"garbage":        {[]byte("not a key\n")},
		"second key":     {append(append([]byte(nil), a...), b...)},
		"one bad of two": {a, []byte("ssh-ed25519 AAAA\n")},
		"private key":    {[]byte("-----BEGIN OPENSSH PRIVATE KEY-----\n")},
	} {
		if got, err := probeBinaryStageHostKeyFingerprints(bodies); err != domain.ErrProbeBinaryStageWriteUnavailable || got != nil {
			t.Fatalf("%s: fingerprints %v err=%v", name, got, err)
		}
	}
}

func TestProbeBinaryStageHostOwnsIntentMatchesPinnedHostKeyOnly(t *testing.T) {
	intent := helperIntent()
	fp := intent.Identity.HostKeyFingerprint
	if !probeBinaryStageHostOwnsIntent([]string{"SHA256:other", fp}, intent) {
		t.Fatal("own host key refused")
	}
	if probeBinaryStageHostOwnsIntent([]string{"SHA256:other"}, intent) || probeBinaryStageHostOwnsIntent(nil, intent) {
		t.Fatal("foreign host key accepted")
	}
	// The client key pin is not a host identity.
	intent.Identity.HostKeyFingerprint = "SHA256:other-host"
	if probeBinaryStageHostOwnsIntent([]string{intent.Identity.ClientKeyFingerprint}, intent) {
		t.Fatal("client fingerprint accepted as host identity")
	}
	intent.Identity.HostKeyFingerprint = ""
	if probeBinaryStageHostOwnsIntent([]string{""}, intent) {
		t.Fatal("empty fingerprint matched")
	}
}

// Host-key loading is a local prerequisite: every refusal precedes the
// challenge, so no sender ever consumes an admission for an unbindable host.
func TestRunAuthorizedProbeBinaryStageHelperHostKeyRefusalEmitsNoChallenge(t *testing.T) {
	_, pub, _ := ed25519.GenerateKey(rand.Reader)
	var key [32]byte
	copy(key[:], pub[32:])
	cases := map[string]func(context.Context) ([]string, error){
		"nil":        nil,
		"error":      func(context.Context) ([]string, error) { return nil, errors.New("sensitive-hostkey-canary") },
		"empty":      func(context.Context) ([]string, error) { return nil, nil },
		"storage":    func(context.Context) ([]string, error) { return nil, domain.ErrProbeArtifactStorage },
		"canceled":   func(context.Context) ([]string, error) { return nil, context.Canceled },
		"zero slice": func(context.Context) ([]string, error) { return []string{}, nil },
	}
	for name, hostKeys := range cases {
		t.Run(name, func(t *testing.T) {
			input := &hostMockInput{}
			var out, stderr bytes.Buffer
			acquired := 0
			acquire := func(context.Context) (*os.File, error) {
				acquired++
				return nil, domain.ErrProbeBinaryStageWriteUnavailable
			}
			entropy := bytes.NewReader(bytes.Repeat([]byte{7}, 32))
			code := runAuthorizedProbeBinaryStageHelper(context.Background(), nil, input, &out, &stderr,
				func(context.Context) ([32]byte, error) { return key, nil }, acquire, entropy, hostKeys)
			if code != 1 || out.Len() != 0 || acquired != 0 || input.reads.Load() != 0 || input.closes.Load() != 1 {
				t.Fatalf("code=%d stdout=%q acquired=%d reads=%d closes=%d", code, out.String(), acquired, input.reads.Load(), input.closes.Load())
			}
			if bytes.Contains(stderr.Bytes(), []byte("canary")) {
				t.Fatalf("leaked host-key error %q", stderr.String())
			}
			if entropy.Len() != 32 {
				t.Fatalf("entropy consumed before host binding: %d left", entropy.Len())
			}
			if _, err := io.ReadAll(entropy); err != nil {
				t.Fatal(err)
			}
		})
	}
}
