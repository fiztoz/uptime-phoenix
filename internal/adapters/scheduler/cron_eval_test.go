package scheduler

import (
	"testing"
	"time"
)

func TestCronEvaluator_UTC(t *testing.T) {
	eval := NewCronEvaluator()
	// Fixed: every day at 02:00 UTC, duration 60 minutes.
	// Pick a moment at 02:30 UTC.
	now := time.Date(2026, 3, 15, 2, 30, 0, 0, time.UTC)
	if !eval.IsWindowActive("0 2 * * *", 60, now, time.UTC) {
		t.Fatal("expected active at 02:30 UTC for 0 2 * * * / 60m")
	}
	// Before start (01:59) — inactive.
	before := time.Date(2026, 3, 15, 1, 59, 0, 0, time.UTC)
	if eval.IsWindowActive("0 2 * * *", 60, before, time.UTC) {
		t.Fatal("expected inactive at 01:59 UTC")
	}
	// At exact start — inclusive.
	start := time.Date(2026, 3, 15, 2, 0, 0, 0, time.UTC)
	if !eval.IsWindowActive("0 2 * * *", 60, start, time.UTC) {
		t.Fatal("expected active at exact start 02:00 UTC")
	}
	// At exact end — exclusive.
	end := time.Date(2026, 3, 15, 3, 0, 0, 0, time.UTC)
	if eval.IsWindowActive("0 2 * * *", 60, end, time.UTC) {
		t.Fatal("expected inactive at exclusive end 03:00 UTC")
	}
}

func TestCronEvaluator_AsiaBangkok(t *testing.T) {
	eval := NewCronEvaluator()
	bkk, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Fatal(err)
	}
	// 02:00 Bangkok = 19:00 previous day UTC.
	// At 02:15 Bangkok the window started at 02:00 BKK should be active.
	nowBKK := time.Date(2026, 3, 15, 2, 15, 0, 0, bkk)
	if !eval.IsWindowActive("0 2 * * *", 60, nowBKK, bkk) {
		t.Fatal("expected active at 02:15 Asia/Bangkok for 0 2 * * *")
	}
	// Same absolute instant evaluated in UTC with a UTC schedule would depend
	// on UTC hour 19 — not 2. Prove timezone actually shifts evaluation:
	nowUTC := nowBKK.UTC() // 19:15 previous day UTC on Mar 14
	if eval.IsWindowActive("0 2 * * *", 60, nowUTC, time.UTC) {
		t.Fatal("02:15 BKK wall-clock must NOT match UTC 02:00 schedule")
	}
}

func TestCronEvaluator_AmericaNewYork_DSTSpringForward(t *testing.T) {
	eval := NewCronEvaluator()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-08: US clocks spring forward 02:00 → 03:00.
	// Schedule at 01:30 America/New_York for 120 minutes.
	// Before spring-forward the window covers 01:30–03:30 local (with the gap).
	// A time at 01:45 EST should be inside.
	before := time.Date(2026, 3, 8, 1, 45, 0, 0, ny)
	if !eval.IsWindowActive("30 1 * * *", 120, before, ny) {
		t.Fatal("expected active at 01:45 America/New_York before spring-forward")
	}
	// After spring-forward, 03:15 EDT is still within 120m of 01:30 EST start
	// (01:30 + 120m = 03:30 EDT wall after the jump).
	after := time.Date(2026, 3, 8, 3, 15, 0, 0, ny)
	if !eval.IsWindowActive("30 1 * * *", 120, after, ny) {
		t.Fatal("expected active at 03:15 America/New_York after spring-forward within duration")
	}
	// Far outside.
	late := time.Date(2026, 3, 8, 5, 0, 0, 0, ny)
	if eval.IsWindowActive("30 1 * * *", 120, late, ny) {
		t.Fatal("expected inactive at 05:00 America/New_York")
	}
}

func TestCronEvaluator_NilLocationDefaultsUTC(t *testing.T) {
	eval := NewCronEvaluator()
	now := time.Date(2026, 3, 15, 2, 30, 0, 0, time.UTC)
	if !eval.IsWindowActive("0 2 * * *", 60, now, nil) {
		t.Fatal("nil loc should behave as UTC")
	}
}

func TestCronEvaluator_InvalidExpr(t *testing.T) {
	eval := NewCronEvaluator()
	now := time.Now().UTC()
	if eval.IsWindowActive("not a cron", 60, now, time.UTC) {
		t.Fatal("invalid cron must return false")
	}
}
