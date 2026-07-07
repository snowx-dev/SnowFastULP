package selfupdate

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/atomicfs"
)

const (
	checkCacheTTL      = 24 * time.Hour
	checkSummaryWait   = 2 * time.Second
	checkCacheFileName = "snowfast-update-check.json"
)

// cachePathHook, when set, overrides the default temp-dir cache path (tests).
var cachePathHook func() (string, error)

func checkCachePath() (string, error) {
	if cachePathHook != nil {
		return cachePathHook()
	}
	return filepath.Join(os.TempDir(), checkCacheFileName), nil
}

type cacheEntry struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
	Newer     bool      `json:"newer"`
}

// Notice describes an available release for summary footers.
type Notice struct {
	Latest  string
	Command string
}

// Checker performs a background update-availability peek with a 24h cache.
type Checker struct {
	currentVersion string
	command        string
	disabled       bool
	hooks          *testHooks

	mu      sync.Mutex
	notice  *Notice
	done    chan struct{}
	started bool
}

// NewChecker builds a checker. binName is the executable stem (sfu/sfs).
func NewChecker(currentVersion, binName string, disabled bool) *Checker {
	binName = strings.TrimSuffix(strings.ToLower(filepath.Base(binName)), ".exe")
	return &Checker{
		currentVersion: currentVersion,
		command:        binName + " update",
		disabled:       disabled,
	}
}

// Start kicks off a cache lookup or background check. Safe to call once per run.
func (c *Checker) Start() {
	if c.disabled {
		return
	}
	if _, err := assetSuffix(); err != nil {
		return
	}

	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.mu.Unlock()

	if entry, ok := readFreshCache(); ok {
		c.applyEntry(entry)
		return
	}

	done := make(chan struct{})
	c.mu.Lock()
	c.done = done
	c.mu.Unlock()

	go func() {
		defer close(done)
		entry := c.performCheck()
		writeCheckCache(entry)
		c.applyEntry(entry)
	}()
}

// NoticeForSummary returns a nudge when a newer release exists, waiting briefly
// for an in-flight check. Nil on disable, up-to-date, errors, or slow checks.
func (c *Checker) NoticeForSummary() *Notice {
	if c.disabled {
		return nil
	}
	c.waitIfRunning()

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notice
}

func (c *Checker) waitIfRunning() {
	c.mu.Lock()
	ch := c.done
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(checkSummaryWait):
	}
}

func (c *Checker) applyEntry(entry cacheEntry) {
	if !entry.Newer || entry.Latest == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notice = &Notice{
		Latest:  entry.Latest,
		Command: c.command,
	}
}

func (c *Checker) performCheck() cacheEntry {
	entry := cacheEntry{CheckedAt: time.Now().UTC()}

	manifest, err := fetchLatest(c.hooks)
	if err != nil {
		return entry
	}
	latest := strings.TrimPrefix(manifest.Version, "v")
	if latest == "" {
		return entry
	}
	entry.Latest = latest
	cur := strings.TrimPrefix(c.currentVersion, "v")
	entry.Newer = compareVersions(latest, cur) > 0
	return entry
}

func readFreshCache() (cacheEntry, bool) {
	path, err := checkCachePath()
	if err != nil {
		return cacheEntry{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cacheEntry{}, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return cacheEntry{}, false
	}
	if entry.CheckedAt.IsZero() {
		return cacheEntry{}, false
	}
	if time.Since(entry.CheckedAt) > checkCacheTTL {
		return cacheEntry{}, false
	}
	return entry, true
}

func writeCheckCache(entry cacheEntry) {
	path, err := checkCachePath()
	if err != nil {
		log.Printf("selfupdate: check cache path: %v", err)
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		log.Printf("selfupdate: encode check cache: %v", err)
		return
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		log.Printf("selfupdate: create check cache temp: %v", err)
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		log.Printf("selfupdate: write check cache: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("selfupdate: close check cache: %v", err)
		return
	}
	if err := atomicfs.Rename(tmpPath, path); err != nil {
		log.Printf("selfupdate: rename check cache: %v", err)
	}
}
