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
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   *eventPayload  `json:"payload"`
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

	for _, f := range files {
		snap, raw, ts, err := scanRollout(f)
		if err != nil {
			// Skip unreadable files; the next one might be fine.
			continue
		}
		if snap == nil {
			continue
		}
		out := normalise(snap, ts)
		if r.MaxAge > 0 {
			staleAt := ts.Add(r.MaxAge).Unix()
			out.StaleAfter = &staleAt
		}
		return out, raw, nil
	}

	return nil, nil, nil
}

// recentRollouts returns up to `limit` rollout JSONL paths, newest first by mtime.
//
// To avoid walking thousands of historical files, we descend YYYY/MM/DD
// in reverse-sorted order and stop once we've collected enough recent files.
func recentRollouts(root string, limit int) ([]string, error) {
	years, err := readDirSortedDesc(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, y := range years {
		months, err := readDirSortedDesc(filepath.Join(root, y))
		if err != nil {
			continue
		}
		for _, m := range months {
			days, err := readDirSortedDesc(filepath.Join(root, y, m))
			if err != nil {
				continue
			}
			for _, d := range days {
				dir := filepath.Join(root, y, m, d)
				files, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				type entry struct {
					path string
					mod  time.Time
				}
				var es []entry
				for _, fe := range files {
					if fe.IsDir() || filepath.Ext(fe.Name()) != ".jsonl" {
						continue
					}
					info, err := fe.Info()
					if err != nil {
						continue
					}
					es = append(es, entry{path: filepath.Join(dir, fe.Name()), mod: info.ModTime()})
				}
				sort.Slice(es, func(i, j int) bool { return es[i].mod.After(es[j].mod) })
				for _, e := range es {
					out = append(out, e.path)
					if len(out) >= limit {
						return out, nil
					}
				}
			}
		}
	}
	return out, nil
}

func readDirSortedDesc(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
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
