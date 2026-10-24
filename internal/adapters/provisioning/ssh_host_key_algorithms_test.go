package provisioning

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// hkServer offers two host keys (ECDSA and the fixture ED25519 key) and runs
// the authorized-helper emulation, like a real sshd with several host keys.
// It counts connections and authentications.
type hkServer struct {
	address        string
	ecdsa, ed25519 ssh.Signer
	conns, auths   atomic.Int32
	execs          atomic.Int32
	wg             sync.WaitGroup
}

func newHKServer(t *testing.T, clientPub ssh.PublicKey) *hkServer {
	t.Helper()
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecSigner, err := ssh.NewSignerFromKey(ek)
	if err != nil {
		t.Fatal(err)
	}
	_, edSigner := deploymentKey(t)
	s := &hkServer{ecdsa: ecSigner, ed25519: edSigner}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.address = l.Addr().String()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() { _ = conn.Close() }()
				cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
					s.auths.Add(1)
					if !bytes.Equal(key.Marshal(), clientPub.Marshal()) {
						return nil, errors.New("rejected")
					}
					return nil, nil
				}}
				cfg.AddHostKey(s.ecdsa)
				cfg.AddHostKey(s.ed25519)
				sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer func() { _ = sc.Close() }()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, err := nc.Accept()
					if err != nil {
						return
					}
					for req := range creqs {
						if req.Type == "exec" {
							s.execs.Add(1)
							_ = req.Reply(true, nil)
							_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
							_ = ch.Close()
							break
						}
						_ = req.Reply(false, nil)
					}
					return
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = l.Close(); s.wg.Wait() })
	return s
}

// Pinning the ED25519 key of a server that also offers ECDSA (which Go
// negotiates first) must still connect: one rejected handshake, probes, then
// a single authenticated retry restricted to ED25519.
func TestHostKeyPinOfNonPreferredFamilyConnectsAfterLearning(t *testing.T) {
	clientKey, clientSigner := deploymentKey(t)
	srv := newHKServer(t, clientSigner.PublicKey())
	insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(srv.ed25519.PublicKey()))
	if got := insp.hostKeyAlgorithms(srv.address); got != nil {
		t.Fatalf("algorithms before learning: %v", got)
	}
	if !insp.learnHostKeyAlgorithms(t.Context(), srv.address) {
		t.Fatal("ED25519 family not learned")
	}
	if got := insp.hostKeyAlgorithms(srv.address); len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("learned %v", got)
	}
	if srv.auths.Load() != 0 {
		t.Fatalf("probing authenticated %d times", srv.auths.Load())
	}
	// Learning again changes nothing and is not a reason to retry.
	if insp.learnHostKeyAlgorithms(t.Context(), srv.address) {
		t.Fatal("re-learning the same family reported a retry")
	}
}

func TestHostKeyPinOfAbsentKeyNeverAuthenticates(t *testing.T) {
	clientKey, clientSigner := deploymentKey(t)
	srv := newHKServer(t, clientSigner.PublicKey())
	_, other := deploymentKey(t)
	insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(other.PublicKey()))
	if insp.learnHostKeyAlgorithms(t.Context(), srv.address) {
		t.Fatal("learned a family for a key the server does not hold")
	}
	if srv.auths.Load() != 0 || srv.execs.Load() != 0 {
		t.Fatalf("auths=%d execs=%d", srv.auths.Load(), srv.execs.Load())
	}
	if got := int(srv.conns.Load()); got != len(sshHostKeyFamilies) {
		t.Fatalf("probe connections %d, want one per family", got)
	}
}

func TestSenderRetriesOnceAfterLearningTheHostKeyFamily(t *testing.T) {
	clientKey, clientSigner := deploymentKey(t)
	srv := newHKServer(t, clientSigner.PublicKey())
	edPin := ssh.FingerprintSHA256(srv.ed25519.PublicKey())
	sender, err := NewSSHBinaryStageSender(t.Context(), clientKey, edPin, ssh.FingerprintSHA256(clientSigner.PublicKey()), sessionTPolicy())
	if err != nil {
		t.Fatal(err)
	}
	intent := sessionTestHostActionIntent(t, srv.address, edPin, ssh.FingerprintSHA256(clientSigner.PublicKey()))
	_, err = sender.SendHostAction(t.Context(), intent, sessionTActions{})
	// The emulated helper exits 1 with no challenge, so the call ends
	// NotStaged, but only after authenticating on the learned family.
	if !errors.Is(err, ErrSSHBinaryStageNotStaged) {
		t.Fatalf("err %v", err)
	}
	if srv.auths.Load() != 1 || srv.execs.Load() != 1 {
		t.Fatalf("auths=%d execs=%d, want exactly one authenticated retry", srv.auths.Load(), srv.execs.Load())
	}
	if got := sender.insp.host.hostKeyAlgorithms(srv.address); len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("learned %v", got)
	}
}

func sessionTestInspector(t *testing.T, clientKey []byte, hostPin string) *SSHHostInspector {
	t.Helper()
	insp, err := NewSSHHostInspector(t.Context(), clientKey, SSHHostTrust{Fingerprint: hostPin}, sessionTPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return insp
}

func sessionTestHostActionIntent(t *testing.T, address, hostPin, clientPin string) domain.ProbeHostActionIntent {
	t.Helper()
	intent := hostActFrameIntent(domain.ProbeHostActionDataDirectory)
	intent.Identity.Host.Address = address
	intent.Identity.HostKeyFingerprint = hostPin
	intent.Identity.ClientKeyFingerprint = clientPin
	if !intent.Valid() {
		t.Fatal("fixture intent invalid")
	}
	return intent
}
