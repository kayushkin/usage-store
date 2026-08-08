package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

func i64(v int64) *int64 { return &v }

func TestKeyForWindow(t *testing.T) {
	cases := []struct {
		name     string
		minutes  *int64
		position string
		want     string
	}{
		{"five hour by length", i64(300), WindowPrimary, WindowFiveHour},
		{"weekly by length", i64(10080), WindowSecondary, WindowWeekly},
		{"unrecognised length keeps its raw key", i64(60), WindowPrimary, "60m"},
		{"absent length falls back to primary position", nil, WindowPrimary, WindowPrimary},
		{"absent length falls back to secondary position", nil, WindowSecondary, WindowSecondary},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := keyForWindow(c.minutes, c.position); got != c.want {
				t.Fatalf("keyForWindow(%v, %q) = %q, want %q", c.minutes, c.position, got, c.want)
			}
		})
	}
}

// The defect this pins: window_minutes is optional on the wire. When BOTH windows
// omit it, the old code keyed both on the literal "unknown" and the second write
// silently replaced the first, so a snapshot stating two limits was stored as one.
func TestNormaliseKeepsBothWindowsWhenNeitherStatesItsLength(t *testing.T) {
	snap := &rateLimits{
		Primary:   &rateLimitWindow{UsedPercent: 11, ResetsAt: i64(100)},
		Secondary: &rateLimitWindow{UsedPercent: 22, ResetsAt: i64(200)},
	}
	out, err := normalise(snap, time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if len(out.Windows) != 2 {
		t.Fatalf("got %d windows %v, want 2 — a window was dropped", len(out.Windows), keysOf(out.Windows))
	}
	if w := out.Windows[WindowPrimary]; w == nil || w.UsedPercent != 11 {
		t.Errorf("primary window = %+v, want 11%%", w)
	}
	if w := out.Windows[WindowSecondary]; w == nil || w.UsedPercent != 22 {
		t.Errorf("secondary window = %+v, want 22%%", w)
	}
	if _, ok := out.Windows["unknown"]; ok {
		t.Error(`a window is still keyed "unknown"; position is the distinguishing fact, not a placeholder`)
	}
}

// A single window omitting its length must not borrow the other's name. This is the
// asymmetric case: one real key, one positional key, and they must not collide.
func TestNormaliseMixesPositionalAndLengthKeys(t *testing.T) {
	snap := &rateLimits{
		Primary:   &rateLimitWindow{UsedPercent: 5},
		Secondary: &rateLimitWindow{UsedPercent: 60, WindowMinutes: i64(10080)},
	}
	out, err := normalise(snap, time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if len(out.Windows) != 2 {
		t.Fatalf("got %v, want a positional key and a weekly key", keysOf(out.Windows))
	}
	if out.Windows[WindowPrimary] == nil {
		t.Errorf("positional key missing: %v", keysOf(out.Windows))
	}
	// The autoworker gates a dispatch on exactly this lookup; if it moves, the
	// nightly worker silently stops firing.
	if w := out.Windows[WindowWeekly]; w == nil || w.UsedPercent != 60 {
		t.Errorf("weekly window = %+v, want 60%%", w)
	}
}

// The case the card's proposed fix does NOT cover: a positional fallback only
// triggers on a nil length, so two windows reporting the SAME length still collide.
// Refuse rather than drop one.
func TestNormaliseRefusesTwoWindowsOfTheSameLength(t *testing.T) {
	snap := &rateLimits{
		Primary:   &rateLimitWindow{UsedPercent: 10, WindowMinutes: i64(300)},
		Secondary: &rateLimitWindow{UsedPercent: 90, WindowMinutes: i64(300)},
	}
	out, err := normalise(snap, time.Unix(1000, 0))
	if err == nil {
		t.Fatalf("normalise succeeded with %v; want an error rather than a silent drop", keysOf(out.Windows))
	}
	// The message has to carry both readings, or the operator cannot tell which
	// limit was in question.
	for _, want := range []string{WindowFiveHour, "10.00", "90.00"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNormaliseAcceptsAMissingSecondWindow(t *testing.T) {
	// Measured on the live store: 3 of 169 codex snapshots carry only a weekly
	// window. That is legitimate input and must not become an error.
	snap := &rateLimits{Primary: &rateLimitWindow{UsedPercent: 0, WindowMinutes: i64(10080)}}
	out, err := normalise(snap, time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if len(out.Windows) != 1 || out.Windows[WindowWeekly] == nil {
		t.Fatalf("got %v, want exactly the weekly window", keysOf(out.Windows))
	}
}

func TestNormaliseCarriesPlanTypeAndProvenance(t *testing.T) {
	plan := "pro"
	snap := &rateLimits{
		PlanType: &plan,
		Primary:  &rateLimitWindow{UsedPercent: 1, WindowMinutes: i64(300)},
	}
	out, err := normalise(snap, time.Unix(1234, 0))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if out.PlanType != "pro" || out.Provider != "codex" || out.Source != "rollout" {
		t.Fatalf("provenance lost: %+v", out)
	}
	if out.SnapshotAt != 1234 {
		t.Fatalf("SnapshotAt = %d, want 1234", out.SnapshotAt)
	}
}

// End-to-end: a contradictory snapshot must surface out of Latest() rather than be
// skipped in favour of an older rollout, which would report stale limits as fresh.
func TestLatestPropagatesAContradictorySnapshot(t *testing.T) {
	dir := t.TempDir()
	day := filepath.Join(dir, "2026", "08", "08")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	line := map[string]any{
		"timestamp": "2026-08-08T09:00:00Z",
		"type":      "event_msg",
		"payload": map[string]any{
			"type": "token_count",
			"rate_limits": map[string]any{
				"primary":   map[string]any{"used_percent": 10, "window_minutes": 300},
				"secondary": map[string]any{"used_percent": 90, "window_minutes": 300},
			},
		},
	}
	blob, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "rollout-x.jsonl"), append(blob, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Reader{SessionsDir: dir}
	out, _, err := r.Latest()
	if err == nil {
		t.Fatalf("Latest returned %+v with no error; a self-contradictory snapshot must not pass silently", out)
	}
	if !strings.Contains(err.Error(), "refusing to drop one") {
		t.Fatalf("error %q does not explain the refusal", err)
	}
}

func TestLatestReadsAWellFormedSnapshot(t *testing.T) {
	dir := t.TempDir()
	day := filepath.Join(dir, "2026", "08", "08")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-08-08T09:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":12.5,"window_minutes":300},"secondary":{"used_percent":40,"window_minutes":10080}}}}`
	if err := os.WriteFile(filepath.Join(day, "rollout-x.jsonl"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Reader{SessionsDir: dir}
	out, raw, err := r.Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if out == nil {
		t.Fatal("Latest returned no snapshot")
	}
	if w := out.Windows[WindowFiveHour]; w == nil || w.UsedPercent != 12.5 {
		t.Errorf("five_hour = %+v, want 12.5%%", w)
	}
	if w := out.Windows[WindowWeekly]; w == nil || w.UsedPercent != 40 {
		t.Errorf("weekly = %+v, want 40%%", w)
	}
	if len(raw) == 0 {
		t.Error("raw snapshot line was not retained")
	}
}

// keysOf names the window keys in a stable order, so a failure message says which
// windows survived rather than just how many.
func keysOf(m map[string]*usagestore.LimitWindow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
