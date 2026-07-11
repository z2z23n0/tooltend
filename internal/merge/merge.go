// Package merge implements a deliberately conservative three-way tree merge.
// It prefers a review-required result over guessing when file identity, type,
// permissions, or edit ordering is ambiguous.
package merge

import (
	"bytes"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

// Kind identifies the filesystem object represented by an Entry.
type Kind string

const (
	KindRegular Kind = "regular"
	KindSymlink Kind = "symlink"
)

// Entry is one leaf in a component tree. Data is file content for regular
// files and the link target for symlinks.
type Entry struct {
	Kind Kind        `json:"kind"`
	Mode fs.FileMode `json:"mode"`
	Data []byte      `json:"data,omitempty"`
}

// Tree is keyed by slash-separated paths relative to a component root.
type Tree map[string]Entry

type Status string

const (
	StatusMerged   Status = "merged"
	StatusConflict Status = "conflict"
	StatusFork     Status = "fork"
)

type Reason string

const (
	ReasonNoBaseline     Reason = "no_baseline"
	ReasonInvalidPath    Reason = "invalid_path"
	ReasonPathCollision  Reason = "path_collision"
	ReasonUnsupported    Reason = "unsupported_file_type"
	ReasonSymlink        Reason = "symlink"
	ReasonModeChanged    Reason = "mode_changed"
	ReasonBinaryChanged  Reason = "binary_changed"
	ReasonDeleteModify   Reason = "delete_modify"
	ReasonAddAdd         Reason = "add_add"
	ReasonTextOverlap    Reason = "text_overlap"
	ReasonTextTooComplex Reason = "text_too_complex"
)

type Conflict struct {
	Path   string `json:"path"`
	Reason Reason `json:"reason"`
}

// Result contains a merged tree only when Status is StatusMerged. Callers
// must not activate Tree when any conflict is present.
type Result struct {
	Status    Status     `json:"status"`
	Tree      Tree       `json:"tree,omitempty"`
	Conflicts []Conflict `json:"conflicts,omitempty"`
}

// MergeTree merges binding-local changes and a new upstream tree relative to
// a known baseline. A nil baseline means provenance is unavailable and is
// classified as a fork instead of manufacturing a merge base.
func MergeTree(baseline *Tree, local, upstream Tree) Result {
	if baseline == nil {
		return Result{
			Status:    StatusFork,
			Conflicts: []Conflict{{Reason: ReasonNoBaseline}},
		}
	}

	allPaths := make(map[string]struct{}, len(*baseline)+len(local)+len(upstream))
	for name := range *baseline {
		allPaths[name] = struct{}{}
	}
	for name := range local {
		allPaths[name] = struct{}{}
	}
	for name := range upstream {
		allPaths[name] = struct{}{}
	}
	paths := make([]string, 0, len(allPaths))
	for name := range allPaths {
		paths = append(paths, name)
	}
	sort.Strings(paths)

	conflicts := validatePaths(paths)
	merged := make(Tree, len(paths))
	for _, name := range paths {
		base, hasBase := (*baseline)[name]
		ours, hasOurs := local[name]
		theirs, hasTheirs := upstream[name]

		entry, keep, reason := mergeEntry(base, hasBase, ours, hasOurs, theirs, hasTheirs)
		if reason != "" {
			conflicts = append(conflicts, Conflict{Path: name, Reason: reason})
			continue
		}
		if keep {
			merged[name] = cloneEntry(entry)
		}
	}

	if len(conflicts) != 0 {
		sort.Slice(conflicts, func(i, j int) bool {
			if conflicts[i].Path == conflicts[j].Path {
				return conflicts[i].Reason < conflicts[j].Reason
			}
			return conflicts[i].Path < conflicts[j].Path
		})
		return Result{Status: StatusConflict, Conflicts: conflicts}
	}
	return Result{Status: StatusMerged, Tree: merged}
}

func mergeEntry(base Entry, hasBase bool, ours Entry, hasOurs bool, theirs Entry, hasTheirs bool) (Entry, bool, Reason) {
	if !hasBase {
		switch {
		case hasOurs && hasTheirs:
			if !entryEqual(ours, theirs) {
				return Entry{}, false, ReasonAddAdd
			}
			if reason := unsafeNewEntry(ours); reason != "" {
				return Entry{}, false, reason
			}
			return ours, true, ""
		case hasOurs:
			if reason := unsafeNewEntry(ours); reason != "" {
				return Entry{}, false, reason
			}
			return ours, true, ""
		case hasTheirs:
			if reason := unsafeNewEntry(theirs); reason != "" {
				return Entry{}, false, reason
			}
			return theirs, true, ""
		default:
			return Entry{}, false, ""
		}
	}

	if !hasOurs && !hasTheirs {
		if base.Kind == KindSymlink {
			return Entry{}, false, ReasonSymlink
		}
		if binary(base.Data) {
			return Entry{}, false, ReasonBinaryChanged
		}
		return Entry{}, false, ""
	}
	if !hasOurs {
		if entryEqual(base, theirs) {
			if base.Kind == KindSymlink {
				return Entry{}, false, ReasonSymlink
			}
			if binary(base.Data) {
				return Entry{}, false, ReasonBinaryChanged
			}
			return Entry{}, false, ""
		}
		return Entry{}, false, ReasonDeleteModify
	}
	if !hasTheirs {
		if entryEqual(base, ours) {
			if base.Kind == KindSymlink {
				return Entry{}, false, ReasonSymlink
			}
			if binary(base.Data) {
				return Entry{}, false, ReasonBinaryChanged
			}
			return Entry{}, false, ""
		}
		return Entry{}, false, ReasonDeleteModify
	}

	if reason := unsupportedKind(base, ours, theirs); reason != "" {
		return Entry{}, false, reason
	}
	if base.Mode.Perm() != ours.Mode.Perm() || base.Mode.Perm() != theirs.Mode.Perm() {
		return Entry{}, false, ReasonModeChanged
	}
	if binary(base.Data) || binary(ours.Data) || binary(theirs.Data) {
		if entryEqual(base, ours) && entryEqual(base, theirs) {
			return base, true, ""
		}
		return Entry{}, false, ReasonBinaryChanged
	}
	if entryEqual(ours, theirs) {
		return ours, true, ""
	}
	if entryEqual(base, ours) {
		return theirs, true, ""
	}
	if entryEqual(base, theirs) {
		return ours, true, ""
	}

	text := MergeText(base.Data, ours.Data, theirs.Data)
	if text.Conflict {
		return Entry{}, false, text.Reason
	}
	return Entry{Kind: KindRegular, Mode: base.Mode, Data: text.Data}, true, ""
}

func unsafeNewEntry(entry Entry) Reason {
	if entry.Kind == KindSymlink {
		return ReasonSymlink
	}
	if entry.Kind != KindRegular {
		return ReasonUnsupported
	}
	if entry.Mode&fs.ModeType != 0 {
		return ReasonUnsupported
	}
	if entry.Mode.Perm()&0o111 != 0 {
		return ReasonModeChanged
	}
	if binary(entry.Data) {
		return ReasonBinaryChanged
	}
	return ""
}

func unsupportedKind(entries ...Entry) Reason {
	for _, entry := range entries {
		if entry.Kind == KindSymlink {
			return ReasonSymlink
		}
		if entry.Kind != KindRegular || entry.Mode&fs.ModeType != 0 {
			return ReasonUnsupported
		}
	}
	return ""
}

func entryEqual(a, b Entry) bool {
	return a.Kind == b.Kind && a.Mode.Perm() == b.Mode.Perm() && bytes.Equal(a.Data, b.Data)
}

func cloneEntry(entry Entry) Entry {
	entry.Data = bytes.Clone(entry.Data)
	return entry
}

func binary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
}

func validatePaths(paths []string) []Conflict {
	var conflicts []Conflict
	valid := make(map[string]struct{}, len(paths))
	for _, name := range paths {
		if name == "" || strings.ContainsRune(name, 0) || strings.Contains(name, `\`) || path.IsAbs(name) {
			conflicts = append(conflicts, Conflict{Path: name, Reason: ReasonInvalidPath})
			continue
		}
		clean := path.Clean(name)
		if clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			conflicts = append(conflicts, Conflict{Path: name, Reason: ReasonInvalidPath})
			continue
		}
		valid[name] = struct{}{}
	}
	for name := range valid {
		parent := path.Dir(name)
		for parent != "." {
			if _, exists := valid[parent]; exists {
				conflicts = append(conflicts, Conflict{Path: name, Reason: ReasonPathCollision})
				break
			}
			parent = path.Dir(parent)
		}
	}
	return conflicts
}
