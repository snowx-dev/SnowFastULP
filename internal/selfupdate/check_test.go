package selfupdate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckerFreshCacheRevalidatedWhenAlreadyUpdated(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "cache.json")
	cachePathHook = func() (string, error) { return cacheFile, nil }
	t.Cleanup(func() { cachePathHook = nil })

	// Written while still on 0.1; user updated to 0.2 since.
	entry := cacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    "0.2",
		Newer:     true,
	}
	writeCacheFile(t, cacheFile, entry)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "should not be called", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	c := NewChecker("0.2", "sfu", false)
	c.hooks = &testHooks{releaseURL: srv.URL + "/releases/latest"}
	c.Start()

	if got := hits.Load(); got != 0 {
		t.Fatalf("network hits = %d, want 0", got)
	}
	if c.NoticeForSummary() != nil {
		t.Fatal("expected nil notice when cache is stale but binary is current")
	}
}

func TestCheckerFreshCacheSkipsNetwork(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "cache.json")
	cachePathHook = func() (string, error) { return cacheFile, nil }
	t.Cleanup(func() { cachePathHook = nil })

	entry := cacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    "0.2.0",
		Newer:     true,
	}
	writeCacheFile(t, cacheFile, entry)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "should not be called", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	c := NewChecker("0.1.0", "sfu", false)
	c.hooks = &testHooks{releaseURL: srv.URL + "/releases/latest"}
	c.Start()

	if got := hits.Load(); got != 0 {
		t.Fatalf("network hits = %d, want 0", got)
	}
	n := c.NoticeForSummary()
	if n == nil || n.Latest != "0.2.0" || n.Command != "sfu update" {
		t.Fatalf("notice = %#v, want latest 0.2.0", n)
	}
}

func TestCheckerStaleCacheRefetches(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "cache.json")
	cachePathHook = func() (string, error) { return cacheFile, nil }
	t.Cleanup(func() { cachePathHook = nil })

	entry := cacheEntry{
		CheckedAt: time.Now().UTC().Add(-25 * time.Hour),
		Latest:    "0.1.0",
		Newer:     false,
	}
	writeCacheFile(t, cacheFile, entry)

	srv, _ := startMockReleaseServer(t, "0.2.0", mustAssetSuffix(t), []byte("a"), []byte("b"))
	t.Cleanup(srv.Close)

	c := NewChecker("0.1.0", "sfs", false)
	c.hooks = &testHooks{releaseURL: srv.URL + "/releases/latest"}
	c.Start()

	n := c.NoticeForSummary()
	if n == nil || n.Latest != "0.2.0" || n.Command != "sfs update" {
		t.Fatalf("notice = %#v, want 0.2.0", n)
	}

	got, ok := readFreshCache()
	if !ok || !got.Newer || got.Latest != "0.2.0" {
		t.Fatalf("refreshed cache = %#v ok=%v", got, ok)
	}
}

func TestCheckerFailedCheckWritesCacheAndSuppressesRetry(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "cache.json")
	cachePathHook = func() (string, error) { return cacheFile, nil }
	t.Cleanup(func() { cachePathHook = nil })

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewChecker("0.1.0", "sfu", false)
	c.hooks = &testHooks{releaseURL: srv.URL + "/releases/latest"}
	c.Start()
	if c.NoticeForSummary() != nil {
		t.Fatal("expected nil notice on failed check")
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}

	got, ok := readFreshCache()
	if !ok || got.Newer || got.Latest != "" {
		t.Fatalf("cache after failure = %#v ok=%v", got, ok)
	}

	c2 := NewChecker("0.1.0", "sfu", false)
	c2.hooks = c.hooks
	c2.Start()
	if c2.NoticeForSummary() != nil {
		t.Fatal("expected nil notice from cached failure")
	}
	if hits.Load() != 1 {
		t.Fatalf("hits after cached failure = %d, want 1", hits.Load())
	}
}

func TestCheckerUpToDateWritesCacheWithoutNotice(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "cache.json")
	cachePathHook = func() (string, error) { return cacheFile, nil }
	t.Cleanup(func() { cachePathHook = nil })

	srv, _ := startMockReleaseServer(t, "0.1.1", mustAssetSuffix(t), []byte("a"), []byte("b"))
	t.Cleanup(srv.Close)

	c := NewChecker("0.1.1", "sfu", false)
	c.hooks = &testHooks{releaseURL: srv.URL + "/releases/latest"}
	c.Start()
	if c.NoticeForSummary() != nil {
		t.Fatal("expected nil notice when up to date")
	}

	got, ok := readFreshCache()
	if !ok || got.Newer || got.Latest != "0.1.1" {
		t.Fatalf("cache = %#v ok=%v", got, ok)
	}
}

func TestCheckerDevBuildSeesReleaseAsNewer(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "cache.json")
	cachePathHook = func() (string, error) { return cacheFile, nil }
	t.Cleanup(func() { cachePathHook = nil })

	srv, _ := startMockReleaseServer(t, "0.1.1", mustAssetSuffix(t), []byte("a"), []byte("b"))
	t.Cleanup(srv.Close)

	c := NewChecker("0.1.1-dev", "sfu", false)
	c.hooks = &testHooks{releaseURL: srv.URL + "/releases/latest"}
	c.Start()
	n := c.NoticeForSummary()
	if n == nil || n.Latest != "0.1.1" {
		t.Fatalf("notice = %#v, want 0.1.1 for dev build", n)
	}
}

func TestCheckerDisabledSkipsCacheAndNetwork(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "cache.json")
	cachePathHook = func() (string, error) { return cacheFile, nil }
	t.Cleanup(func() { cachePathHook = nil })

	entry := cacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    "9.9.9",
		Newer:     true,
	}
	writeCacheFile(t, cacheFile, entry)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	c := NewChecker("0.1.0", "sfu", true)
	c.hooks = &testHooks{releaseURL: srv.URL + "/releases/latest"}
	c.Start()
	if c.NoticeForSummary() != nil {
		t.Fatal("disabled checker must not surface notice")
	}
	if hits.Load() != 0 {
		t.Fatalf("hits = %d, want 0", hits.Load())
	}
}

func writeCacheFile(t *testing.T, path string, entry cacheEntry) {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustAssetSuffix(t *testing.T) string {
	t.Helper()
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	return suffix
}
