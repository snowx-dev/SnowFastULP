package sflog

import (
	"io"
	"sync/atomic"
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
}

const (
	phaseDiscover int32 = iota
	phaseExtract
	phaseIngest
	phaseDone
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
func (p *Progress) addLogDone() {
	if p != nil {
		p.logsDone.Add(1)
	}
}

// creditor maps bytes read for a single item onto that item's fixed weight so
// progress never overshoots. zip/7z scale uncompressed reads onto the on-disk
// weight; loose files and rar read byte-for-byte (scale 1).
type creditor struct {
	p        *Progress
	weight   int64
	scale    float64
	credited int64
}

func newCreditor(p *Progress, weight int64, scale float64) *creditor {
	if scale <= 0 {
		scale = 1
	}
	return &creditor{p: p, weight: weight, scale: scale}
}

func (c *creditor) add(readBytes int64) {
	if c == nil || readBytes <= 0 {
		return
	}
	inc := int64(float64(readBytes) * c.scale)
	if c.credited+inc > c.weight {
		inc = c.weight - c.credited
	}
	if inc <= 0 {
		return
	}
	c.credited += inc
	c.p.add(inc)
}

// finish credits any rounding/shortfall remainder so each item contributes
// exactly its weight even on early stops or skipped members.
func (c *creditor) finish() {
	if c == nil {
		return
	}
	if rem := c.weight - c.credited; rem > 0 {
		c.credited += rem
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
