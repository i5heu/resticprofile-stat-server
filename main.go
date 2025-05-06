package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const defaultCache = 3600 // 1 hour default cache

var (
	dataRoot     string
	resticBinary string
	cacheSeconds int

	cacheMu    sync.RWMutex // protects cachedData & cachedAt
	cachedAt   time.Time
	cachedData []ProfileStats

	computeMu   sync.Mutex // serialises expensive refresh
	computing   bool       // true while a refresh is running
	computeCond = sync.NewCond(&computeMu)
)

// Raw JSON from resticprofile
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

// API response
type ProfileStats struct {
	Name string `json:"name"`

	// Restore-size stats
	RestoreBytes int64  `json:"restore_bytes"`
	RestoreHuman string `json:"restore_human"`
	RestoreFiles int64  `json:"restore_files"`

	// Raw-data stats
	RawBytes            int64   `json:"raw_bytes"`
	RawHuman            string  `json:"raw_human"`
	UncompBytes         int64   `json:"uncompressed_bytes"`
	UncompHuman         string  `json:"uncompressed_human"`
	CompressRatio       float64 `json:"compression_ratio"`
	CompressionSavingPc float64 `json:"compression_space_saving"`
	CompressionProgPct  int64   `json:"compression_progress"`
	RawBlobs            int64   `json:"raw_blob_count"`

	// Common
	Snapshots int64 `json:"snapshots"`
}

func init() {
	dataRoot = getenvOr("DATA_ROOT", "/data")
	resticBinary = getenvOr("RESTICPROFILE_BINARY", "/resticprofile")
	cacheSeconds = getCacheSeconds()
}

func main() {
	log.Printf("Data root: %s", dataRoot)
	log.Printf("Resticprofile binary: %s", resticBinary)
	log.Printf("Cache TTL: %ds", cacheSeconds)

	http.HandleFunc("/stats", statsHandler)

	log.Println("Listening on :8080 ...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("http server: %v", err)
	}
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	res, err := getStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// getStats ensures only one concurrent refresh and returns cachedData if fresh
func getStats() ([]ProfileStats, error) {
	cacheMu.RLock()
	if time.Since(cachedAt) < time.Duration(cacheSeconds)*time.Second && cachedData != nil {
		defer cacheMu.RUnlock()
		return cachedData, nil
	}
	cacheMu.RUnlock()

	computeMu.Lock()
	for computing {
		computeCond.Wait()
	}
	cacheMu.RLock()
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
	if err == nil {
		cachedData = stats
		cachedAt = time.Now()
	}
	cacheMu.Unlock()

	computeMu.Lock()
	computing = false
	computeCond.Broadcast()
	computeMu.Unlock()

	return stats, err
}

// generateStats walks dataRoot and runs restic stats for each profile
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

		var restore restoreJSON
		if err := runAndParse(dirPath, "restore-size", &restore); err != nil {
			log.Printf("restore-size for %s: %v", dirPath, err)
			continue
		}

		var raw rawJSON
		if err := runAndParse(dirPath, "raw-data", &raw); err != nil {
			log.Printf("raw-data for %s: %v", dirPath, err)
			continue
		}

		stats = append(stats, ProfileStats{
			Name:                name,
			RestoreBytes:        restore.TotalSize,
			RestoreHuman:        human(bytes(float64(restore.TotalSize))),
			RestoreFiles:        restore.TotalFileCount,
			RawBytes:            raw.TotalSize,
			RawHuman:            human(bytes(float64(raw.TotalSize))),
			UncompBytes:         raw.TotalUncompressed,
			UncompHuman:         human(bytes(float64(raw.TotalUncompressed))),
			CompressRatio:       raw.CompressionRatio,
			CompressionSavingPc: raw.CompressionSavingPct,
			CompressionProgPct:  raw.CompressionProgress,
			RawBlobs:            raw.TotalBlobCount,
			Snapshots:           restore.SnapshotsCount,
		})
	}
	return stats, nil
}

// runAndParse executes resticprofile in dir, streams logs, and parses first JSON line into v
func runAndParse(dir, mode string, v interface{}) error {
	cmd := exec.Command(resticBinary, "stats", "--mode", mode, "--json")
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
		if len(line) > 0 && line[0] == '{' {
			if err := json.Unmarshal(line, v); err != nil {
				return fmt.Errorf("decode %s JSON: %w", mode, err)
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return cmd.Wait()
}

// human converts bytes to IEC units (KiB, MiB, GiB, TiB, …)
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

type bytes float64

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
