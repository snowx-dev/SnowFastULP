package ulpengine

// DefaultZstChunkLines is the default -split-zst granularity (lands
// ~1.2-1.8 GB compressed/part on typical ULP text). Exported so the sfu CLI
// can use it as a flag default without touching the unexported original.
const DefaultZstChunkLines = defaultZstChunkLines

// Exported aliases for the pipeline + -od phase enums. The engine keeps the
// lowercase originals for its own use; the command TUIs read these so they
// never touch unexported identifiers.

const (
	PhaseInit   = phaseInit
	PhasePhase0 = phasePhase0
	PhaseShard  = phaseShard
	PhaseDedup  = phaseDedup
	PhaseDone   = phaseDone
)

// ODPhase is the -od phase-0 (discover/regen/index) enum.
type ODPhase = odPhase

const (
	ODPhaseIdle     = odPhaseIdle
	ODPhaseDiscover = odPhaseDiscover
	ODPhaseRegen    = odPhaseRegen
	ODPhaseDone     = odPhaseDone
	ODPhaseUpgrade  = odPhaseUpgrade
)

// ODPhaseInFlight reports whether an -od scan is mid-flight (neither idle nor
// done) so the TUI can decide to draw the phase-0 panel.
func ODPhaseInFlight(m *ODMetrics) bool { return odPhaseInFlight(m) }
