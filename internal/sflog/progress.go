package sflog

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Progress is a concurrency-safe view of an in-flight extraction, consumed by
// the live TUI. Total is the sum of per-item weights (on-disk bytes) computed
// during discovery; Done is credited as bytes are read so the bar moves
// smoothly and reaches Total exactly when every item is processed or skipped.
type Progress struct {
	total      atomic.Int64 // set once after discovery; 0 = still discovering
	done       atomic.Int64
	files      atomic.Int64
	archives   atomic.Int64
	emitted    atomic.Int64
	dups       atomic.Int64
	discovered atomic.Int64 // sources seen so far during the discovery walk
	logsTotal  atomic.Int64 // distinct logs (top-level subfolder or archive)
	logsDone   atomic.Int64
	current    atomic.Value // string: path of the item being processed
	phase      atomic.Int32
	ingestView atomic.Value // func() IngestView, installed by BeginIngest

	// Secret scanning (-secrets). secretsOn gates the second progress bar and
	// the live "Secrets" row so they surface only when a sink is wired.
	// secretsFound drives the found count; secretFilesScanned/secretFilesTotal
	// drive both the live "X / Y files" row and the secondary bar (scanned /
	// total). scan cost is per-file (a Titus pass runs regardless of size), so a
	// file count tracks real progress where bytes cannot: compressed archive
	// members weigh almost nothing on disk yet dominate scan time. The total is
	// seeded up front — loose files plus a discovery-time member pre-count for
	// zip/7z — so the ratio climbs monotonically instead of lurching when a late
	// archive inflates the denominator. Streaming/encrypted members that cannot
	// be pre-counted are credited at open and smoothed by the TUI's monotonic
	// display clamp.
	secretsOn          atomic.Bool
	secretsFound       atomic.Int64
	secretFilesScanned atomic.Int64
	secretFilesTotal   atomic.Int64
	// secretStreamsOpen is the count of in-flight sources whose scan-candidate
	// members are discovered incrementally (rar, encrypted-header 7z, nested
	// archives) rather than pre-counted. While > 0 the "X / Y" denominator is
	// not final, so the TUI renders "Y+" to signal "still growing".
	secretStreamsOpen atomic.Int64

	// dryRun (-odr): the live header and completion summary flag the run as a
	// preview so the user knows nothing will be written to the library.
	dryRun atomic.Bool

	// workers is the live status registry the TUI reads to render concurrent
	// activity. Sized once by SetWorkers; guarded by workersMu for the slice
	// header (resize) and the free-list, slot fields themselves are atomic.
	// Slots are no longer pinned to a fixed engine worker: any in-flight task
	// (top-level item, dispatched archive member, password probe) leases a slot
	// from slotFree for its lifetime, so the panel shows real concurrency. Total
	// simultaneous leases is bounded by the extraction budget (= worker count),
	// so the free-list never underflows for budgeted work; opportunistic tasks
	// that miss out (acquireSlot returns -1) still run, just without a row.
	workersMu sync.RWMutex
	workers   []workerSlot
	slotFree  []int
}

// WorkerStage labels what an extraction worker is doing right now. It is
// surfaced per-slot in the live TUI so the user sees many things happening at
// once even when the byte bar moves slowly.
type WorkerStage int32

const (
	StageIdle WorkerStage = iota
	StageOpening
	StageTestingPassword
	StageExtracting
	StageParsing
	// StageScanning marks a worker running a secret scan (the CPU-bound Titus
	// pass over a member's bytes). It is distinct from StageExtracting so the
	// panel reads "scanning" during the -secrets tail — when the byte bar is
	// already at 100% but the scanners are still working — instead of looking
	// stuck on "extracting".
	StageScanning
)

func (s WorkerStage) String() string {
	switch s {
	case StageOpening:
		return "opening"
	case StageTestingPassword:
		return "testing password"
	case StageExtracting:
		return "extracting"
	case StageParsing:
		return "parsing"
	case StageScanning:
		return "scanning"
	default:
		return ""
	}
}

// ActiveWorker is a snapshot of one busy worker slot for the TUI panel.
// LastULP/LastSec are the wall-clock times the slot most recently entered an
// extracting/parsing or scanning stage; the renderer uses them to surface
// "ulp + secrets" when one archive is doing both within a short window.
type ActiveWorker struct {
	Index int
	Path  string
	Stage WorkerStage

	LastULP time.Time
	LastSec time.Time
}

// workerSlot is one engine worker's live status. A nil path pointer means the
// slot is idle. atomic.Pointer[string] gives lock-free consistent reads (there
// is no atomic string primitive); a fresh *string is stored per item.
type workerSlot struct {
	path  atomic.Pointer[string]
	stage atomic.Int32
	// lastULP/lastSec (unix nano) record when the slot last did credential
	// extraction/parsing vs. secret scanning, so the TUI can render a combined
	// "ulp + secrets" label for an archive doing both concurrently.
	lastULP atomic.Int64
	lastSec atomic.Int64
}

// IngestWorker is one regen/index worker row for the ingest TUI panel.
type IngestWorker struct {
	Archive               string
	PartIdx, PartsTotal   int32
	BytesDone, BytesTotal int64
}

// IngestView is a live snapshot of an in-process library ingest, rendered by
// the TUI's INGESTING frame. It is produced by a caller-supplied closure so
// sflog stays decoupled from the dedup engine that drives the merge.
type IngestView struct {
	Fraction float64 // 0..1 overall ingest progress
	Status   string  // short phase label ("merging…", etc.)

	EnginePhase int32 // ulpengine.Phase*

	// Library / regen (-od phase 0)
	ODPhase         int32
	ArchivesTotal   int32
	PartsRegenDone  int32
	PartsRegenTotal int32
	RegenBytesRead  int64
	RegenBytesTotal int64
	RegenBPS        float64

	// ULP read (shard)
	ULPBytes  int64
	BytesRead int64
	LinesRead int64

	// Dedup merge
	ShowMerge         bool
	Unique            int64
	Skipped           int64
	BucketsDone       int64
	BucketsTotal      int64
	BucketsBytesRead  int64
	BucketsBytesTotal int64

	Workers []IngestWorker
}

const (
	phaseDiscover int32 = iota
	phaseExtract
	phaseIngest
	phaseDone
	// phaseSecretsFinalize is the brief dedicated phase between extraction and
	// the summary while the secrets store drains its last batch and checkpoints
	// its WAL. Appended (not inserted) so the earlier values the TUI mirrors
	// stay put. Only entered on a -secrets run via BeginSecretsFinalize.
	phaseSecretsFinalize
)

// NewProgress returns a tracker in the discovery phase. The engine fills in the
// total weight once discovery completes.
func NewProgress() *Progress {
	p := &Progress{}
	p.current.Store("")
	return p
}

func (p *Progress) setTotal(n int64) {
	if p != nil {
		p.total.Store(n)
	}
}

func (p *Progress) Total() int64 {
	if p == nil {
		return 0
	}
	return p.total.Load()
}

func (p *Progress) DoneBytes() int64 {
	if p == nil {
		return 0
	}
	total := p.total.Load()
	d := p.done.Load()
	if total > 0 && d > total {
		return total
	}
	return d
}

func (p *Progress) Fraction() float64 {
	if p == nil {
		return 0
	}
	total := p.total.Load()
	if total <= 0 {
		return 0
	}
	f := float64(p.DoneBytes()) / float64(total)
	if f > 1 {
		return 1
	}
	return f
}

func (p *Progress) Files() int64    { return p.files.Load() }
func (p *Progress) Archives() int64 { return p.archives.Load() }
func (p *Progress) Emitted() int64  { return p.emitted.Load() }
func (p *Progress) Duplicates() int64 {
	return p.dups.Load()
}
func (p *Progress) Discovered() int64 {
	if p == nil {
		return 0
	}
	return p.discovered.Load()
}
func (p *Progress) LogsTotal() int64 {
	if p == nil {
		return 0
	}
	return p.logsTotal.Load()
}
func (p *Progress) Logs() int64 {
	if p == nil {
		return 0
	}
	return p.logsDone.Load()
}
func (p *Progress) Phase() int32 { return p.phase.Load() }

// EnableSecrets marks the run as scanning for secrets so the TUI shows the
// second bar and the live Secrets row. Called once by the engine when a sink is
// wired.
func (p *Progress) EnableSecrets() {
	if p != nil {
		p.secretsOn.Store(true)
	}
}
func (p *Progress) SecretsEnabled() bool { return p != nil && p.secretsOn.Load() }

// SetDryRun flags the run as a -odr preview so the live header and completion
// summary mark it as DRY RUN. Called once from run() before extraction starts.
func (p *Progress) SetDryRun(v bool) {
	if p != nil {
		p.dryRun.Store(v)
	}
}
func (p *Progress) DryRun() bool { return p != nil && p.dryRun.Load() }
func (p *Progress) SecretsFound() int64 {
	if p == nil {
		return 0
	}
	return p.secretsFound.Load()
}
func (p *Progress) SecretFilesScanned() int64 {
	if p == nil {
		return 0
	}
	return p.secretFilesScanned.Load()
}

// SecretFilesTotal is the number of secret-scan candidates identified so far
// (loose files up front, archive members as each archive opens). It is the
// denominator of the live "X / Y files" row; it grows during the run, always
// ahead of SecretFilesScanned, so the user can see what is left to scan.
func (p *Progress) SecretFilesTotal() int64 {
	if p == nil {
		return 0
	}
	return p.secretFilesTotal.Load()
}

// ScanFraction is the secret-scan progress by file count (secretFilesScanned /
// secretFilesTotal): the secondary bar's raw value. Scan cost is per-file, so
// this tracks the CPU-bound scan tail that the byte bar (Fraction) cannot —
// Fraction hits 100% as soon as reads finish, long before the scanners do. With
// the total seeded up front it climbs monotonically; the small residual from
// members counted only at open (streaming/encrypted) is smoothed by the TUI's
// monotonic clamp. Returns the raw ratio (clamped to [0,1]); the clamp lives at
// the display layer so this stays a pure, honest reading.
func (p *Progress) ScanFraction() float64 {
	if p == nil {
		return 0
	}
	total := p.secretFilesTotal.Load()
	if total <= 0 {
		return 0
	}
	f := float64(p.secretFilesScanned.Load()) / float64(total)
	if f > 1 {
		return 1
	}
	return f
}

// BeginSecretsFinalize flips the tracker into the dedicated secrets-flush phase
// so the live frame stops showing 100% extraction and reads as active work
// while the store drains.
func (p *Progress) BeginSecretsFinalize() {
	if p != nil {
		p.phase.Store(phaseSecretsFinalize)
	}
}

// BeginIngest flips the tracker into the ingest phase and installs the view
// provider the live frame polls each tick. Called once, after extraction
// finishes, by the -od driver.
func (p *Progress) BeginIngest(view func() IngestView) {
	if p == nil {
		return
	}
	if view != nil {
		p.ingestView.Store(view)
	}
	p.phase.Store(phaseIngest)
}

// IngestSnapshot returns the current ingest view and true when an ingest is in
// flight, or false before BeginIngest installs a provider.
func (p *Progress) IngestSnapshot() (IngestView, bool) {
	if p == nil {
		return IngestView{}, false
	}
	if v, ok := p.ingestView.Load().(func() IngestView); ok && v != nil {
		return v(), true
	}
	return IngestView{}, false
}
func (p *Progress) Current() string {
	if p == nil {
		return ""
	}
	if v, ok := p.current.Load().(string); ok {
		return v
	}
	return ""
}

func (p *Progress) setCurrent(path string) {
	if p != nil {
		p.current.Store(path)
	}
}

// SetWorkers sizes the active-worker registry to n slots. Called once by the
// engine after it resolves its worker count; n<=0 leaves the registry empty so
// the TUI falls back to the single-line current path.
func (p *Progress) SetWorkers(n int) {
	if p == nil || n < 0 {
		return
	}
	p.workersMu.Lock()
	p.workers = make([]workerSlot, n)
	// Fill the free-list so acquireSlot (pop from the end) hands out 0,1,2,...
	// first, keeping the panel's busy rows packed at the low indices.
	p.slotFree = make([]int, n)
	for i := range p.slotFree {
		p.slotFree[i] = n - 1 - i
	}
	p.workersMu.Unlock()
}

// acquireSlot leases a free registry slot index for an in-flight task, or -1
// when none is free (the task still runs, just without a live row). The lease
// is released with releaseSlot. Callers pair this with the extraction budget so
// leases never exceed the slot count.
func (p *Progress) acquireSlot() int {
	if p == nil {
		return -1
	}
	p.workersMu.Lock()
	defer p.workersMu.Unlock()
	if len(p.slotFree) == 0 {
		return -1
	}
	idx := p.slotFree[len(p.slotFree)-1]
	p.slotFree = p.slotFree[:len(p.slotFree)-1]
	return idx
}

// releaseSlot clears the slot's live status and returns it to the free-list.
// A negative idx (no lease was granted) is a no-op.
func (p *Progress) releaseSlot(idx int) {
	if p == nil || idx < 0 {
		return
	}
	p.clearActive(idx)
	p.workersMu.Lock()
	p.slotFree = append(p.slotFree, idx)
	p.workersMu.Unlock()
}

// WorkerCount is the number of registered worker slots (0 if SetWorkers was
// never called).
func (p *Progress) WorkerCount() int {
	if p == nil {
		return 0
	}
	p.workersMu.RLock()
	defer p.workersMu.RUnlock()
	return len(p.workers)
}

func (p *Progress) slot(idx int) *workerSlot {
	if p == nil {
		return nil
	}
	p.workersMu.RLock()
	defer p.workersMu.RUnlock()
	if idx < 0 || idx >= len(p.workers) {
		return nil
	}
	return &p.workers[idx]
}

// setActive marks worker idx busy on path at the given stage.
func (p *Progress) setActive(idx int, path string, stage WorkerStage) {
	if s := p.slot(idx); s != nil {
		s.path.Store(&path)
		s.lastULP.Store(0)
		s.lastSec.Store(0)
		p.recordStage(s, stage)
	}
}

// unixNanoTime converts a stored unix-nano activity stamp to a time.Time,
// mapping a never-set stamp (0) to the Go zero time so callers can use IsZero.
func unixNanoTime(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// recordStage stores stage and timestamps the ULP vs. secret activity times used
// by the "ulp + secrets" combined panel label. StageScanning marks secret work;
// StageExtracting/StageParsing mark credential work; other stages leave the
// activity clocks untouched so a brief "opening" doesn't erase a fresh window.
func (p *Progress) recordStage(s *workerSlot, stage WorkerStage) {
	s.stage.Store(int32(stage))
	now := time.Now().UnixNano()
	switch stage {
	case StageScanning:
		s.lastSec.Store(now)
	case StageExtracting, StageParsing:
		s.lastULP.Store(now)
	}
}

// setStage updates only the stage of an already-active worker slot, and records
// the time of ULP vs. secret activity so the panel can show "ulp + secrets" when
// one worker is interleaving both on a sequential archive stream.
func (p *Progress) setStage(idx int, stage WorkerStage) {
	if s := p.slot(idx); s != nil {
		p.recordStage(s, stage)
	}
}

// setWorkerPath updates only the displayed path of an already-active worker
// slot, leaving its stage intact. The recursive archive readers call this as
// they descend into / return from nested archives so the live line names the
// archive the worker is actually inside, not just the top-level item.
func (p *Progress) setWorkerPath(idx int, path string) {
	if s := p.slot(idx); s != nil {
		s.path.Store(&path)
	}
}

// clearActive marks worker idx idle once it finishes its current item. Activity
// timestamps are reset so a reused slot can't inherit the prior item's "ulp +
// secrets" window.
func (p *Progress) clearActive(idx int) {
	if s := p.slot(idx); s != nil {
		s.path.Store(nil)
		s.stage.Store(int32(StageIdle))
		s.lastULP.Store(0)
		s.lastSec.Store(0)
	}
}

// ActiveWorkers snapshots up to max busy slots, lowest index first, into a
// fresh slice so the renderer never re-reads the atomics. max<=0 returns nil.
func (p *Progress) ActiveWorkers(max int) []ActiveWorker {
	if p == nil || max <= 0 {
		return nil
	}
	p.workersMu.RLock()
	defer p.workersMu.RUnlock()
	out := make([]ActiveWorker, 0, min(max, len(p.workers)))
	for i := range p.workers {
		ptr := p.workers[i].path.Load()
		if ptr == nil {
			continue
		}
		lastULPnano := p.workers[i].lastULP.Load()
		lastSecNano := p.workers[i].lastSec.Load()
		out = append(out, ActiveWorker{
			Index:   i,
			Path:    *ptr,
			Stage:   WorkerStage(p.workers[i].stage.Load()),
			LastULP: unixNanoTime(lastULPnano),
			LastSec: unixNanoTime(lastSecNano),
		})
		if len(out) >= max {
			break
		}
	}
	return out
}
func (p *Progress) setPhase(ph int32) {
	if p != nil {
		p.phase.Store(ph)
	}
}
func (p *Progress) add(n int64) {
	if p != nil && n > 0 {
		p.done.Add(n)
	}
}
func (p *Progress) addEmitted() {
	if p != nil {
		p.emitted.Add(1)
	}
}
func (p *Progress) addDup() {
	if p != nil {
		p.dups.Add(1)
	}
}
func (p *Progress) addFile() {
	if p != nil {
		p.files.Add(1)
	}
}
func (p *Progress) addArchive() {
	if p != nil {
		p.archives.Add(1)
	}
}

// addArchives credits nested archives discovered while processing one item so
// the live count matches the final ArchivesScanned (which includes nested).
func (p *Progress) addArchives(n int64) {
	if p != nil && n > 0 {
		p.archives.Add(n)
	}
}
func (p *Progress) addDiscovered() {
	if p != nil {
		p.discovered.Add(1)
	}
}
func (p *Progress) setLogsTotal(n int64) {
	if p != nil {
		p.logsTotal.Store(n)
	}
}

// addSecretFilesTotal credits scan candidates identified as a group: loose
// files and pre-counted zip/7z members up front, streaming/encrypted members at
// open. The total leads the scanned count so "X / Y" shows what is left.
func (p *Progress) addSecretFilesTotal(n int64) {
	if p != nil && n > 0 {
		p.secretFilesTotal.Add(n)
	}
}

// beginSecretStream marks a non-pre-counted source (rar, encrypted-header 7z,
// nested archive) as opened: its member count is discovered as it streams, so
// the "X / Y" denominator is incomplete while it runs. Pairs with
// endSecretStream; nil-safe.
func (p *Progress) beginSecretStream() {
	if p != nil {
		p.secretStreamsOpen.Add(1)
	}
}

// endSecretStream marks a streaming source closed, clearing the "Y+" signal once
// the last one finishes. nil-safe.
func (p *Progress) endSecretStream() {
	if p != nil {
		p.secretStreamsOpen.Add(-1)
	}
}

// SecretStreamsOpen reports the number of in-flight sources whose candidate
// count is still being discovered. > 0 means the "X / Y" denominator can still
// grow. nil-safe.
func (p *Progress) SecretStreamsOpen() int64 {
	if p == nil {
		return 0
	}
	return p.secretStreamsOpen.Load()
}
func (p *Progress) addSecretsFound(n int64) {
	if p != nil && n > 0 {
		p.secretsFound.Add(n)
	}
}
func (p *Progress) addSecretFileScanned() {
	if p != nil {
		p.secretFilesScanned.Add(1)
	}
}
func (p *Progress) addLogDone() {
	if p != nil {
		p.logsDone.Add(1)
	}
}

// creditor maps bytes read for a single item onto that item's fixed weight so
// progress never overshoots. zip/7z scale uncompressed reads onto the on-disk
// weight; loose files and rar read byte-for-byte (scale 1). credited is atomic
// so the zip member pool can credit concurrently from many goroutines.
type creditor struct {
	p        *Progress
	weight   int64
	scale    float64
	credited atomic.Int64
}

func newCreditor(p *Progress, weight int64, scale float64) *creditor {
	if scale <= 0 {
		scale = 1
	}
	return &creditor{p: p, weight: weight, scale: scale}
}

// add credits readBytes (scaled) toward the item's weight, clamped so concurrent
// callers can never overshoot. The CAS loop makes the clamp atomic with respect
// to other workers crediting the same item.
func (c *creditor) add(readBytes int64) {
	if c == nil || readBytes <= 0 {
		return
	}
	want := int64(float64(readBytes) * c.scale)
	if want <= 0 {
		return
	}
	for {
		cur := c.credited.Load()
		inc := want
		if cur+inc > c.weight {
			inc = c.weight - cur
		}
		if inc <= 0 {
			return
		}
		if c.credited.CompareAndSwap(cur, cur+inc) {
			c.p.add(inc)
			return
		}
	}
}

// useScale recomputes the uncompressed->on-disk mapping once the member table
// is known. The 7z reader only learns total uncompressed size after opening the
// archive (unlike zip, which has it from the central directory up front), so it
// sets the scale here before crediting. Safe only before any concurrent
// crediting; the 7z member read is sequential.
func (c *creditor) useScale(uncompressed int64) {
	if c == nil {
		return
	}
	c.scale = scaleFor(c.weight, uncompressed)
}

// finish credits any rounding/shortfall remainder so each item contributes
// exactly its weight even on early stops or skipped members. Called once per
// item after its (possibly parallel) member pass has joined, so it is not
// contended; a plain Add keeps the books straight.
func (c *creditor) finish() {
	if c == nil {
		return
	}
	if rem := c.weight - c.credited.Load(); rem > 0 {
		c.credited.Add(rem)
		c.p.add(rem)
	}
}

type countingReader struct {
	r io.Reader
	c *creditor
}

func (cr countingReader) Read(b []byte) (int, error) {
	n, err := cr.r.Read(b)
	if n > 0 {
		cr.c.add(int64(n))
	}
	return n, err
}
