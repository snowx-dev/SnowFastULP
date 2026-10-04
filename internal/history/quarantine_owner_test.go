package history_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// writeOwnerMetadata writes quarantine owner metadata in the documented
// on-disk format, pinning the .owner.json contract from the tests.
func writeOwnerJSON(t *testing.T, container, owner string, heartbeat time.Time) {
	t.Helper()
	if owner == "" {
		owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	metadata := `{"version":1,"owner":"` + owner + `","pid":4242,` +
		`"started_at":"` + heartbeat.Add(-time.Minute).Format(time.RFC3339Nano) + `",` +
		`"heartbeat_at":"` + heartbeat.Format(time.RFC3339Nano) + `",` +
		`"original_basename":"orphan.txt"}`
	if err := os.WriteFile(filepath.Join(container, ".owner.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The core RR-4.5 scenario: run B's sweep must never treat run A's actively
// staged container as stale. A is paused right after staging (the afterStage
// seam) while run B deletes another file in the same parent; B's sweep covers
// A's container, which must survive untouched so A can finish.
func TestDeleteStagedConcurrentSweepSparesActiveContainer(t *testing.T) {
	parent := t.TempDir()
	aPath := filepath.Join(parent, "a.txt")
	bPath := filepath.Join(parent, "b.txt")
	for path, content := range map[string]string{aPath: "a", bPath: "b"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aCandidate, err := history.FingerprintFile(context.Background(), aPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	bCandidate, err := history.FingerprintFile(context.Background(), bPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	var bErr error
	var aPayload string
	hook := func(staged []history.StagedPath) error {
		aPayload = staged[0].Payload
		// Run B deletes a different source in the same parent; its sweep
		// sees A's just-staged container.
		_, bErr = history.DeleteStaged(context.Background(), []string{bPath}, []history.Candidate{bCandidate}, nil)
		if _, statErr := os.Stat(aPayload); statErr != nil {
			t.Errorf("run A's staged payload did not survive run B's sweep: %v", statErr)
		}
		return nil
	}
	removed, err := history.DeleteStaged(context.Background(), []string{aPath}, []history.Candidate{aCandidate}, hook)
	if err != nil {
		t.Fatalf("run A failed after a concurrent sweep: %v", err)
	}
	if bErr != nil {
		t.Fatalf("run B failed: %v", bErr)
	}
	if len(removed) != 1 || removed[0] != aPath {
		t.Fatalf("run A removed = %v, want [%s]", removed, aPath)
	}
}

// An owned container whose lease expired must be recovered: the payload is
// restored and the metadata is removed only afterwards, with the container.
func TestDeleteStagedRecoversExpiredOwnedContainer(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "orphan.txt"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOwnerJSON(t, container, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Now().Add(-time.Hour))
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	restored, readErr := os.ReadFile(filepath.Join(parent, "orphan.txt"))
	if readErr != nil {
		t.Fatalf("expired owned container payload was not restored: %v", readErr)
	}
	if string(restored) != "staged" {
		t.Fatalf("restored content = %q, want %q", restored, "staged")
	}
	if _, statErr := os.Lstat(container); !os.IsNotExist(statErr) {
		t.Fatalf("expired owned container was not removed: %v", statErr)
	}
}

// A container whose owner heartbeat is young belongs to a live run: the sweep
// must leave it and its payload completely alone.
func TestSweepSparesContainerWithFreshHeartbeat(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "orphan.txt"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOwnerJSON(t, container, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Now())
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if got, readErr := os.ReadFile(filepath.Join(container, "orphan.txt")); readErr != nil || string(got) != "staged" {
		t.Fatalf("fresh-lease container payload was touched: %q %v", got, readErr)
	}
	if _, err := os.Stat(filepath.Join(parent, "orphan.txt")); !os.IsNotExist(err) {
		t.Fatal("payload was renamed out of a live container")
	}
}

// Corrupt or foreign owner metadata must fail the sweep closed: never touch a
// container whose ownership cannot be proven.
func TestSweepLeavesContainerWithCorruptMetadata(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "orphan.txt"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, ".owner.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(container, "orphan.txt")); statErr != nil {
		t.Fatalf("container with unparseable owner metadata was touched: %v", statErr)
	}
}

// A container whose lease expired but whose owner token was rewritten between
// the sweeper's first read and its re-read was re-adopted by a live run: the
// sweep must take no action.
func TestSweepSkipsExpiredContainerWhenOwnerTokenRewritten(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "orphan.txt"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOwnerJSON(t, container, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Now().Add(-time.Hour))
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	history.SetSweepOwnerRecheckHook(func() {
		// The owner run re-adopted the container with a fresh token while the
		// sweeper was between its two reads.
		writeOwnerJSON(t, container, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", time.Now())
	})
	defer history.SetSweepOwnerRecheckHook(nil)

	if _, err := history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(container, "orphan.txt")); statErr != nil {
		t.Fatalf("container whose owner token changed was acted on: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(parent, "orphan.txt")); !os.IsNotExist(statErr) {
		t.Fatal("payload was restored despite a rewritten owner token")
	}
}

// A legacy container (no owner metadata) whose directory mtime is younger than
// the lease may belong to a pre-upgrade live run: leave it alone.
func TestSweepSparesFreshLegacyContainer(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "orphan.txt"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(container, "orphan.txt")); statErr != nil {
		t.Fatalf("fresh legacy container was swept before its lease expired: %v", statErr)
	}
}

// Replacing the owner token after staging must fail the run closed: the
// container is no longer provably ours, so nothing is validated or removed and
// the payload stays preserved inside the container.
func TestDeleteStagedFailsWhenOwnerTokenReplacedAfterStaging(t *testing.T) {
	parent := t.TempDir()
	aPath := filepath.Join(parent, "a.txt")
	if err := os.WriteFile(aPath, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), aPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	var payload string
	hook := func(staged []history.StagedPath) error {
		payload = staged[0].Payload
		// An impostor rewrote the ownership metadata with its own token.
		writeOwnerJSON(t, staged[0].Container, "ffffffffffffffffffffffffffffffff", time.Now())
		return nil
	}
	if _, err := history.DeleteStaged(context.Background(), []string{aPath}, []history.Candidate{candidate}, hook); err == nil {
		t.Fatal("DeleteStaged accepted a container whose owner token was replaced")
	}
	if payload != "" {
		if _, statErr := os.Stat(payload); statErr != nil {
			t.Fatalf("staged payload was removed despite ownership failure: %v", statErr)
		}
	}
}

// A matching owner token is not enough at the sweeper's re-read point: if the
// heartbeat was refreshed between the sweeper's two reads, the owner run came
// back from a pause longer than the lease and the container must be left
// completely alone.
func TestSweepSparesContainerRefreshedBetweenOwnerReads(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "orphan.txt"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The owner was paused long enough for its lease to expire, but its token
	// still matches what the sweeper reads first.
	writeOwnerJSON(t, container, "", time.Now().Add(-time.Hour))
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	history.SetSweepOwnerRecheckHook(func() {
		// The resumed owner refreshed its heartbeat with the same token
		// between the sweeper's first read and its re-read.
		writeOwnerJSON(t, container, "", time.Now())
	})
	defer history.SetSweepOwnerRecheckHook(nil)

	if _, err := history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if got, readErr := os.ReadFile(filepath.Join(container, "orphan.txt")); readErr != nil || string(got) != "staged" {
		t.Fatalf("container with a re-freshened heartbeat was acted on: %q %v", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(parent, "orphan.txt")); !os.IsNotExist(statErr) {
		t.Fatal("payload was restored despite a refreshed heartbeat")
	}
}

// A single validation pass over a huge tree can run far longer than the lease:
// the owner must keep refreshing its heartbeat during the pass, so a
// concurrent sweep keeps seeing the run as alive. The refresh hook fires
// mid-validation and records the heartbeat it just wrote; the staged payload
// is tampered with in place (same size) and a late arrival occupies the
// original path so validation fails deterministically and the container
// survives for inspection.
func TestLongValidationKeepsOwnerHeartbeatAlive(t *testing.T) {
	defer history.SetHeartbeatInterval(time.Millisecond)()
	// The sampled fingerprint reads only the head and tail 1 MiB, so a real
	// 24 MiB validation is now faster than any heartbeat tick; inject read
	// latency to restore the "validation outlives the heartbeat interval"
	// premise this test exists for.
	defer history.SetFingerprintReadPause(5 * time.Millisecond)()
	parent := t.TempDir()
	path := filepath.Join(parent, "big.txt")
	data := bytes.Repeat([]byte("x"), 24<<20)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}

	var container string
	var heartbeatAtStage int64
	var refreshedDuringValidation atomic.Bool
	var heartbeatMidValidation atomic.Int64
	history.SetHeartbeatRefreshHook(func() {
		refreshedDuringValidation.Store(true)
		if stamp, err := readOwnerHeartbeat(t, container); err == nil {
			heartbeatMidValidation.Store(stamp.UnixNano())
		}
	})
	defer history.SetHeartbeatRefreshHook(nil)

	hook := func(staged []history.StagedPath) error {
		container = staged[0].Container
		metadata, err := readOwnerHeartbeat(t, container)
		if err != nil {
			return err
		}
		heartbeatAtStage = metadata.UnixNano()
		// Same-size rewrite so validation re-hashes the payload and then
		// fails on the content change instead of an early size or mtime
		// mismatch.
		payloadInfo, err := os.Lstat(staged[0].Payload)
		if err != nil {
			return err
		}
		payload, err := os.ReadFile(staged[0].Payload)
		if err != nil {
			return err
		}
		payload[0] ^= 0xff
		if err := os.WriteFile(staged[0].Payload, payload, 0o600); err != nil {
			return err
		}
		if err := os.Chtimes(staged[0].Payload, payloadInfo.ModTime(), payloadInfo.ModTime()); err != nil {
			return err
		}
		// A late arrival at the original path makes the eventual restore fail
		// no-replace, so the container and its metadata survive for
		// inspection after DeleteStaged reports the failure.
		return os.WriteFile(path, []byte("late arrival"), 0o600)
	}
	if _, err := history.DeleteStaged(context.Background(), []string{path}, []history.Candidate{candidate}, hook); err == nil {
		t.Fatal("DeleteStaged accepted tampered staged content")
	}
	if !refreshedDuringValidation.Load() {
		t.Fatal("heartbeat was never refreshed during the validation pass")
	}
	if mid := heartbeatMidValidation.Load(); mid <= heartbeatAtStage {
		t.Fatalf("mid-validation heartbeat %d is not newer than the staged heartbeat %d", mid, heartbeatAtStage)
	}
	if _, err := os.Stat(filepath.Join(container, ".owner.json")); err != nil {
		t.Fatalf("container did not survive the failed validation: %v", err)
	}
}

// readOwnerHeartbeat reads the pinned .owner.json contract and returns the
// recorded heartbeat timestamp.
func readOwnerHeartbeat(t *testing.T, container string) (time.Time, error) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(container, ".owner.json"))
	if err != nil {
		return time.Time{}, err
	}
	var metadata struct {
		HeartbeatAt time.Time `json:"heartbeat_at"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return time.Time{}, err
	}
	return metadata.HeartbeatAt, nil
}
