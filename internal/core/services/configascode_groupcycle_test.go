package services_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- observable group fake -------------------------------------------------
// Unlike cfgNilGroup this records writes so "the rejected document made no
// resource changes" is an assertion, not an assumption.

type cfgCycleGroupRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.MonitorGroup
	nextID int64
	writes int
}

func newCfgCycleGroupRepo() *cfgCycleGroupRepo {
	return &cfgCycleGroupRepo{byID: map[int64]*domain.MonitorGroup{}}
}

func (r *cfgCycleGroupRepo) Create(_ context.Context, g *domain.MonitorGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.writes++
	g.ID = r.nextID
	cp := *g
	r.byID[g.ID] = &cp
	return nil
}

func (r *cfgCycleGroupRepo) GetByID(_ context.Context, id int64) (*domain.MonitorGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *g
	return &cp, nil
}

func (r *cfgCycleGroupRepo) List(_ context.Context, _ int64) ([]*domain.MonitorGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MonitorGroup, 0, len(r.byID))
	for _, g := range r.byID {
		cp := *g
		out = append(out, &cp)
	}
	return out, nil
}

func (r *cfgCycleGroupRepo) ListAll(ctx context.Context) ([]*domain.MonitorGroup, error) {
	return r.List(ctx, 0)
}

func (r *cfgCycleGroupRepo) Update(_ context.Context, g *domain.MonitorGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[g.ID]; !ok {
		return ports.ErrNotFound
	}
	r.writes++
	cp := *g
	r.byID[g.ID] = &cp
	return nil
}

func (r *cfgCycleGroupRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; ok {
		r.writes++
	}
	delete(r.byID, id)
	return nil
}

func (r *cfgCycleGroupRepo) ClaimStatusTransition(context.Context, int64, *domain.Status, domain.Status) (bool, error) {
	return false, nil
}

// --- config service with observable stores ---------------------------------

type cycleCfg struct {
	svc    *services.ConfigService
	keys   *memory.ConfigKeyRepo
	tags   *cfgTagRepo
	notifs *cfgNotifRepo
	mons   *cfgMonRepo
	groups *cfgCycleGroupRepo
}

func newCycleCfg(t *testing.T) *cycleCfg {
	t.Helper()
	c := &cycleCfg{
		keys:   memory.NewConfigKeyRepo(),
		tags:   newCfgTagRepo(),
		notifs: newCfgNotifRepo(),
		mons:   newCfgMonRepo(),
		groups: newCfgCycleGroupRepo(),
	}
	c.svc = services.NewConfigService(
		c.keys, c.tags, cfgNilProxy{}, c.notifs, c.groups, c.mons,
		cfgNilMT{}, cfgNilMN{}, cfgNilGN{}, cfgNilSP{}, cfgNilSPM{},
		cfgNilMaint{}, cfgNilMM{}, cfgPass{},
	)
	return c
}

// assertNoWrites verifies the rejected document mutated nothing.
func (c *cycleCfg) assertNoWrites() error {
	if c.groups.writes != 0 {
		return fmt.Errorf("monitor_group repo received %d writes", c.groups.writes)
	}
	if len(c.mons.byID) != 0 || len(c.tags.byID) != 0 || len(c.notifs.byID) != 0 {
		return fmt.Errorf("resource rows created: monitors=%d tags=%d notifications=%d",
			len(c.mons.byID), len(c.tags.byID), len(c.notifs.byID))
	}
	for _, kind := range []string{
		domain.ConfigResourceMonitorGroup, domain.ConfigResourceMonitor,
		domain.ConfigResourceTag, domain.ConfigResourceNotification,
	} {
		keys, err := c.keys.ListByType(context.Background(), kind)
		if err != nil {
			return err
		}
		if len(keys) != 0 {
			return fmt.Errorf("%s config keys created: %v", kind, keys)
		}
	}
	return nil
}

// --- cycle fixtures --------------------------------------------------------

// groupCycleDocs holds one document per cycle shape: self-parent, two-group,
// and a longer chain.
var groupCycleDocs = map[string][]services.ConfigMonitorGroup{
	"self":  {{Key: "self", Name: "Self", Parent: "self"}},
	"two":   {{Key: "a", Name: "A", Parent: "b"}, {Key: "b", Name: "B", Parent: "a"}},
	"three": {{Key: "a", Name: "A", Parent: "b"}, {Key: "b", Name: "B", Parent: "c"}, {Key: "c", Name: "C", Parent: "a"}},
}

func cycleDoc(groups []services.ConfigMonitorGroup) *services.ConfigDocument {
	return &services.ConfigDocument{
		APIVersion: services.ConfigAPIVersion,
		Kind:       services.ConfigKind,
		Spec:       services.ConfigSpec{MonitorGroups: groups},
	}
}

// TestConfigValidate_RejectsGroupParentCycles asserts validation reports every
// cycle shape with the offending chain instead of accepting it (issue #64).
func TestConfigValidate_RejectsGroupParentCycles(t *testing.T) {
	for name, groups := range groupCycleDocs {
		t.Run(name, func(t *testing.T) {
			cfg := newCycleCfg(t)
			errs := cfg.svc.Validate(context.Background(), cycleDoc(groups))
			if len(errs) == 0 {
				t.Fatal("Validate accepted a cyclic group parent document")
			}
			joined := strings.Join(errs, "; ")
			if !strings.Contains(joined, "cycle") {
				t.Fatalf("validation errors do not mention a cycle: %v", errs)
			}
			for _, g := range groups {
				if !strings.Contains(joined, g.Key) {
					t.Fatalf("validation errors do not name offending group %q: %v", g.Key, errs)
				}
			}
		})
	}
}

// TestConfigApply_GroupParentCycle_MakesNoChanges asserts Apply rejects a cycle
// before any resource change (in-process; the recursion itself is proven fatal
// in the isolated helper below).
func TestConfigApply_GroupParentCycle_MakesNoChanges(t *testing.T) {
	for name, groups := range groupCycleDocs {
		t.Run(name, func(t *testing.T) {
			cfg := newCycleCfg(t)
			_, err := cfg.svc.Apply(context.Background(), 1, cycleDoc(groups), services.ConfigApplyOptions{})
			if err == nil {
				t.Fatal("Apply accepted a cyclic group parent document")
			}
			if !strings.Contains(err.Error(), "cycle") {
				t.Fatalf("apply error does not mention a cycle: %v", err)
			}
			if werr := cfg.assertNoWrites(); werr != nil {
				t.Fatalf("rejected document still mutated state: %v", werr)
			}
		})
	}
}

// TestConfigApply_GroupOrdering_ParentsFirst asserts valid declarations apply
// regardless of declaration order, with multiple roots and deep nesting.
func TestConfigApply_GroupOrdering_ParentsFirst(t *testing.T) {
	cfg := newCycleCfg(t)
	doc := cycleDoc([]services.ConfigMonitorGroup{
		{Key: "leaf", Name: "Leaf", Parent: "mid"},
		{Key: "other-root", Name: "Other Root"},
		{Key: "mid", Name: "Mid", Parent: "root"},
		{Key: "root", Name: "Root"},
	})
	if _, err := cfg.svc.Apply(context.Background(), 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatalf("valid groups rejected: %v", err)
	}
	byKey := map[string]*domain.MonitorGroup{}
	for _, g := range cfg.groups.byID {
		byKey[g.Name] = g
	}
	if byKey["Leaf"] == nil || byKey["Mid"] == nil || byKey["Root"] == nil || byKey["Other Root"] == nil {
		t.Fatalf("groups missing after apply: %+v", cfg.groups.byID)
	}
	if byKey["Mid"].ParentID == nil || *byKey["Mid"].ParentID != byKey["Root"].ID {
		t.Fatalf("mid.ParentID = %v, want %d", byKey["Mid"].ParentID, byKey["Root"].ID)
	}
	if byKey["Leaf"].ParentID == nil || *byKey["Leaf"].ParentID != byKey["Mid"].ID {
		t.Fatalf("leaf.ParentID = %v, want %d", byKey["Leaf"].ParentID, byKey["Mid"].ID)
	}
	if byKey["Root"].ParentID != nil || byKey["Other Root"].ParentID != nil {
		t.Fatalf("roots must stay top-level: %+v %+v", byKey["Root"], byKey["Other Root"])
	}
}

// --- isolated fatal-path regression ----------------------------------------
//
// Before issue #64 was fixed, Apply recursed through orderConfigGroups forever
// and the Go runtime died with "fatal error: stack overflow" — uncatchable and
// process-fatal. The regression is therefore exercised in a helper subprocess
// with a lowered stack cap (debug.SetMaxStack) so the fatal path cannot take
// the test runner down with it. The parent asserts the child rejected the
// document in-process and exited cleanly.

const cycleHelperEnv = "PHOENIX_CONFIG_GROUP_CYCLE_HELPER"

func TestConfigGroupCycleHelperProcess(t *testing.T) {
	variant := os.Getenv(cycleHelperEnv)
	if variant == "" {
		t.Skip("helper subprocess entry; run via TestConfigGroupCycle_DoesNotKillProcess")
	}
	groups, ok := groupCycleDocs[variant]
	if !ok {
		fmt.Printf("FAIL: unknown variant %q\n", variant)
		os.Exit(2)
	}
	cfg := newCycleCfg(t)
	ctx := context.Background()
	doc := cycleDoc(groups)

	var problems []string
	if errs := cfg.svc.Validate(ctx, doc); len(errs) == 0 {
		problems = append(problems, "Validate accepted a cyclic parent document")
	}

	// The former fatal path: Apply used to recurse into orderConfigGroups until
	// the stack died. The capped stack bounds this subprocess's resource use and
	// contains that crash so the parent can report it.
	prev := debug.SetMaxStack(64 * 1024)
	_, err := cfg.svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{})
	debug.SetMaxStack(prev)

	if err == nil {
		problems = append(problems, "Apply accepted a cyclic parent document")
	} else if !strings.Contains(err.Error(), "cycle") {
		problems = append(problems, fmt.Sprintf("apply error lacks cycle detail: %v", err))
	}
	if werr := cfg.assertNoWrites(); werr != nil {
		problems = append(problems, fmt.Sprintf("rejected document still mutated state: %v", werr))
	}
	if len(problems) > 0 {
		fmt.Printf("FAIL: %s\n", strings.Join(problems, "; "))
		os.Exit(1)
	}
	fmt.Printf("REJECTED: %v\n", err)
	os.Exit(0)
}

func TestConfigGroupCycle_DoesNotKillProcess(t *testing.T) {
	for _, variant := range []string{"self", "two", "three"} {
		t.Run(variant, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestConfigGroupCycleHelperProcess$", "-test.timeout=60s")
			cmd.Env = append(os.Environ(), cycleHelperEnv+"="+variant)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			err := cmd.Run()
			if err != nil {
				t.Fatalf("helper subprocess died on the cyclic document (%v); the service process must survive it.\noutput:\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), "REJECTED") {
				t.Fatalf("helper did not report an in-process rejection:\n%s", out.String())
			}
		})
	}
}
