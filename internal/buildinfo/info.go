package buildinfo

import (
	"runtime"
	"strconv"
)

// These values are replaced with -ldflags for release builds.
var (
	Version  = "dev"
	Commit   = "unknown"
	Date     = "unknown"
	Sequence = "0"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Sequence  uint64 `json:"release_sequence"`
}

func Current() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Sequence:  ReleaseSequence(),
	}
}

func ReleaseSequence() uint64 {
	value, err := strconv.ParseUint(Sequence, 10, 64)
	if err != nil {
		return 0
	}
	return value
}
