// Package codex extracts Codex CLI subscription limits from local rollout JSONL files.
//
// The Codex CLI writes a RateLimitSnapshot into every session rollout under
// ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl on each `token_count` event.
// Reading the latest such file is enough to recover the current 5-hour and
// weekly utilisation without making any API calls.
//
// Caveat: the snapshot is only as fresh as the last Codex interaction. The
// caller decides what counts as stale (StaleAfter on the returned snapshot).
package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

// Window names exposed in ProviderLimits.Windows.
//
// Codex names them generically as primary/secondary; we map by window_minutes:
// 300 → five_hour, 10080 → weekly. Anything else falls through with its raw key.
const (
	WindowFiveHour = "five_hour"
	WindowWeekly   = "weekly"
)

// rateLimitWindow matches the wire format inside rollout JSONL.
type rateLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes *int64  `json:"window_minutes"`
	ResetsAt      *int64  `json:"resets_at"` // unix seconds
}

type rateLimits struct {
	LimitID   string           `json:"limit_id"`
	LimitName *string          `json:"limit_name"`
	Primary   *rateLimitWindow `json:"primary"`
	Secondary *rateLimitWindow `json:"secondary"`
	PlanType  *string          `json:"plan_type"`
}

type eventPayload struct {
	Type       string      `json:"type"`
	RateLimits *rateLimits `json:"rate_limits"`
}

type rolloutLine struct {
	Timestamp string        `json:"timestamp"`
	Type      string        `json:"type"`
	Payload   *eventPayload `json:"payload"`
}

// Reader walks the local Codex sessions directory.
type Reader struct {
	// SessionsDir defaults to ~/.codex/sessions when empty.
	SessionsDir string
	// MaxAge is how long a rollout snapshot is considered fresh.
	// Snapshots older than MaxAge are still returned, but with StaleAfter set in the past.
	MaxAge time.Duration
}

// New returns a Reader with sane defaults: ~/.codex/sessions, 2h MaxAge.
func New() *Reader {
	home, _ := os.UserHomeDir()
	return &Reader{
		SessionsDir: filepath.Join(home, ".codex", "sessions"),
		MaxAge:      2 * time.Hour,
	}
}

// Latest scans the configured sessions directory and returns the most recent
// rate-limit snapshot recorded by the Codex CLI.
//
// Returns (nil, nil) if no rollout files exist at all.
func (r *Reader) Latest() (*usagestore.ProviderLimits, []byte, error) {
	if r.SessionsDir == "" {
		home, _ := os.UserHomeDir()
		r.SessionsDir = filepath.Join(home, ".codex", "sessions")
	}

	files, err := recentRollouts(r.SessionsDir, 10)
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, nil
	}

	// A rollout is named and filed by the day its session STARTED, but Codex keeps
	// appending to it for as long as the session lives. A session started yesterday
	// can hold a newer snapshot than one started today, so neither the directory nor
	// the file order decides: the snapshot with the newest line timestamp wins.
	var (
		bestSnap *rateLimits
		bestRaw  []byte
		bestTime time.Time
	)
	for _, f := range files {
		snap, raw, ts, err := scanRollout(f)
		if err != nil {
			// Skip unreadable files; the next one might be fine.
			continue
		}
		if snap == nil {
			continue
		}
		if bestSnap == nil || ts.After(bestTime) {
			bestSnap, bestRaw, bestTime = snap, raw, ts
		}
	}
	if bestSnap == nil {
		return nil, nil, nil
	}
	out := normalise(bestSnap, bestTime)
	if r.MaxAge > 0 {
		staleAt := bestTime.Add(r.MaxAge).Unix()
		out.StaleAfter = &staleAt
	}
	return out, bestRaw, nil
}

// recentRollouts returns up to `limit` rollout JSONL paths, newest first by mtime.
//
// It orders every rollout by modification time, not by the YYYY/MM/DD directory it
// sits in: that directory records when the session started, and a long session
// keeps writing to it days later. Walking a few hundred directory entries is cheap
// next to reporting an old snapshot as the latest one.
func recentRollouts(root string, limit int) ([]string, error) {
	type entry struct {
		path string
		mod  time.Time
	}
	var entries []entry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root && os.IsNotExist(err) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || filepath.Ext(d.Name()) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, entry{path: path, mod: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.After(entries[j].mod) })
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.path
	}
	return out, nil
}

// scanRollout reads a JSONL file and returns the last `rate_limits` payload,
// the raw JSON of that line, and the timestamp parsed from the line.
func scanRollout(path string) (*rateLimits, []byte, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	defer f.Close()

	var (
		lastSnap *rateLimits
		lastRaw  []byte
		lastTime time.Time
	)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var rl rolloutLine
		if err := json.Unmarshal(line, &rl); err != nil {
			continue
		}
		if rl.Payload == nil || rl.Payload.RateLimits == nil {
			continue
		}
		// Copy line because scanner reuses its buffer.
		rawCopy := make([]byte, len(line))
		copy(rawCopy, line)
		lastSnap = rl.Payload.RateLimits
		lastRaw = rawCopy
		if t, err := time.Parse(time.RFC3339Nano, rl.Timestamp); err == nil {
			lastTime = t
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, time.Time{}, err
	}
	if lastSnap != nil && lastTime.IsZero() {
		// Fall back to file mtime if the line was missing a usable timestamp.
		if info, err := os.Stat(path); err == nil {
			lastTime = info.ModTime()
		}
	}
	return lastSnap, lastRaw, lastTime, nil
}

// normalise maps Codex's primary/secondary windows onto stable keys.
func normalise(snap *rateLimits, snapAt time.Time) *usagestore.ProviderLimits {
	out := &usagestore.ProviderLimits{
		Provider:   "codex",
		SnapshotAt: snapAt.Unix(),
		Source:     "rollout",
		Windows:    map[string]*usagestore.LimitWindow{},
	}
	if snap.PlanType != nil {
		out.PlanType = *snap.PlanType
	}
	addWindow := func(w *rateLimitWindow) {
		if w == nil {
			return
		}
		key := keyForWindow(w.WindowMinutes)
		out.Windows[key] = &usagestore.LimitWindow{
			UsedPercent:   w.UsedPercent,
			WindowMinutes: w.WindowMinutes,
			ResetsAt:      w.ResetsAt,
		}
	}
	addWindow(snap.Primary)
	addWindow(snap.Secondary)
	return out
}

func keyForWindow(min *int64) string {
	if min == nil {
		return "unknown"
	}
	switch *min {
	case 300:
		return WindowFiveHour
	case 10080:
		return WindowWeekly
	default:
		return fmt.Sprintf("%dm", *min)
	}
}

// Walk is a tiny helper exposed for tests.
func Walk(root string, fn func(path string, info fs.FileInfo) error) error {
	return filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return fn(path, info)
	})
}
