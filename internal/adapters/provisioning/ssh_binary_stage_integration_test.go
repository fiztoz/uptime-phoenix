package provisioning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// This server independently encodes the small v3 wire subset. It does not use
// production packet/parser helpers, a system SSH daemon, files or credentials.
type binaryStageServer struct {
	t              *testing.T
	mode           string
	key            []byte
	hostSigner     ssh.Signer
	intent         domain.ProbeBinaryStageIntent
	payload        []byte
	listener       net.Listener
	mu             sync.Mutex
	connections    map[net.Conn]bool
	kinds          []byte
	paths          []string
	sshRequests    []string
	closedHandles  int
	auth, accepted atomic.Int32
	wg             sync.WaitGroup
	entered        chan struct{}
	enterOnce      sync.Once
}

func stageTestU32(v uint32) []byte    { return binary.BigEndian.AppendUint32(nil, v) }
func stageTestString(b []byte) []byte { return append(stageTestU32(uint32(len(b))), b...) }
func stageTestFrame(typ byte, b []byte) []byte {
	return append(append(stageTestU32(uint32(len(b)+1)), typ), b...)
}
func stageTestTakeString(b []byte) (string, []byte, bool) {
	if len(b) < 4 {
		return "", nil, false
	}
	n := uint64(binary.BigEndian.Uint32(b))
	if n > uint64(len(b)-4) {
		return "", nil, false
	}
	return string(b[4 : 4+int(n)]), b[4+int(n):], true
}
func stageTestStatus(id, code uint32) []byte {
	b := append(stageTestU32(id), stageTestU32(code)...)
	b = append(b, stageTestString([]byte("private-stage-server-diagnostic"))...)
	return append(b, stageTestString(nil)...)
}
func stageTestAttrs(id uint32, size int64, mode uint32, file bool) []byte {
	b := stageTestU32(id)
	flags := uint32(4)
	if file {
		flags |= 1
	}
	b = append(b, stageTestU32(flags)...)
	if file {
		b = binary.BigEndian.AppendUint64(b, uint64(size))
	}
	return append(b, stageTestU32(mode)...)
}

func serveBinaryStage(t *testing.T, mode string) *binaryStageServer {
	t.Helper()
	key, client := deploymentKey(t)
	_, host := deploymentKey(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("bounded verified stage bytes\n"), 2500)
	sum := sha256.Sum256(payload)
	f := &binaryStageServer{t: t, mode: mode, key: key, hostSigner: host, payload: payload, listener: ln, connections: map[net.Conn]bool{}, entered: make(chan struct{})}
	f.intent = domain.ProbeBinaryStageIntent{Identity: domain.ProbeInstallIdentity{OperationID: "11111111-1111-4111-8111-111111111111", Host: domain.ProbeSSHHost{Address: ln.Addr().String(), User: "deploy"}, HostKeyFingerprint: ssh.FingerprintSHA256(host.PublicKey()), ClientKeyFingerprint: ssh.FingerprintSHA256(client.PublicKey()), RequestedBy: 1, LayoutVersion: 1, Mode: domain.ProbeInstallSystemd, Binary: domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload))}}, OwnerID: "22222222-2222-4222-8222-222222222222", AttemptID: "33333333-3333-4333-8333-333333333333"}
	if mode == "dns_numeric" {
		_, port, _ := net.SplitHostPort(f.intent.Identity.Host.Address)
		f.intent.Identity.Host.Address = "stage.example:" + port
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			f.accepted.Add(1)
			f.mu.Lock()
			f.connections[c] = true
			f.mu.Unlock()
			f.wg.Add(1)
			go f.serve(c, client.PublicKey())
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		f.mu.Lock()
		for c := range f.connections {
			_ = c.Close()
		}
		f.mu.Unlock()
		done := make(chan struct{})
		go func() { f.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("mock SSH work did not join")
		}
	})
	return f
}

func (f *binaryStageServer) serve(raw net.Conn, clientKey ssh.PublicKey) {
	defer f.wg.Done()
	defer func() { _ = raw.Close(); f.mu.Lock(); delete(f.connections, raw); f.mu.Unlock() }()
	if f.mode == "stall_handshake" {
		f.enterOnce.Do(func() { close(f.entered) })
		_, _ = io.Copy(io.Discard, raw)
		return
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		f.auth.Add(1)
		if !bytes.Equal(key.Marshal(), clientKey.Marshal()) {
			return nil, errors.New("rejected")
		}
		return nil, nil
	}}
	cfg.AddHostKey(f.hostSigner)
	conn, channels, requests, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	f.wg.Add(1)
	go func() { defer f.wg.Done(); ssh.DiscardRequests(requests) }()
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			f.t.Error("unexpected forwarding channel")
			_ = incoming.Reject(ssh.Prohibited, "rejected")
			continue
		}
		if f.mode == "stall_channel" {
			f.enterOnce.Do(func() { close(f.entered) })
			_ = conn.Wait()
			return
		}
		ch, reqs, e := incoming.Accept()
		if e != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() { _ = ch.Close() }()
			for req := range reqs {
				f.mu.Lock()
				f.sshRequests = append(f.sshRequests, req.Type)
				f.mu.Unlock()
				var sub struct{ Name string }
				if req.Type != "subsystem" || ssh.Unmarshal(req.Payload, &sub) != nil || sub.Name != "sftp" {
					f.t.Error("SSH exec/env/PTY or wrong subsystem")
					_ = req.Reply(false, nil)
					continue
				}
				if f.mode == "stall_subsystem" {
					f.enterOnce.Do(func() { close(f.entered) })
					_, _ = io.Copy(io.Discard, ch)
					return
				}
				if f.mode == "deny_subsystem" {
					_ = req.Reply(false, nil)
					return
				}
				_ = req.Reply(true, nil)
				if f.mode == "stderr_overflow" {
					_, _ = ch.Stderr().Write(bytes.Repeat([]byte("x"), 4097))
				}
				f.protocol(ch)
				return
			}
		}()
	}
}

func (f *binaryStageServer) protocol(ch ssh.Channel) {
	files := map[string][]byte{"m": []byte(f.intent.Marker()), "b": append([]byte(nil), f.payload...)}
	if f.mode == "marker_digest" {
		files["m"][0] ^= 1
	}
	if f.mode == "foreign_attempt" {
		files["m"] = []byte(strings.Replace(f.intent.Marker(), f.intent.AttemptID, "44444444-4444-4444-8444-444444444444", 1))
	}
	if f.mode == "binary_digest" {
		files["b"][0] ^= 1
	}
	if f.mode == "truncated" {
		files["b"] = files["b"][:len(files["b"])-1]
	}
	if f.mode == "grown" {
		files["b"] = append(files["b"], 1)
	}
	handles := map[string]string{}
	marker := f.intent.Directory() + "/" + domain.ProbeBinaryStageMarkerName
	bin := f.intent.Directory() + "/" + f.intent.Identity.Binary.ReleaseName()
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(ch, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n < 1 || n > 65536 {
			f.t.Error("unbounded client packet")
			return
		}
		packet := make([]byte, n)
		if _, err := io.ReadFull(ch, packet); err != nil {
			return
		}
		typ := packet[0]
		body := packet[1:]
		f.mu.Lock()
		f.kinds = append(f.kinds, typ)
		f.mu.Unlock()
		if typ == 1 {
			if !bytes.Equal(body, stageTestU32(3)) {
				f.t.Error("invalid SFTP init")
				return
			}
			version := uint32(3)
			if f.mode == "version" {
				version = 4
			}
			reply := stageTestU32(version)
			if f.mode == "extensions" {
				for range 32 {
					reply = append(reply, stageTestString([]byte("ignored"))...)
					reply = append(reply, stageTestString([]byte("not-invoked"))...)
				}
			}
			if f.mode == "extensions_over" {
				for range 33 {
					reply = append(reply, stageTestString(nil)...)
					reply = append(reply, stageTestString(nil)...)
				}
			}
			_, _ = ch.Write(stageTestFrame(2, reply))
			continue
		}
		if len(body) < 4 {
			f.t.Error("missing request id")
			return
		}
		id := binary.BigEndian.Uint32(body)
		body = body[4:]
		replyID := id
		if f.mode == "wrong_id" {
			replyID++
		}
		var outType byte
		var out []byte
		switch typ {
		case 7:
			path, rest, ok := stageTestTakeString(body)
			if !ok || len(rest) != 0 {
				f.t.Error("malformed LSTAT")
				return
			}
			f.mu.Lock()
			f.paths = append(f.paths, path)
			f.mu.Unlock()
			if path != "/opt" && path != f.intent.Directory() && path != marker && path != bin {
				f.t.Error("unexpected caller path")
				return
			}
			if (f.mode == "absent" && path == f.intent.Directory()) || (f.mode == "marker_missing" && path == marker) {
				outType = 101
				out = stageTestStatus(replyID, 2)
			} else if f.mode == "permission" && path == f.intent.Directory() {
				outType = 101
				out = stageTestStatus(replyID, 3)
			} else if path == "/opt" || path == f.intent.Directory() {
				mode := uint32(0040000 | 0700)
				if f.mode == "directory_symlink" && path == f.intent.Directory() {
					mode = 0120000 | 0700
				}
				outType = 105
				out = stageTestAttrs(replyID, 0, mode, false)
			} else {
				h := "m"
				if path == bin {
					h = "b"
				}
				size := int64(len(files[h]))
				if h == "b" && (f.mode == "truncated" || f.mode == "grown") {
					size = f.intent.Identity.Binary.Size
				}
				if f.mode == "size" {
					size++
				}
				mode := uint32(0100000 | 0400)
				if f.mode == "symlink" {
					mode = 0120000 | 0400
				}
				if f.mode == "special" {
					mode = 0020000 | 0400
				}
				outType = 105
				out = stageTestAttrs(replyID, size, mode, true)
			}
		case 3:
			path, rest, ok := stageTestTakeString(body)
			if !ok || len(rest) != 8 || binary.BigEndian.Uint32(rest) != 1 || binary.BigEndian.Uint32(rest[4:]) != 0 {
				f.t.Error("OPEN was not exact read-only")
				return
			}
			h := "m"
			if path == bin {
				h = "b"
			} else if path != marker {
				f.t.Error("OPEN outside canonical paths")
				return
			}
			handles[h] = h
			outType = 102
			hb := []byte(h)
			if f.mode == "handle" {
				hb = bytes.Repeat([]byte("x"), 257)
			}
			out = append(stageTestU32(replyID), stageTestString(hb)...)
		case 8, 5, 4:
			h, rest, ok := stageTestTakeString(body)
			if !ok || handles[h] == "" {
				f.t.Error("unknown client handle")
				return
			}
			if typ == 8 {
				if len(rest) != 0 {
					f.t.Error("FSTAT trailing bytes")
					return
				}
				size := int64(len(files[h]))
				if h == "b" && (f.mode == "truncated" || f.mode == "grown") {
					size = f.intent.Identity.Binary.Size
				}
				if f.mode == "fstat_size" {
					size++
				}
				outType = 105
				out = stageTestAttrs(replyID, size, 0100000|0400, true)
				if f.mode == "attrs" {
					out = append(stageTestU32(replyID), stageTestU32(0x80000000)...)
				}
			}
			if typ == 4 {
				if len(rest) != 0 {
					f.t.Error("CLOSE trailing bytes")
					return
				}
				f.mu.Lock()
				f.closedHandles++
				f.mu.Unlock()
				delete(handles, h)
				code := uint32(0)
				if f.mode == "close_failure" {
					code = 4
				}
				outType = 101
				out = stageTestStatus(replyID, code)
			}
			if typ == 5 {
				if len(rest) != 12 {
					f.t.Error("READ framing")
					return
				}
				offset := binary.BigEndian.Uint64(rest)
				length := binary.BigEndian.Uint32(rest[8:])
				if length == 0 || length > 32768 {
					f.t.Error("unbounded READ")
					return
				}
				if f.mode == "stall_read" {
					f.enterOnce.Do(func() { close(f.entered) })
					_, _ = io.Copy(io.Discard, ch)
					return
				}
				outType = 101
				out = stageTestStatus(replyID, 1)
				if offset < uint64(len(files[h])) {
					end := offset + uint64(length)
					if end > uint64(len(files[h])) {
						end = uint64(len(files[h]))
					}
					data := files[h][offset:end]
					if f.mode == "zero_data" {
						data = nil
					}
					if f.mode == "oversize_data" {
						data = make([]byte, length+1)
					}
					outType = 103
					out = append(stageTestU32(replyID), stageTestString(data)...)
				}
			}
		default:
			f.t.Error("mutating or unexpected SFTP request")
			return
		}
		if f.mode == "oversize_packet" {
			_, _ = ch.Write(stageTestU32(65537))
			return
		}
		if f.mode == "trailing" {
			out = append(out, 0)
		}
		if _, err := ch.Write(stageTestFrame(outType, out)); err != nil {
			return
		}
	}
}

func (f *binaryStageServer) inspector(t *testing.T) *SSHBinaryStageInspector {
	t.Helper()
	i, err := NewSSHBinaryStageInspector(t.Context(), f.key, f.intent.Identity.HostKeyFingerprint, f.intent.Identity.ClientKeyFingerprint, probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestProbeBinaryStageRealSSH(t *testing.T) {
	type realSSHCase struct {
		mode, state string
		err         error
	}
	cases := []realSSHCase{{"match", domain.ProbeBinaryStageContentMatches, nil}, {"extensions", domain.ProbeBinaryStageContentMatches, nil}, {"absent", domain.ProbeBinaryStageAbsent, nil}, {"marker_missing", domain.ProbeBinaryStageConflict, nil}, {"marker_digest", domain.ProbeBinaryStageConflict, nil}, {"foreign_attempt", domain.ProbeBinaryStageConflict, nil}, {"binary_digest", domain.ProbeBinaryStageConflict, nil}, {"size", domain.ProbeBinaryStageConflict, nil}, {"fstat_size", domain.ProbeBinaryStageConflict, nil}, {"truncated", domain.ProbeBinaryStageConflict, nil}, {"grown", domain.ProbeBinaryStageConflict, nil}, {"symlink", domain.ProbeBinaryStageConflict, nil}, {"directory_symlink", domain.ProbeBinaryStageConflict, nil}, {"special", domain.ProbeBinaryStageConflict, nil}}
	inspectionModes := []string{"permission", "version", "extensions_over", "wrong_id", "handle", "attrs", "zero_data", "oversize_data", "oversize_packet", "trailing", "close_failure", "deny_subsystem", "stderr_overflow"}
	inspectionCases := make([]realSSHCase, 0, len(inspectionModes))
	for _, mode := range inspectionModes {
		inspectionCases = append(inspectionCases, realSSHCase{mode: mode, err: domain.ErrProbeBinaryStageInspection})
	}
	cases = slices.Concat(cases, inspectionCases)
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			f := serveBinaryStage(t, tc.mode)
			i := f.inspector(t)
			svc, e := services.NewProbeBinaryStageInspectionService(i)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			got, err := svc.Inspect(ctx, f.intent)
			if err != tc.err || tc.err == nil && (got.State != tc.state || got.Intent != f.intent) || tc.err != nil && got != (domain.ProbeBinaryStageObservation{}) {
				t.Fatal("real SSH observation/cause mismatch", got.State, err)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("remote diagnostics leaked")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, r := range f.sshRequests {
				if r != "subsystem" {
					t.Fatal("SSH exec/env/PTY used")
				}
			}
			for _, kind := range f.kinds {
				switch kind {
				case 1, 3, 4, 5, 7, 8:
				default:
					t.Fatal("mutating SFTP operation")
				}
			}
			if tc.mode == "match" && f.closedHandles != 2 {
				t.Fatal("matched before both handle closes acknowledged")
			}
			if (tc.mode == "symlink" || tc.mode == "special") && f.closedHandles != 0 {
				t.Fatal("special/symlink was opened")
			}
		})
	}
}

func TestProbeBinaryStageTrustAndPolicy(t *testing.T) {
	for _, mode := range []string{"host_pin", "client_pin", "intent_pin", "intent_host_pin", "intent_client_pin", "destination", "dns_numeric", "borrowed_key", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			serverMode := "match"
			if mode == "dns_numeric" {
				serverMode = mode
			}
			f := serveBinaryStage(t, serverMode)
			hp, cp := f.intent.Identity.HostKeyFingerprint, f.intent.Identity.ClientKeyFingerprint
			policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
			if mode == "host_pin" {
				_, other := deploymentKey(t)
				hp = ssh.FingerprintSHA256(other.PublicKey())
			}
			if mode == "client_pin" {
				_, other := deploymentKey(t)
				cp = ssh.FingerprintSHA256(other.PublicKey())
			}
			if mode == "destination" {
				policy = probe.EndpointPolicy{}
			}
			i, err := NewSSHBinaryStageInspector(t.Context(), f.key, hp, cp, policy)
			want := domain.ErrProbeSSHInput
			if err == nil {
				intent := f.intent
				if mode == "host_pin" {
					intent.Identity.HostKeyFingerprint = hp
					want = domain.ErrProbeSSHTrust
				}
				if mode == "intent_pin" {
					intent.OwnerID = "invalid"
					want = domain.ErrProbeBinaryStageInput
				}
				if mode == "intent_host_pin" || mode == "intent_client_pin" {
					_, other := deploymentKey(t)
					if mode == "intent_host_pin" {
						intent.Identity.HostKeyFingerprint = ssh.FingerprintSHA256(other.PublicKey())
					} else {
						intent.Identity.ClientKeyFingerprint = ssh.FingerprintSHA256(other.PublicKey())
					}
				}
				if mode == "borrowed_key" {
					clear(f.key)
					want = nil
				}
				if mode == "destination" {
					want = domain.ErrProbeSSHDestination
				}
				if mode == "dns_numeric" {
					var calls atomic.Int32
					i.host.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
						calls.Add(1)
						return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
					}
					dial := i.host.dial
					i.host.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
						h, _, _ := net.SplitHostPort(address)
						if h != "127.0.0.1" {
							t.Error("hostname redialed")
						}
						return dial(ctx, network, address)
					}
					want = nil
					defer func() {
						if calls.Load() != 1 {
							t.Error("DNS not resolved exactly once")
						}
					}()
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "canceled" {
					cancel()
					want = context.Canceled
				}
				_, err = i.InspectBinaryStage(ctx, intent)
			}
			if err != want {
				t.Fatal("trust/policy refusal mismatch", err, want)
			}
			if mode != "dns_numeric" && mode != "borrowed_key" && f.auth.Load() != 0 {
				t.Fatal("refusal authenticated before trust/policy")
			}
			if mode != "dns_numeric" && mode != "borrowed_key" && mode != "host_pin" && f.accepted.Load() != 0 {
				t.Fatal("invalid request/key/policy dialed")
			}
		})
	}
}

func TestProbeBinaryStageCancelPhases(t *testing.T) {
	for _, phase := range []string{"stall_handshake", "stall_channel", "stall_subsystem", "stall_read"} {
		t.Run(phase, func(t *testing.T) {
			f := serveBinaryStage(t, phase)
			i := f.inspector(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				o, e := i.InspectBinaryStage(ctx, f.intent)
				if o != (domain.ProbeBinaryStageObservation{}) {
					done <- errors.New("canceled observation escaped")
					return
				}
				done <- e
			}()
			select {
			case <-f.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("phase not entered")
			}
			cancel()
			select {
			case e := <-done:
				if e != context.Canceled {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("local SSH work did not join")
			}
		})
	}
}

type binaryStageHeldClose struct {
	net.Conn
	once             atomic.Bool
	entered, release chan struct{}
}

func (c *binaryStageHeldClose) Close() error {
	if c.once.CompareAndSwap(false, true) {
		err := c.Conn.Close()
		close(c.entered)
		<-c.release
		return err
	}
	return nil
}
func TestProbeBinaryStageCancelCallbackJoined(t *testing.T) {
	f := serveBinaryStage(t, "stall_read")
	i := f.inspector(t)
	held := &binaryStageHeldClose{entered: make(chan struct{}), release: make(chan struct{})}
	dial := i.host.dial
	i.host.dial = func(ctx context.Context, n, a string) (net.Conn, error) {
		c, e := dial(ctx, n, a)
		held.Conn = c
		return held, e
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := i.InspectBinaryStage(ctx, f.intent); done <- e }()
	select {
	case <-f.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("read not entered")
	}
	cancel()
	select {
	case <-held.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel close not entered")
	}
	early := false
	select {
	case <-done:
		early = true
	case <-time.After(30 * time.Millisecond):
	}
	close(held.release)
	if early {
		t.Fatal("inspection returned before cancellation callback joined")
	}
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback release did not join")
	}
}

type binaryStageNilContext struct{}

func (*binaryStageNilContext) Deadline() (time.Time, bool) { panic("typed nil") }
func (*binaryStageNilContext) Done() <-chan struct{}       { panic("typed nil") }
func (*binaryStageNilContext) Err() error                  { panic("typed nil") }
func (*binaryStageNilContext) Value(any) any               { panic("typed nil") }
func TestProbeBinaryStageTypedNilContext(t *testing.T) {
	for _, kind := range []string{"constructor", "inspect"} {
		t.Run(kind, func(t *testing.T) {
			defer func() {
				if recover() != nil {
					t.Fatal("typed-nil context method dispatched")
				}
			}()
			var ctx *binaryStageNilContext
			if kind == "constructor" {
				if _, err := NewSSHBinaryStageInspector(ctx, nil, "", "", probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput {
					t.Fatal("typed nil constructor input accepted", err)
				}
				return
			}
			f := serveBinaryStage(t, "match")
			i := f.inspector(t)
			if o, e := i.InspectBinaryStage(ctx, f.intent); e != domain.ErrProbeBinaryStageInput || o != (domain.ProbeBinaryStageObservation{}) {
				t.Fatal("typed nil call accepted", e)
			}
		})
	}
}

type binaryStageShortWriter struct{ calls, shortOn int }

func (w *binaryStageShortWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.shortOn {
		return len(p) - 1, nil
	}
	return len(p), nil
}
func TestProbeBinaryStageShortWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    io.Writer
	}{{"header", &binaryStageShortWriter{shortOn: 1}}, {"payload", &binaryStageShortWriter{shortOn: 2}}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &sftpPipe{ctx: t.Context(), w: tc.w}
			if err := p.writePacket(1, stageTestU32(3)); err == nil {
				t.Fatal("short write accepted as complete framing")
			}
		})
	}
}
