package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// NewProbeConfigProtectorFromFile loads exactly 32 raw bytes from a protected
// regular file. It never creates, replaces, trims or decodes key material.
// Relative symlinks within the parent directory support projected secret mounts.
// The operator must keep the parent and its ancestors under trusted ownership.
func NewProbeConfigProtectorFromFile(ctx context.Context, path string) (*ProbeConfigProtector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, name, err := openProbeKeyDirectory(ctx, path, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	// Nonblocking open allows rejecting a FIFO without waiting for a writer.
	file, err := root.OpenFile(name, os.O_RDONLY|probeKeyNonblock, 0)
	if err != nil {
		return nil, probeKeyIOError("open probe secret key", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, probeKeyIOError("inspect probe secret key", err)
	}
	if !info.Mode().IsRegular() || !probeKeyTrustedOwner(info) ||
		(info.Mode() != 0400 && info.Mode() != 0600) {
		return nil, errors.New("probe secret key must be a regular file owned by the current user or root with mode 0400 or 0600")
	}
	if info.Size() != 32 {
		return nil, errors.New("probe secret key must contain exactly 32 raw bytes")
	}
	var key [33]byte
	defer clear(key[:])
	// Read one extra byte to reject growth since Stat, with a strict upper bound.
	n, err := io.ReadFull(file, key[:])
	if n != 32 || !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, errors.New("probe secret key must contain exactly 32 readable raw bytes")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return NewProbeConfigProtector(key[:32])
}

// CreateProbeSecretKeyFile explicitly provisions a fresh installation key.
// It requires an existing mode-0700 directory owned by the current user, writes
// and syncs a mode-0600 staging file, then publishes it with an atomic no-replace
// hard link and syncs the directory. Existing destinations are never modified.
// On any error after publication, retain the destination and inspect it; never
// delete or regenerate a key that may already protect persisted snapshots.
func CreateProbeSecretKeyFile(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, name, err := openProbeKeyDirectory(ctx, path, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	// Refuse regular files, symlinks (even dangling), and every other existing
	// destination before generating material. Link repeats this check atomically.
	if _, err := root.Lstat(name); err == nil {
		return fmt.Errorf("create probe secret key: %w", fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return probeKeyIOError("inspect probe secret key destination", err)
	}
	var key [32]byte
	defer clear(key[:])
	if _, err := rand.Read(key[:]); err != nil {
		return errors.New("generate probe secret key failed")
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return errors.New("generate probe secret key staging name failed")
	}
	staging := ".phoenix-probe-key-" + hex.EncodeToString(suffix[:])
	file, err := root.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return probeKeyIOError("create probe secret key staging file", err)
	}
	defer func() { _ = file.Close() }()
	defer func() { _ = root.Remove(staging) }()
	// Set the exact mode on the open descriptor even under a restrictive umask.
	if err := file.Chmod(0600); err != nil {
		return probeKeyIOError("protect probe secret key staging file", err)
	}
	if n, err := file.Write(key[:]); err != nil || n != len(key) {
		return errors.New("write probe secret key staging file failed")
	}
	if err := file.Sync(); err != nil {
		return probeKeyIOError("sync probe secret key staging file", err)
	}
	if err := file.Close(); err != nil {
		return probeKeyIOError("close probe secret key staging file", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Link(staging, name); err != nil {
		return probeKeyIOError("publish probe secret key", err)
	}
	// Once published, complete durability even if the caller cancels. A failure
	// here is ambiguous to the caller, so preserve the key for recovery.
	if err := root.Remove(staging); err != nil {
		return probeKeyIOError("remove probe secret key staging link; destination retained", err)
	}
	dir, err := root.Open(".")
	if err != nil {
		return probeKeyIOError("open probe secret key directory for sync; destination retained", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return probeKeyIOError("sync probe secret key directory; destination retained", err)
	}
	return nil
}

func openProbeKeyDirectory(ctx context.Context, path string, creating bool) (*os.Root, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if path == "" || filepath.Base(path) == "." || filepath.Base(path) == ".." || os.IsPathSeparator(path[len(path)-1]) {
		return nil, "", errors.New("probe secret key file path is required")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, "", probeKeyIOError("open probe secret key directory", err)
	}
	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, "", probeKeyIOError("inspect probe secret key directory", err)
	}
	valid := info.IsDir() && probeKeyTrustedOwner(info) && info.Mode().Perm()&0022 == 0
	if creating {
		valid = valid && probeKeyCurrentOwner(info) && info.Mode().Perm() == 0700
	}
	if !valid {
		_ = root.Close()
		if creating {
			return nil, "", errors.New("probe secret key creation requires a mode-0700 directory owned by the current user")
		}
		return nil, "", errors.New("probe secret key directory must be owned by the current user or root and not writable by group or others")
	}
	return root, filepath.Base(path), nil
}

// Retain useful filesystem sentinels without returning paths or raw diagnostics.
func probeKeyIOError(operation string, err error) error {
	for _, kind := range []error{fs.ErrNotExist, fs.ErrExist, fs.ErrPermission, fs.ErrInvalid} {
		if errors.Is(err, kind) {
			return fmt.Errorf("%s: %w", operation, kind)
		}
	}
	return errors.New(operation + " failed")
}
