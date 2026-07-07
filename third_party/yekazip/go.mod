// Vendored fork of github.com/yeka/zip (v0.0.0-20231116150916-03d6312748a9).
//
// Patched to fix a ZIP64 central-directory parsing bug: the upstream reader
// consumes the ZIP64 extra block unconditionally by length instead of by the
// 0xFFFFFFFF sentinel, so any member whose local-header offset is past the 4GB
// mark (i.e. any zip larger than ~4GB) is misparsed and fails to open. See
// reader.go readDirectoryHeader. Upstream master is unmaintained.
module github.com/yeka/zip

go 1.25.0

require golang.org/x/crypto v0.46.0
