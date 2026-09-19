//go:build linux || darwin

package auth

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func privateKeyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProbeSecretKeyRestartAndBackup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(privateKeyDir(t), "snapshot.key")
	if err := CreateProbeSecretKeyFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != 32 || bytes.Equal(key, make([]byte, 32)) {
		t.Fatal("expected a fresh 32-byte key")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode() != 0600 {
		t.Fatal("key was not published with mode 0600")
	}
	first, err := NewProbeConfigProtectorFromFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	metadata := domain.ProbeConfigMetadata{
		ProbeConfigTarget: domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "local"},
		Revision:          1, SchemaVersion: 1, SHA256: strings.Repeat("a", 64), CreatedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(),
	}
	plaintext := []byte("credential retained across restart")
	ciphertext, err := first.Seal(ctx, metadata, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh adapter reopens the same key; a separately restored backup also
	// decrypts the previously sealed bytes. No key material comes from the DB.
	backup := filepath.Join(privateKeyDir(t), "restored.key")
	if err := os.WriteFile(backup, key, 0400); err != nil {
		t.Fatal(err)
	}
	for _, keyPath := range []string{path, backup} {
		restarted, err := NewProbeConfigProtectorFromFile(ctx, keyPath)
		if err != nil {
			t.Fatal(err)
		}
		got, err := restarted.Open(ctx, metadata, ciphertext)
		if err != nil || !bytes.Equal(got, plaintext) {
			t.Fatal("retained ciphertext did not survive key reload/restore")
		}
	}
	otherPath := filepath.Join(privateKeyDir(t), "other.key")
	if err := CreateProbeSecretKeyFile(ctx, otherPath); err != nil {
		t.Fatal(err)
	}
	other, err := NewProbeConfigProtectorFromFile(ctx, otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := other.Open(ctx, metadata, ciphertext); err == nil || got != nil {
		t.Fatal("another installation's key returned plaintext")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProbeConfigProtectorFromFile(ctx, path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("missing key was not rejected")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("loading silently regenerated the key")
	}
}

func TestProbeSecretKeyConcurrentCreation(t *testing.T) {
	ctx := context.Background()
	dir := privateKeyDir(t)
	path := filepath.Join(dir, "snapshot.key")
	const writers = 20
	start := make(chan struct{})
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			<-start
			results <- CreateProbeSecretKeyFile(ctx, path)
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("unexpected creation error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected one publisher, got %d", winners)
	}
	if _, err := NewProbeConfigProtectorFromFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := CreateProbeSecretKeyFile(ctx, path); !errors.Is(err, fs.ErrExist) {
			t.Fatal("creation did not refuse existing key")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing key changed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "snapshot.key" {
		t.Fatal("successful/losing creators left staging files")
	}
}

func TestProbeSecretKeyRejectsMalformedFiles(t *testing.T) {
	ctx := context.Background()
	for _, size := range []int{0, 1, 16, 31, 33, 44, 64, 4096} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			path := filepath.Join(privateKeyDir(t), "confidential-path.key")
			data := bytes.Repeat([]byte{'s'}, size)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewProbeConfigProtectorFromFile(ctx, path); err == nil || strings.Contains(err.Error(), path) {
				t.Fatal("malformed key accepted or path leaked")
			}
			if err := CreateProbeSecretKeyFile(ctx, path); !errors.Is(err, fs.ErrExist) {
				t.Fatal("creation did not refuse invalid existing file")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("invalid key was silently repaired")
			}
		})
	}
	for _, mode := range []os.FileMode{0000, 0200, 0500, 0640, 0644, 0666, 0700, 0600 | os.ModeSetuid} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(privateKeyDir(t), "snapshot.key")
			if err := os.WriteFile(path, make([]byte, 32), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode&os.ModeSetuid != 0 && info.Mode()&os.ModeSetuid == 0 {
				t.Skip("filesystem does not preserve setuid mode on the fixture")
			}
			if _, err := NewProbeConfigProtectorFromFile(ctx, path); err == nil {
				t.Fatal("unsafe file mode accepted")
			}
		})
	}
	for _, kind := range []string{"directory", "fifo", "device", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(privateKeyDir(t), "snapshot.key")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "device":
				path = "/dev/null"
			case "dangling":
				err = os.Symlink("absent", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewProbeConfigProtectorFromFile(ctx, path); err == nil {
				t.Fatal("non-regular key accepted")
			}
			if kind != "device" {
				if err := CreateProbeSecretKeyFile(ctx, path); !errors.Is(err, fs.ErrExist) {
					t.Fatal("existing non-regular destination was not refused")
				}
			}
		})
	}
}

func TestProbeSecretKeyProjectedMountAndDirectoryTrust(t *testing.T) {
	ctx := context.Background()
	dir := privateKeyDir(t)
	dataDir := filepath.Join(dir, "..timestamp")
	if err := os.Mkdir(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "key"), bytes.Repeat([]byte{10}, 32), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..timestamp", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "key")
	if err := os.Symlink("..data/key", path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// Newline bytes are raw key material, never trimmed.
	if _, err := NewProbeConfigProtectorFromFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := CreateProbeSecretKeyFile(ctx, filepath.Join(dir, "new.key")); err == nil {
		t.Fatal("creation accepted a non-private directory")
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProbeConfigProtectorFromFile(ctx, path); err == nil {
		t.Fatal("loading accepted a writable directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(privateKeyDir(t), "outside.key")
	if err := os.WriteFile(out, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{out, "../" + filepath.Base(filepath.Dir(out)) + "/outside.key"} {
		link := filepath.Join(privateKeyDir(t), "escape.key")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := NewProbeConfigProtectorFromFile(ctx, link); err == nil {
			t.Fatal("symlink escaped the opened directory")
		}
		if err := CreateProbeSecretKeyFile(ctx, link); !errors.Is(err, fs.ErrExist) {
			t.Fatal("creation did not refuse an existing symlink")
		}
	}
}

func TestProbeSecretKeyCancellationAndMissingDirectory(t *testing.T) {
	dir := privateKeyDir(t)
	path := filepath.Join(dir, "key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CreateProbeSecretKeyFile(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("creation ignored cancellation")
	}
	if _, err := NewProbeConfigProtectorFromFile(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("loading ignored cancellation")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("canceled creation wrote a file")
	}
	for _, path := range []string{"", filepath.Join(dir, "missing", "key")} {
		if err := CreateProbeSecretKeyFile(context.Background(), path); err == nil {
			t.Fatal("invalid path accepted")
		}
		if _, err := NewProbeConfigProtectorFromFile(context.Background(), path); err == nil {
			t.Fatal("invalid load path accepted")
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("creation invented a parent directory")
	}
}

func TestProbeSecretKeyOwnerChecks(t *testing.T) {
	path := filepath.Join(privateKeyDir(t), "key")
	if err := os.WriteFile(path, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !probeKeyTrustedOwner(info) || !probeKeyCurrentOwner(info) {
		t.Fatal("current owner rejected")
	}
	stat := info.Sys().(*syscall.Stat_t)
	stat.Uid = 0
	if !probeKeyTrustedOwner(info) {
		t.Fatal("root-owned mount rejected")
	}
	stat.Uid = uint32(os.Geteuid()) + 1
	if probeKeyTrustedOwner(info) || probeKeyCurrentOwner(info) {
		t.Fatal("another user's file accepted")
	}
}
