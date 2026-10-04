package main

import (
	"context"
	"io"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
)

// The caller holds RuntimeIdentity's exclusive OS lock and has authenticated
// retained configuration and the currently selected TLS identity.
func resetStoppedStream(ctx context.Context, store *edge.Store, planFile string, out, stderr io.Writer) int {
	fail := func(message string) int { _, _ = io.WriteString(stderr, message+"\n"); return 1 }
	info, err := os.Lstat(planFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return fail("Reset plan must be a private regular file of at most 8192 bytes")
	}
	f, err := os.Open(planFile)
	if err != nil {
		return fail("Reset plan is unavailable")
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return fail("Reset plan changed during opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil {
		return fail("Reset plan is unreadable")
	}
	codec := probe.StreamResetCodec{}
	plan, err := codec.DecodeStreamResetPlan(ctx, data)
	if err != nil {
		return fail("Reset plan is invalid; use the exact plan issued by the hub administrator")
	}
	receipt, err := store.ResetStream(ctx, plan)
	if err != nil {
		return fail("Stream reset failed; preserve the original identity, protection key and archive, then retry the same plan")
	}
	data, err = codec.EncodeStreamResetReceipt(ctx, receipt)
	if err != nil {
		return fail("Reset receipt encoding failed; retry the same plan")
	}
	if _, err := out.Write(append(data, '\n')); err != nil {
		return 1
	}
	return 0
}
