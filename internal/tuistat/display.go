package tuistat

// ShowLiveDisplay reports whether the alt-screen monitor and the inline
// pre-pass bars should run.
//
// noTUI is -no-tui or config no_tui. jsonTarget is the resolved -json
// target: "" means off, "-" means stdout, any other string is a file path.
// stderrTTY is the human display. stdoutTTY is whether stdout is a terminal.
// vtOK is false on a console that cannot render ANSI.
//
// -no-tui always hides the live display. -json hides it only when the
// stream is stdout and stdout is a terminal, because those bytes would land
// on the same terminal the alt-screen takes over. A file target, or stdout
// piped away from the terminal, leaves the live display on.
func ShowLiveDisplay(noTUI bool, jsonTarget string, stderrTTY, stdoutTTY, vtOK bool) bool {
	if noTUI || !stderrTTY || !vtOK {
		return false
	}
	return !(jsonTarget == "-" && stdoutTTY)
}
