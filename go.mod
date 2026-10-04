module github.com/snowx-dev/SnowFastULP

go 1.25.8

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/bodgit/sevenzip v1.6.4
	github.com/cespare/xxhash/v2 v2.3.0
	github.com/charmbracelet/lipgloss v1.1.0
	github.com/charmbracelet/x/ansi v0.11.6
	github.com/charmbracelet/x/term v0.2.2
	github.com/klauspost/compress v1.18.6
	github.com/lucasb-eyer/go-colorful v1.4.0
	github.com/muesli/termenv v0.16.0
	github.com/nwaples/rardecode/v2 v2.2.5
	github.com/yeka/zip v0.0.0-20231116150916-03d6312748a9
	golang.org/x/sys v0.45.0
	golang.org/x/term v0.43.0
	golang.org/x/text v0.37.0
	modernc.org/sqlite v1.53.0
)

require (
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/bodgit/plumbing v1.3.0 // indirect
	github.com/bodgit/windows v1.0.1 // indirect
	github.com/charmbracelet/colorprofile v0.4.1 // indirect
	github.com/charmbracelet/x/cellbuf v0.0.15 // indirect
	github.com/clipperhouse/displaywidth v0.9.0 // indirect
	github.com/clipperhouse/stringish v0.1.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.5.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.26 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/stangelandcl/ppmd v0.1.0 // indirect
	github.com/ulikunitz/xz v0.5.15 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	go4.org v0.0.0-20260112195520-a5071408f32f // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/sync v0.21.0 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Vendored fork with a ZIP64 central-directory parsing fix (members past the
// 4GB offset boundary). Upstream is unmaintained; see third_party/yekazip.
replace github.com/yeka/zip => ./third_party/yekazip
