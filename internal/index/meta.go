package index

import (
	"os"
	"time"
)

const (
	EnsureActionLoad  = "load"
	EnsureActionBuild = "build"
)

// EnsureMeta describes how Ensure resolved an archive index.
type EnsureMeta struct {
	Action      string
	SidecarPath string
	ArchiveMod  time.Time
	SidecarMod  time.Time
	Stale       bool
	Missing     bool
}

func sidecarTimestamps(archivePath, sidecarPath string) (archMod, sidecarMod time.Time, err error) {
	archInfo, err := os.Stat(archivePath)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	archMod = archInfo.ModTime()
	if sidecarPath == "" {
		return archMod, time.Time{}, nil
	}
	idxInfo, err := os.Stat(sidecarPath)
	if err != nil {
		return archMod, time.Time{}, err
	}
	return archMod, idxInfo.ModTime(), nil
}
