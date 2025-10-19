package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const defaultCache = 3600 // 1 h

var (
	dataRoot     string
	resticBinary string
	cacheSeconds int

	cacheMu    sync.RWMutex
	cachedAt   time.Time
	cachedData []ProfileStats

	computeMu   sync.Mutex
	computing   bool
	computeCond = sync.NewCond(&computeMu)
)

/* ─── JSON models ─────────────────────────────────────────────────────────── */

type restoreJSON struct {
	TotalSize      int64 `json:"total_size"`
	TotalFileCount int64 `json:"total_file_count"`
	SnapshotsCount int64 `json:"snapshots_count"`
}

type rawJSON struct {
	TotalSize            int64   `json:"total_size"`
	TotalUncompressed    int64   `json:"total_uncompressed_size"`
	CompressionRatio     float64 `json:"compression_ratio"`
	CompressionProgress  int64   `json:"compression_progress"`
	CompressionSavingPct float64 `json:"compression_space_saving"`
	TotalBlobCount       int64   `json:"total_blob_count"`
	SnapshotsCount       int64   `json:"snapshots_count"`
}

type snapshotEntry struct {
	Time  string   `json:"time"`  // RFC 3339
	Paths []string `json:"paths"` // list of source paths
}

/* ─── API model ───────────────────────────────────────────────────────────── */

type PathSnapshot struct {
	Path         string `json:"path"`
	LastSnapshot string `json:"last_snapshot"` // human readable
}

type ProfileStats struct {
	// Identification
	Name string `json:"name"`

	// Restore‑size
	RestoreBytes int64  `json:"restore_bytes"`
	RestoreHuman string `json:"restore_human"`
	RestoreFiles int64  `json:"restore_files"`

	// Raw‑data
	RawBytes               int64   `json:"raw_bytes"`
	RawHuman               string  `json:"raw_human"`
	UncompBytes            int64   `json:"uncompressed_bytes"`
	UncompHuman            string  `json:"uncompressed_human"`
	CompressRatio          float64 `json:"compression_ratio"`
	CompressRatioHuman     string  `json:"compression_ratio_human"`
	CompressionSavingPc    float64 `json:"compression_space_saving"`
	CompressionSavingHuman string  `json:"compression_space_saving_human"`
	CompressionProgPct     int64   `json:"compression_progress"`
	RawBlobs               int64   `json:"raw_blob_count"`

	// Snapshot info
	LastSnapshot string         `json:"last_snapshot"`
	Paths        []PathSnapshot `json:"paths"`

	// Common
	Snapshots int64 `json:"snapshots"`
}

/* ─── init ────────────────────────────────────────────────────────────────── */

func init() {
	dataRoot = getenvOr("DATA_ROOT", "/data")
	resticBinary = getenvOr("RESTICPROFILE_BINARY", "/usr/local/bin/resticprofile")
	cacheSeconds = getCacheSeconds()
}

/* ─── main ────────────────────────────────────────────────────────────────── */

func main() {
	fmt.Printf("Data root: %s", dataRoot)
	fmt.Printf("Resticprofile binary: %s", resticBinary)
	fmt.Printf("Cache TTL: %ds", cacheSeconds)

	http.HandleFunc("/stats", statsHandler)

	fmt.Println("Listening on :8080 🚀")
	fmt.Println(http.ListenAndServe(":8080", nil))
}

/* ─── HTTP handler & caching ──────────────────────────────────────────────── */

func statsHandler(w http.ResponseWriter, r *http.Request) {
	res, err := getStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func getStats() ([]ProfileStats, error) {
	// quick cache check
	cacheMu.RLock()
	fmt.Println("Cache hit, checking if still valid", time.Since(cachedAt), "since last update", time.Duration(cacheSeconds)*time.Second, "cache seconds")
	if time.Since(cachedAt) < time.Duration(cacheSeconds)*time.Second && cachedData != nil {
		defer cacheMu.RUnlock()
		return cachedData, nil
	}
	cacheMu.RUnlock()

	// ensure only one generator runs
	computeMu.Lock()
	for computing {
		computeCond.Wait()
	}
	// maybe someone else refreshed while we waited
	cacheMu.RLock()
	fmt.Println("Cache hit 2, checking if still valid", time.Since(cachedAt), "since last update", time.Duration(cacheSeconds)*time.Second, "cache seconds")
	if time.Since(cachedAt) < time.Duration(cacheSeconds)*time.Second && cachedData != nil {
		cacheMu.RUnlock()
		computeMu.Unlock()
		return cachedData, nil
	}
	cacheMu.RUnlock()

	computing = true
	computeMu.Unlock()

	stats, err := generateStats()

	cacheMu.Lock()
	if err != nil {
		fmt.Printf("DEBUG: generateStats() returned an error: %v. CACHE WILL NOT BE UPDATED.", err)
		fmt.Printf("Error generating stats: %v\n", err)
	} else {
		fmt.Println("DEBUG: generateStats() succeeded (err is nil). PROCEEDING TO UPDATE CACHE.")
		cachedData = stats
		originalCachedAt := cachedAt
		cachedAt = time.Now()
		fmt.Printf("DEBUG: CACHE UPDATED. Old cachedAt for this goroutine: %s, New cachedAt: %s. Time since new update: %s", originalCachedAt.Format(time.RFC3339Nano), cachedAt.Format(time.RFC3339Nano), time.Since(cachedAt))
	}
	cacheMu.Unlock()

	computeMu.Lock()
	computing = false
	computeCond.Broadcast()
	computeMu.Unlock()

	return stats, err
}

/* ─── stats generation ────────────────────────────────────────────────────── */

func generateStats() ([]ProfileStats, error) {
	entries, err := os.ReadDir(dataRoot)
	if err != nil {
		return nil, err
	}
	var stats []ProfileStats
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dirPath := filepath.Join(dataRoot, name)

		// restore‑size
		var restore restoreJSON
		if err := runAndParse(dirPath, "stats", "restore-size", &restore); err != nil {
			fmt.Printf("restore-size for %s: %v", dirPath, err)
			continue
		}

		// raw‑data
		var raw rawJSON
		if err := runAndParse(dirPath, "stats", "raw-data", &raw); err != nil {
			fmt.Printf("raw-data for %s: %v", dirPath, err)
			continue
		}

		// snapshots
		var snaps []snapshotEntry
		if err := runAndParse(dirPath, "snapshots", "", &snaps); err != nil {
			fmt.Printf("snapshots for %s: %v", dirPath, err)
			continue
		}
		lastSnap, pathInfo := summariseSnapshots(snaps)

		stats = append(stats, ProfileStats{
			Name:                   name,
			RestoreBytes:           restore.TotalSize,
			RestoreHuman:           human(bytes(float64(restore.TotalSize))),
			RestoreFiles:           restore.TotalFileCount,
			RawBytes:               raw.TotalSize,
			RawHuman:               human(bytes(float64(raw.TotalSize))),
			UncompBytes:            raw.TotalUncompressed,
			UncompHuman:            human(bytes(float64(raw.TotalUncompressed))),
			CompressRatio:          raw.CompressionRatio,
			CompressRatioHuman:     fmt.Sprintf("%.2f", raw.CompressionRatio),
			CompressionSavingPc:    raw.CompressionSavingPct,
			CompressionSavingHuman: fmt.Sprintf("%.2f%%", raw.CompressionSavingPct),
			CompressionProgPct:     raw.CompressionProgress,
			RawBlobs:               raw.TotalBlobCount,

			LastSnapshot: lastSnap,
			Paths:        pathInfo,

			Snapshots: restore.SnapshotsCount,
		})
	}
	return stats, nil
}

/* ─── helpers ─────────────────────────────────────────────────────────────── */

// runAndParse executes `resticprofile <cmd> [--mode X] --json`, streams logs,
// and unmarshals the first JSON object (or array) into v.
func runAndParse(dir, cmdName, mode string, v interface{}) error {
	args := []string{cmdName}
	if mode != "" {
		args = append(args, "--mode", mode)
	}
	args = append(args, "--json")

	args = append(args, "--no-lock") // avoid setting locks during stats

	cmd := exec.Command(resticBinary, args...)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Bytes()
		os.Stdout.Write(line)
		os.Stdout.Write([]byte{'\n'})
		if len(line) > 0 && line[0] == '{' || (len(line) > 0 && line[0] == '[') {
			if err := json.Unmarshal(line, v); err != nil {
				return fmt.Errorf("decode %s JSON: %w", cmdName, err)
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return cmd.Wait()
}

/* human‑friendly byte formatter */
type bytes float64

func human(b bytes) string {
	const unit = 1024.0
	if b < unit {
		return fmt.Sprintf("%d B", int64(b))
	}
	exp := int(math.Log(float64(b)) / math.Log(unit))
	pre := "KMGTPE"[exp-1]
	val := float64(b) / math.Pow(unit, float64(exp))
	return fmt.Sprintf("%.2f %ciB", val, pre)
}

/* human‑friendly time formatter */
func prettyTime(t time.Time) string {
	diff := time.Since(t)
	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		return fmt.Sprintf("%d min ago", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%.1f h ago", diff.Hours())
	default:
		return t.Format("2006‑01‑02 15:04")
	}
}

/* summariseSnapshots picks latest snapshot and per‑path latest times */
func summariseSnapshots(snaps []snapshotEntry) (string, []PathSnapshot) {
	var latest time.Time
	pathMap := map[string]time.Time{}
	for _, s := range snaps {
		t, err := time.Parse(time.RFC3339, s.Time)
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
		for _, p := range s.Paths {
			if t.After(pathMap[p]) {
				pathMap[p] = t
			}
		}
	}
	paths := make([]PathSnapshot, 0, len(pathMap))
	for p, t := range pathMap {
		paths = append(paths, PathSnapshot{Path: p, LastSnapshot: prettyTime(t)})
	}
	return prettyTime(latest), paths
}

/* env helpers */
func getenvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getCacheSeconds() int {
	if v := os.Getenv("CACHE_SECONDS"); v != "" {
		if s, err := strconv.Atoi(v); err == nil && s > 0 {
			return s
		}
	}
	return defaultCache
}
