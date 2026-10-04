package history

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// staleContainerNameRegexp matches quarantine container names as this package
// creates them: a fixed prefix plus 128 bits of lowercase hex.
var staleContainerNameRegexp = regexp.MustCompile(`^\.snowfast-quarantine-[0-9a-f]{32}$`)

// sweepOwnerRecheckHook is a test seam, never set in production: it runs
// between the sweeper's first owner read and its immediate pre-action re-read.
var sweepOwnerRecheckHook func()

// heartbeatRefreshHook is a test seam, never set in production: it runs after
// each successful periodic lease refresh during the validation phase.
var heartbeatRefreshHook func()

// quarantineLease is the conservative window a quarantine container's owner
// gets between heartbeats before another run may treat the container as
// crashed. A live -del run refreshes the heartbeat before validation and
// removal, so only a run paused (or crashed) longer than the lease is
// recoverable by a concurrent sweep.
const quarantineLease = 30 * time.Minute

// heartbeatInterval is how often a live -del run refreshes its lease while a
// single validation pass is in flight. A huge tree can re-fingerprint far
// longer than the lease, so the refresh runs on a ticker instead of waiting
// for the next pass boundary. A third of the lease gives the sweeper at least
// two refresh opportunities before the lease would expire. Var so tests can
// shorten the cadence via SetHeartbeatInterval.
var heartbeatInterval = quarantineLease / 3

// ownerFileName holds the ownership metadata inside every quarantine
// container, created before the payload is staged. It is ownership state, not
// payload, and is excluded from payload counting and recovery decisions.
const ownerFileName = ".owner.json"

// ownerMetadataVersion is the schema version of the owner metadata document.
const ownerMetadataVersion = 1

// quarantineOwner is the on-disk ownership document stored beside the staged
// payload: who owns the container, when it was created, and when the owner run
// last proved it is still alive.
type quarantineOwner struct {
	Version          int       `json:"version"`
	Owner            string    `json:"owner"`
	PID              int       `json:"pid"`
	StartedAt        time.Time `json:"started_at"`
	HeartbeatAt      time.Time `json:"heartbeat_at"`
	OriginalBasename string    `json:"original_basename"`
}

// StagedPath identifies an original deletion root, the private same-parent
// container created for it, and the payload path inside that container. The
// payload keeps the original base name, so a container left behind by a
// crashed process is self-describing: a later sweep can rename the payload
// back without any side-channel state. Owner is the random token persisted in
// the container's .owner.json; validation and removal require it to still
// match, so a replaced or swept-away container can never be mistaken for ours.
type StagedPath struct {
	Original  string
	Container string
	Payload   string
	Owner     string

	containerIdentity os.FileInfo
}

// DeleteStaged atomically renames each root into a same-filesystem private
// container, revalidates every candidate from the staged payloads, then removes
// only those payloads and their verified containers. afterStage is a
// deterministic test seam and is nil in normal operation.
//
// Before staging, stale containers left in the target parents by crashed runs
// are swept on a best-effort basis: an empty container is removed, and a
// container holding exactly one regular-file or directory entry has that entry
// renamed back to its original name (no-replace, so a late arrival at the
// original path is never clobbered). Anything the sweep cannot interpret —
// symlinked containers, symlink or special-file entries, unexpected contents —
// is left untouched. The sweep only ever renames payload data back toward its
// original location, never deletes it, and its failures never block the
// current deletion.
//
// The random 128-bit name and mode 0700 prevent an untrusted writer that cannot
// enumerate or access this process's private container from pre-creating or
// populating it. Identity checks fail closed if the container path is exchanged
// before cleanup. A process running as the same principal with permission to
// enumerate and continuously race the parent directory is outside this
// pathname-based threat boundary; defending that actor requires recursive
// handle-relative deletion APIs that are not portable across supported targets.
func DeleteStaged(ctx context.Context, roots []string, candidates []Candidate, afterStage func([]StagedPath) error) ([]string, error) {
	sweepStaleParents(roots)
	staged, err := stageRoots(roots)
	if err != nil {
		return nil, err
	}
	restoreOnError := func(cause error) ([]string, error) {
		if restoreErr := restoreStaged(staged); restoreErr != nil {
			return nil, errors.Join(cause, restoreErr)
		}
		return nil, cause
	}
	if afterStage != nil {
		if err := afterStage(append([]StagedPath(nil), staged...)); err != nil {
			return restoreOnError(fmt.Errorf("history: after deletion staging: %w", err))
		}
	}
	// A single ValidateAt can re-fingerprint a huge tree for far longer than
	// the lease, so prove liveness on a ticker for the whole validation phase.
	// A failed heartbeat cancels the validation context: the run stops
	// promptly and aborts with a restore, exactly like the pre-pass refresh.
	validationCtx, cancelHeartbeats := context.WithCancel(ctx)
	defer cancelHeartbeats()
	heartbeatErrc := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-validationCtx.Done():
				heartbeatErrc <- nil
				return
			case <-ticker.C:
				if err := refreshOwnerHeartbeats(staged); err != nil {
					heartbeatErrc <- err
					cancelHeartbeats()
					return
				}
				if heartbeatRefreshHook != nil {
					heartbeatRefreshHook()
				}
			}
		}
	}()
	var validationErr error
	for _, candidate := range candidates {
		if err := refreshOwnerHeartbeats(staged); err != nil {
			validationErr = fmt.Errorf("history: refresh quarantine lease before validation: %w", err)
			break
		}
		mapped, err := mapCandidatePaths(candidate.Paths, staged)
		if err != nil {
			validationErr = err
			break
		}
		if err := ValidateAt(validationCtx, candidate, mapped, nil); err != nil {
			validationErr = fmt.Errorf("history: staged deletion validation failed: %w", err)
			break
		}
	}
	cancelHeartbeats()
	<-heartbeatDone
	if hbErr := <-heartbeatErrc; hbErr != nil {
		return restoreOnError(fmt.Errorf("history: refresh quarantine lease during validation: %w", hbErr))
	}
	if validationErr != nil {
		return restoreOnError(validationErr)
	}
	removed := make([]string, 0, len(staged))
	if err := refreshOwnerHeartbeats(staged); err != nil {
		// Not restored on purpose: if the owner token was replaced by another
		// run, the payload may already be gone from the container, and a
		// restore could rename the wrong payload. Fail closed instead.
		return nil, fmt.Errorf("history: refresh quarantine lease before removal: %w; staged sources are preserved in their quarantine containers and will be recovered by the stale-container sweeper after the lease expires (or can be restored manually)", err)
	}
	for _, item := range staged {
		if err := removeStaged(item); err != nil {
			return removed, fmt.Errorf("history: remove staged source %s (payload preserved at %s): %w", item.Original, item.Payload, err)
		}
		removed = append(removed, item.Original)
	}
	return removed, nil
}

func stageRoots(roots []string) ([]StagedPath, error) {
	cleaned := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("history: resolve deletion source %s: %w", root, err)
		}
		absolute = filepath.Clean(absolute)
		if _, ok := seen[absolute]; ok {
			continue
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, fmt.Errorf("history: stat deletion source %s: %w", absolute, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("history: refusing to stage symlink source %s", absolute)
		}
		seen[absolute] = struct{}{}
		cleaned = append(cleaned, absolute)
	}
	sort.Strings(cleaned)
	for i, root := range cleaned {
		for _, other := range cleaned[i+1:] {
			if isWithin(root, other) {
				return nil, fmt.Errorf("history: overlapping deletion roots %s and %s", root, other)
			}
		}
	}

	staged := make([]StagedPath, 0, len(cleaned))
	for _, original := range cleaned {
		item, err := createStagingContainer(original)
		if err != nil {
			_, restoreErr := restoreStagedResult(staged)
			return nil, errors.Join(err, restoreErr)
		}
		if err := os.Rename(original, item.Payload); err != nil {
			cleanupErr := removeEmptyContainer(item)
			_, restoreErr := restoreStagedResult(staged)
			return nil, errors.Join(fmt.Errorf("history: stage source %s: %w", original, err), cleanupErr, restoreErr)
		}
		staged = append(staged, item)
	}
	return staged, nil
}

func createStagingContainer(original string) (StagedPath, error) {
	parent := filepath.Dir(original)
	for range 100 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return StagedPath{}, fmt.Errorf("history: generate quarantine name for %s: %w", original, err)
		}
		container := filepath.Join(parent, ".snowfast-quarantine-"+hex.EncodeToString(random[:]))
		if err := os.Mkdir(container, 0o700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return StagedPath{}, fmt.Errorf("history: create quarantine container for %s: %w", original, err)
		}
		identity, err := os.Lstat(container)
		if err != nil {
			return StagedPath{}, fmt.Errorf("history: stat new quarantine container %s: %w", container, err)
		}
		// Ownership metadata exists before the payload is staged, so every
		// container this package creates is self-describing from birth.
		owner, err := writeOwnerMetadata(container, filepath.Base(original))
		if err != nil {
			os.Remove(container)
			return StagedPath{}, fmt.Errorf("history: write quarantine owner metadata for %s: %w", original, err)
		}
		return StagedPath{
			Original:          original,
			Container:         container,
			Payload:           filepath.Join(container, filepath.Base(original)),
			Owner:             owner,
			containerIdentity: identity,
		}, nil
	}
	return StagedPath{}, fmt.Errorf("history: could not allocate quarantine container for %s", original)
}

func validateContainer(item StagedPath) error {
	info, err := os.Lstat(item.Container)
	if err != nil {
		return fmt.Errorf("history: inspect quarantine container %s: %w", item.Container, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || item.containerIdentity == nil || !os.SameFile(item.containerIdentity, info) {
		return fmt.Errorf("history: quarantine container was replaced at %s; staged data not removed", item.Container)
	}
	metadata, err := readOwnerMetadata(item.Container)
	if err != nil {
		return fmt.Errorf("history: quarantine container owner metadata unavailable at %s; staged data not removed: %w", item.Container, err)
	}
	if item.Owner == "" || metadata.Owner != item.Owner {
		return fmt.Errorf("history: quarantine container ownership changed at %s; staged data not removed", item.Container)
	}
	return nil
}

func removeStaged(item StagedPath) error {
	if err := validateContainer(item); err != nil {
		return err
	}
	if err := os.RemoveAll(item.Payload); err != nil {
		return err
	}
	return removeEmptyContainer(item)
}

func removeEmptyContainer(item StagedPath) error {
	if err := validateContainer(item); err != nil {
		return err
	}
	// Owner metadata goes first so the rmdir below sees an empty directory;
	// it is removed only after the payload is already restored or removed.
	if err := os.Remove(filepath.Join(item.Container, ownerFileName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("history: remove quarantine owner metadata %s: %w", item.Container, err)
	}
	if err := os.Remove(item.Container); err != nil {
		return fmt.Errorf("history: remove quarantine container %s: %w", item.Container, err)
	}
	return nil
}

// writeOwnerMetadata creates the container's ownership document and returns
// the owner token. The write is atomic so a crash mid-write can never leave a
// half-written document that a sweeper would have to fail closed on forever.
func writeOwnerMetadata(container, originalBasename string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("history: generate quarantine owner token: %w", err)
	}
	owner := hex.EncodeToString(token[:])
	now := time.Now().UTC()
	if err := writeOwnerFile(filepath.Join(container, ownerFileName), quarantineOwner{
		Version:          ownerMetadataVersion,
		Owner:            owner,
		PID:              os.Getpid(),
		StartedAt:        now,
		HeartbeatAt:      now,
		OriginalBasename: originalBasename,
	}); err != nil {
		return "", err
	}
	return owner, nil
}

func writeOwnerFile(path string, metadata quarantineOwner) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("history: encode quarantine owner metadata: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ownerFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("history: stage quarantine owner metadata %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("history: write quarantine owner metadata %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("history: write quarantine owner metadata %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("history: publish quarantine owner metadata %s: %w", path, err)
	}
	return nil
}

// readOwnerMetadata parses the container's ownership document. Any failure —
// missing, corrupt, or foreign-schema metadata — is an error, and callers fail
// closed.
func readOwnerMetadata(container string) (*quarantineOwner, error) {
	data, err := os.ReadFile(filepath.Join(container, ownerFileName))
	if err != nil {
		return nil, fmt.Errorf("history: read quarantine owner metadata: %w", err)
	}
	var metadata quarantineOwner
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("history: parse quarantine owner metadata: %w", err)
	}
	if metadata.Version != ownerMetadataVersion || metadata.Owner == "" {
		return nil, fmt.Errorf("history: unsupported quarantine owner metadata in %s", container)
	}
	return &metadata, nil
}

// refreshOwnerHeartbeats re-proves the lease for every staged container.
func refreshOwnerHeartbeats(staged []StagedPath) error {
	var errs []error
	for _, item := range staged {
		if err := touchOwnerHeartbeat(item); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// touchOwnerHeartbeat rewrites one container's ownership document with a fresh
// heartbeat, refusing when the token no longer matches what this run staged.
func touchOwnerHeartbeat(item StagedPath) error {
	metadata, err := readOwnerMetadata(item.Container)
	if err != nil {
		return fmt.Errorf("history: refresh quarantine lease for %s: %w", item.Container, err)
	}
	if item.Owner == "" || metadata.Owner != item.Owner {
		return fmt.Errorf("history: quarantine container ownership changed at %s", item.Container)
	}
	metadata.HeartbeatAt = time.Now().UTC()
	if err := writeOwnerFile(filepath.Join(item.Container, ownerFileName), *metadata); err != nil {
		return fmt.Errorf("history: refresh quarantine lease for %s: %w", item.Container, err)
	}
	return nil
}

// sweepStaleParents sweeps the parent directory of each deletion root for
// quarantine containers left behind by crashed runs. Failures are ignored: the
// sweep is a janitor, and anything it cannot safely handle stays in place for
// the next run (or for manual inspection).
func sweepStaleParents(roots []string) {
	parents := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		parents[filepath.Dir(filepath.Clean(absolute))] = struct{}{}
	}
	for parent := range parents {
		sweepStaleContainers(parent)
	}
}

func sweepStaleContainers(parent string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !staleContainerNameRegexp.MatchString(entry.Name()) {
			continue
		}
		info, err := os.Lstat(filepath.Join(parent, entry.Name()))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		sweepStaleContainer(filepath.Join(parent, entry.Name()))
	}
}

// sweepStaleContainer recovers one crashed container. A container with owner
// metadata is only recoverable once its heartbeat is older than the lease, and
// only after a final re-read proves the owner token unchanged; the metadata is
// removed only after the payload is restored or the container is confirmed
// empty. A container without metadata is legacy: it is recoverable only when
// the directory mtime itself is older than the lease. An empty container is
// removed, and a container holding exactly one entry that is a regular file or
// directory has that entry renamed back to its original name in the parent
// (no-replace) before the container is removed. Symlink or special-file
// entries, and containers with unexpected contents, are left untouched.
func sweepStaleContainer(container string) {
	ownerPath := filepath.Join(container, ownerFileName)
	if _, err := os.Lstat(ownerPath); err == nil {
		sweepOwnedContainer(container, ownerPath)
		return
	} else if !os.IsNotExist(err) {
		return
	}
	// Legacy container without owner metadata: it may predate leases, so it
	// is recoverable only after being idle longer than the lease.
	info, err := os.Lstat(container)
	if err != nil || time.Since(info.ModTime()) < quarantineLease {
		return
	}
	sweepLegacyContainer(container)
}

// sweepOwnedContainer applies the lease and owner-token rules to a container
// that carries ownership metadata.
func sweepOwnedContainer(container, ownerPath string) {
	metadata, err := readOwnerMetadata(container)
	if err != nil {
		// Corrupt or foreign metadata: fail closed, touch nothing.
		return
	}
	if time.Since(metadata.HeartbeatAt) < quarantineLease {
		return // the owner run may still be alive
	}
	if sweepOwnerRecheckHook != nil {
		sweepOwnerRecheckHook()
	}
	// Re-read the owner token immediately before acting: a rewrite between
	// the two reads means the container was re-adopted by a live run.
	fresh, err := readOwnerMetadata(container)
	if err != nil || fresh.Owner != metadata.Owner {
		return
	}
	// A matching token is not enough on its own: a live owner paused past the
	// lease (SIGSTOP, VM suspend) looks dead until it proves otherwise. If the
	// heartbeat was refreshed between the two reads, the owner run is alive —
	// leave the container alone.
	// Residual race: an owner resuming between this re-read and the payload
	// renameNoReplace in sweepRecoveredPayloads can still lose the sweep race;
	// the outcome is fail-closed either way (no-replace restore, owner aborts
	// on its next heartbeat/validate failure).
	if time.Since(fresh.HeartbeatAt) < quarantineLease {
		return
	}
	if info, err := os.Lstat(container); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	sweepRecoveredPayloads(container, ownerPath)
}

// sweepRecoveredPayloads restores payload data from an expired container.
// Owner metadata is never counted as payload and is removed only after the
// payload is restored or the container is confirmed empty.
func sweepRecoveredPayloads(container, ownerPath string) {
	contents, err := os.ReadDir(container)
	if err != nil {
		return
	}
	var payloads []os.DirEntry
	for _, entry := range contents {
		if entry.Name() == ownerFileName {
			continue
		}
		payloads = append(payloads, entry)
	}
	switch len(payloads) {
	case 0:
		// Confirmed empty: the payload rename never happened or the payload
		// is already gone; metadata goes first, then the container.
		os.Remove(ownerPath)
		os.Remove(container)
	case 1:
		staged := payloads[0]
		stagedPath := filepath.Join(container, staged.Name())
		info, err := os.Lstat(stagedPath)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
			return
		}
		if err := renameNoReplace(stagedPath, filepath.Join(filepath.Dir(container), staged.Name())); err != nil {
			return
		}
		os.Remove(ownerPath)
		os.Remove(container)
	}
}

// sweepLegacyContainer recovers a pre-owner-metadata container: an empty
// container is removed, and a container holding exactly one entry that is a
// regular file or directory has that entry renamed back to its original name
// in the parent (no-replace) before the container is removed. Symlink or
// special-file entries, and containers with unexpected contents, are left
// untouched.
func sweepLegacyContainer(container string) {
	contents, err := os.ReadDir(container)
	if err != nil {
		return
	}
	if len(contents) == 0 {
		// A removal that was interrupted after the payload was deleted.
		os.Remove(container)
		return
	}
	if len(contents) != 1 {
		return
	}
	staged := contents[0]
	stagedPath := filepath.Join(container, staged.Name())
	info, err := os.Lstat(stagedPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
		return
	}
	if err := renameNoReplace(stagedPath, filepath.Join(filepath.Dir(container), staged.Name())); err != nil {
		return
	}
	os.Remove(container)
}

func restoreStaged(staged []StagedPath) error {
	_, err := restoreStagedResult(staged)
	return err
}

func restoreStagedResult(staged []StagedPath) ([]string, error) {
	var restored []string
	var errs []error
	for i := len(staged) - 1; i >= 0; i-- {
		item := staged[i]
		if err := validateContainer(item); err != nil {
			errs = append(errs, fmt.Errorf("history: cannot restore %s; expected staged payload at %s: %w", item.Original, item.Payload, err))
			continue
		}
		if err := renameNoReplace(item.Payload, item.Original); err != nil {
			errs = append(errs, fmt.Errorf("history: cannot restore %s without replacing a late path; staged data preserved at %s: %w", item.Original, item.Payload, err))
			continue
		}
		restored = append(restored, item.Original)
		if err := removeEmptyContainer(item); err != nil {
			errs = append(errs, fmt.Errorf("history: restored %s but could not remove its quarantine container: %w", item.Original, err))
		}
	}
	return restored, errors.Join(errs...)
}

func mapCandidatePaths(paths []string, staged []StagedPath) ([]string, error) {
	mapped := make([]string, len(paths))
	for i, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("history: resolve candidate path %s: %w", path, err)
		}
		absolute = filepath.Clean(absolute)
		found := false
		for _, item := range staged {
			if absolute == item.Original {
				mapped[i] = item.Payload
				found = true
				break
			}
			if isWithin(item.Original, absolute) {
				relative, err := filepath.Rel(item.Original, absolute)
				if err != nil {
					return nil, err
				}
				mapped[i] = filepath.Join(item.Payload, relative)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("history: candidate path %s is outside staged deletion roots", absolute)
		}
	}
	return mapped, nil
}

func isWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
