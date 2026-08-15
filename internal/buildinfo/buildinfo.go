// Package buildinfo carries the identity of this binary: which version it is,
// which commit it was built from, and when.
//
// An operator looking at a misbehaving daemon needs to answer "what is actually
// running here?" before anything else, and a support report that says "the
// latest one" is not an answer. The values are stamped at link time by the
// Makefile and the Dockerfile:
//
//	-X github.com/unifers/feather/internal/buildinfo.version=v1.2.3
//
// A `go install`ed or `go run` binary gets no -X flags, so the values fall back
// to whatever the module system recorded. That fallback is deliberately silent:
// it is better to report "(devel)" honestly than to claim a version nobody cut.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Stamped at link time. Do not read these directly — use the accessors, which
// apply the ReadBuildInfo fallback.
var (
	version = ""
	commit  = ""
	date    = ""
)

var resolve = sync.OnceValue(func() Info {
	info := Info{Version: version, Commit: commit, Date: date, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}

	// Fill the gaps from the module's own build metadata. `go build` records
	// the VCS revision and dirtiness automatically when building from a git
	// checkout, which covers the common "built it myself" case.
	if bi, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "" && bi.Main.Version != "" {
			info.Version = bi.Main.Version
		}
		var dirty bool
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = setting.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = setting.Value
				}
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
		if dirty && !strings.HasSuffix(info.Commit, "-dirty") && info.Commit != "" {
			info.Commit += "-dirty"
		}
	}

	if info.Version == "" {
		info.Version = "(devel)"
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.Date == "" {
		info.Date = "unknown"
	}
	return info
})

// Info describes the running binary.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Get returns the build identity, resolved once.
func Get() Info { return resolve() }

// Version is the release version alone — the short form, for a page footer or a
// metrics label.
func Version() string { return resolve().Version }

// Short renders "v1.2.3 (abc1234)", the form to put in a log line or a UI
// footer where one line is all there is.
func Short() string {
	info := resolve()
	commit := info.Commit
	if len(commit) > 7 && commit != "unknown" {
		commit = commit[:7]
	}
	return fmt.Sprintf("%s (%s)", info.Version, commit)
}

// String renders the full multi-line form for `feather version`.
func String() string {
	info := resolve()
	return fmt.Sprintf("feather %s\n  commit: %s\n  built:  %s\n  go:     %s\n  target: %s/%s",
		info.Version, info.Commit, info.Date, info.Go, info.OS, info.Arch)
}

// UserAgent identifies feather to the services it calls, so a bucket owner
// reading access logs can tell which tool and which version made a request.
func UserAgent() string { return "feather/" + resolve().Version }
