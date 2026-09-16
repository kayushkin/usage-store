package codex

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeRollout(t *testing.T, root, day, name, timestamp string, fiveHourPercent float64, mtime time.Time) {
	t.Helper()
	dir := filepath.Join(root, day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"` + timestamp + `","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":` +
		strconv.FormatFloat(fiveHourPercent, 'f', -1, 64) + `,"window_minutes":300,"resets_at":1789504081},"secondary":{"used_percent":12,"window_minutes":10080,"resets_at":1790014681},"plan_type":"plus"}}}` + "\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// The defect this pins, measured live on 2026-09-16: a session started on 09-14
// kept writing until 09-15 17:28 (68% of the five-hour window), while a short
// session started on 09-15 last wrote at 17:17 (45%). The reader descended the
// newest DAY directory first and returned 45% — the older snapshot.
func TestLatestPrefersNewestSnapshotOverNewestStartDay(t *testing.T) {
	root := t.TempDir()
	writeRollout(t, root, "2026/09/15", "rollout-2026-09-15T17-17-42-short.jsonl",
		"2026-09-15T17:17:58.402Z", 45, time.Date(2026, 9, 15, 17, 17, 58, 0, time.UTC))
	writeRollout(t, root, "2026/09/14", "rollout-2026-09-14T18-26-10-long.jsonl",
		"2026-09-15T17:28:30.319Z", 68, time.Date(2026, 9, 15, 17, 28, 30, 0, time.UTC))

	reader := &Reader{SessionsDir: root}
	out, _, err := reader.Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if out == nil {
		t.Fatal("Latest returned no snapshot")
	}
	if got := out.Windows[WindowFiveHour].UsedPercent; got != 68 {
		t.Errorf("five-hour used = %v%%, want 68%% from the long session's later snapshot", got)
	}
	want := time.Date(2026, 9, 15, 17, 28, 30, 319000000, time.UTC).Unix()
	if out.SnapshotAt != want {
		t.Errorf("snapshot_at = %d, want %d", out.SnapshotAt, want)
	}
}

func TestLatestWithNoSessionsDirectoryReturnsNothing(t *testing.T) {
	reader := &Reader{SessionsDir: filepath.Join(t.TempDir(), "absent")}
	out, _, err := reader.Latest()
	if err != nil || out != nil {
		t.Fatalf("Latest = %+v, %v; want nil, nil", out, err)
	}
}
