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
	total        atomic.Int64 // set once after discovery; 0 = still discovering
	done         atomic.Int64
	files        atomic.Int64
	archives     atomic.Int64
	emitted      atomic.Int64
	dups         atomic.Int64
	discovered   atomic.Int64 // sources seen so far during the discovery walk
	logsTotal    atomic.Int64 // distinct logs (top-level subfolder or archive)
	logsDone     atomic.Int64
	current      atomic.Value // string: path of the item being processed
	phase        atomic.Int32
	historyTotal atomic.Int64
	historyDone  atomic.Int64
	ingestView   atomic.Value // func() IngestView, installed by BeginIngest

	// envOn gates the live "Env" counter row; envCopied bumps on each successful
	// file write under -env, envDeduped on each byte-identical duplicate the
	// copier skips instead of writing.
	envOn      atomic.Bool
	envCopied  atomic.Int64
	envDeduped atomic.Int64

	// dryRun (-odr): the live header and completion summary flag the run as a
	// preview so the user knows nothing will be written to the library.
	dryRun atomic.Bool

	// libraryOn is set when the run targets an sfu antipublic library (-od/-odr)
	// so the live header can show the same "vs library" hint sfu uses.
	libraryOn atomic.Bool

	// historySkipped: sources history marked complete at prepare time, for
	// the ingest header's muted skip badge. Known before the monitor starts.
	historySkipped atomic.Int32

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
	default:
		return ""
	}
}

// ActiveWorker is a snapshot of one busy worker slot for the TUI panel.
// LastULP is the wall-clock time the slot most recently entered an
// extracting/parsing stage.
type ActiveWorker struct {
	Index int
	Path  string
	Stage WorkerStage

	LastULP time.Time
}

// workerSlot is one engine worker's live status. A nil path pointer means the
// slot is idle. atomic.Pointer[string] gives lock-free consistent reads (there
// is no atomic string primitive); a fresh *string is stored per item.
type workerSlot struct {
	path  atomic.Pointer[string]
	stage atomic.Int32
	// lastULP (unix nano) records when the slot last did credential
	// extraction/parsing.
	lastULP atomic.Int64
}

// IngestWorker is one regen/index worker row for the ingest TUI panel.
type IngestWorker struct {
	Archive               string
	PartIdx, PartsTotal   int32
	BytesDone, BytesTotal int64
	// Committing flags the post-EOF sidecar commit window (spill merge +
	// fsync + rename); the TUI swaps the frozen byte totals for a
	// "committing…" label, mirroring sfu's OD worker rows. Not part of the
	// -json stream.
	Committing bool
}

// IngestView is a live snapshot of an in-process library ingest, rendered by
// the TUI's INGESTING frame. It is produced by a caller-supplied closure so
// sflog stays decoupled from the dedup engine that drives the merge.
type IngestView struct {
	Fraction float64 // 0..1 overall ingest progress
	Status   string  // short phase label ("merging…", etc.)

	EnginePhase int32 // ulpengine.Phase*

	// Library / regen (-od phase 0)
	ODPhase           int32
	ArchivesTotal     int32
	FilesTotal        int32
	ArchivesSkipped   int32
	PartsRegenDone    int32
	PartsRegenTotal   int32
	PartsUpgradeTotal int32
	RegenBytesRead    int64
	RegenBytesTotal   int64
	RegenBPS          float64
	KeysLoaded        int64

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
	// BusyWorkers/DedupWorkers feed the sfu-style dedup Progress rows;
	// RAMMB/CPUPct the System row. TUI-only; not mapped into -json.
	BusyWorkers  int32
	DedupWorkers int32
	RAMMB        float64
	CPUPct       float64

	Workers []IngestWorker

	// LibraryKeys is the dest-library size estimate (sfu's KeysTotalEstimate),
	// shown as the ingest Library stat row's compact "· N in library" total.
	// Zero until phase 0 has counted keys.
	LibraryKeys int64
}

const (
	phaseDiscover int32 = iota
	phaseHistory
	phaseExtract
	phaseIngest
	phaseDone
)

// BeginHistory switches the live tracker to source fingerprinting.
func (p *Progress) BeginHistory(total int64) {
	if p == nil {
		return
	}
	p.historyTotal.Store(total)
	p.historyDone.Store(0)
	p.phase.Store(phaseHistory)
}

func (p *Progress) addHistoryBytes(n int64) {
	if p != nil && n > 0 {
		p.historyDone.Add(n)
	}
}

func (p *Progress) HistoryBytesDone() int64 {
	if p == nil {
		return 0
	}
	d, total := p.historyDone.Load(), p.historyTotal.Load()
	if total > 0 && d > total {
		return total
	}
	return d
}

func (p *Progress) HistoryBytesTotal() int64 {
	if p == nil {
		return 0
	}
	return p.historyTotal.Load()
}

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

// SetDryRun flags the run as a -odr preview so the live header and completion
// summary mark it as DRY RUN. Called once from run() before extraction starts.
func (p *Progress) SetDryRun(v bool) {
	if p != nil {
		p.dryRun.Store(v)
	}
}
func (p *Progress) DryRun() bool { return p != nil && p.dryRun.Load() }

// SetLibrary flags the run as targeting an sfu antipublic library (-od/-odr)
// so the live header can show a "vs library" hint, matching sfu's dedup badge.
func (p *Progress) SetLibrary(v bool) {
	if p != nil {
		p.libraryOn.Store(v)
	}
}
func (p *Progress) LibraryEnabled() bool { return p != nil && p.libraryOn.Load() }

// SetHistorySkipped records the history-skip count known at prepare time;
// HistorySkipped feeds the ingest header's muted skip badge.
func (p *Progress) SetHistorySkipped(n int) { p.historySkipped.Store(int32(n)) }
func (p *Progress) HistorySkipped() int     { return int(p.historySkipped.Load()) }

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

// recordStage stores stage and timestamps credential activity (extract/parse)
// so a slot's recent activity is visible; other stages leave the
// activity clock untouched so a brief "opening" doesn't erase a fresh window.
func (p *Progress) recordStage(s *workerSlot, stage WorkerStage) {
	s.stage.Store(int32(stage))
	now := time.Now().UnixNano()
	switch stage {
	case StageExtracting, StageParsing:
		s.lastULP.Store(now)
	}
}

// setStage updates only the stage of an already-active worker slot.
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
// timestamps are reset so a reused slot can't inherit the prior item's window.
func (p *Progress) clearActive(idx int) {
	if s := p.slot(idx); s != nil {
		s.path.Store(nil)
		s.stage.Store(int32(StageIdle))
		s.lastULP.Store(0)
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
		out = append(out, ActiveWorker{
			Index:   i,
			Path:    *ptr,
			Stage:   WorkerStage(p.workers[i].stage.Load()),
			LastULP: unixNanoTime(lastULPnano),
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

// EnableEnv marks the run as copying env/key files so the TUI shows the live
// Env counter row.
func (p *Progress) EnableEnv() {
	if p != nil {
		p.envOn.Store(true)
	}
}

func (p *Progress) EnvEnabled() bool { return p != nil && p.envOn.Load() }

func (p *Progress) EnvCopied() int64 {
	if p == nil {
		return 0
	}
	return p.envCopied.Load()
}

func (p *Progress) addEnvCopied(n int64) {
	if p != nil && n > 0 {
		p.envCopied.Add(n)
	}
}

// EnvDeduped reports the number of byte-identical flat env/key files the
// copier skipped (exact-content dedup).
func (p *Progress) EnvDeduped() int64 {
	if p == nil {
		return 0
	}
	return p.envDeduped.Load()
}

func (p *Progress) addEnvDeduped() {
	if p != nil {
		p.envDeduped.Add(1)
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
