package provisioning

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

func TestProbeImageArchiveExactBudget(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 2)
	s, _ := snapshotPrepare(t, f)
	first, err := NewProbeImageArchivePacker(t.TempDir()).PackImageArchive(t.Context(), f.Pin, s)
	if err != nil {
		t.Fatal(err)
	}
	size := first.Descriptor().ArchiveSize
	if err = first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int64{0, -1} {
		name := "exact"
		if delta < 0 {
			name = "one_over"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			p := NewProbeImageArchivePacker(base)
			p.limit = size + delta
			a, e := p.PackImageArchive(t.Context(), f.Pin, s)
			if delta == 0 {
				if e != nil || a == nil {
					t.Fatal("exact archive budget refused", e)
				}
				_ = archiveRead(t, a)
				if e = a.Close(t.Context()); e != nil {
					t.Fatal(e)
				}
			} else {
				if a != nil {
					_ = a.Close(t.Context())
				}
				if a != nil || e != domain.ErrProbeImageArchiveStorage {
					t.Fatal("archive one-over budget accepted", e)
				}
			}
			snapshotEmpty(t, base)
		})
	}
}

func TestProbeImageArchiveMaximumEdges(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "arm64", false, false, domain.MaxProbeImageLayers)
	for i := 1; i < len(f.Layers); i++ {
		f.Layers[i] = f.Layers[0]
		f.Uncompressed[i] = f.Uncompressed[0]
	}
	f.RebindContent(t)
	s, _ := snapshotPrepare(t, f)
	a, err := NewProbeImageArchivePacker(t.TempDir()).PackImageArchive(t.Context(), f.Pin, s)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(t.Context()) }()
	entries := archiveGraph(t, archiveRead(t, a))
	if len(entries) != 5 || a.Descriptor().Content.Selection.LayerCount != domain.MaxProbeImageLayers {
		t.Fatal("maximum repeated graph was not bounded/deduplicated")
	}
	s.mu.Lock()
	count := len(s.readers)
	s.mu.Unlock()
	if count != 0 {
		t.Fatal("maximum source bundle not joined")
	}
	b, e := s.Open(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	snapshotCloseBundle(b)
}

func TestProbeImageArchiveLateSourceCloseCancellation(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, false, 1)
	in, all := contentInputs(f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	all[0].onClose = cancel
	fake := &archiveBorrowed{verification: archiveReceipt(f), open: func(context.Context) (ports.ProbeImageContentInputs, error) { return in, nil }}
	base := t.TempDir()
	a, err := NewProbeImageArchivePacker(base).PackImageArchive(ctx, f.Pin, fake)
	if a != nil {
		_ = a.Close(t.Context())
	}
	if a != nil || err != context.Canceled {
		t.Fatal("source-close cancellation did not revoke archive", err)
	}
	contentClosed(t, all)
	if fake.closes.Load() != 0 {
		t.Fatal("borrowed snapshot closed")
	}
	snapshotEmpty(t, base)
}
