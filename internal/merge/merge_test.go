package merge

import (
	"bytes"
	"io/fs"
	"testing"
)

func TestMergeTextDisjointEdits(t *testing.T) {
	result := MergeText(
		[]byte("alpha\nbeta\ngamma\n"),
		[]byte("ALPHA\nbeta\ngamma\n"),
		[]byte("alpha\nbeta\nGAMMA\n"),
	)
	if result.Conflict {
		t.Fatalf("unexpected conflict: %s", result.Reason)
	}
	if got, want := string(result.Data), "ALPHA\nbeta\nGAMMA\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMergeTextConflictingEdits(t *testing.T) {
	result := MergeText([]byte("one\ntwo\n"), []byte("ONE\ntwo\n"), []byte("uno\ntwo\n"))
	if !result.Conflict || result.Reason != ReasonTextOverlap {
		t.Fatalf("got %+v", result)
	}
}

func TestMergeTextPreservesLineEndingsAndFinalLine(t *testing.T) {
	result := MergeText(
		[]byte("one\r\ntwo\r\nthree"),
		[]byte("ONE\r\ntwo\r\nthree"),
		[]byte("one\r\ntwo\r\nTHREE"),
	)
	if result.Conflict {
		t.Fatalf("unexpected conflict: %s", result.Reason)
	}
	if got, want := string(result.Data), "ONE\r\ntwo\r\nTHREE"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMergeTextIdentities(t *testing.T) {
	base := []byte("base\n")
	changed := []byte("changed\n")
	for name, result := range map[string]TextResult{
		"local unchanged":    MergeText(base, base, changed),
		"upstream unchanged": MergeText(base, changed, base),
		"same change":        MergeText(base, changed, changed),
	} {
		if result.Conflict || !bytes.Equal(result.Data, changed) {
			t.Fatalf("%s: %+v", name, result)
		}
	}
}

func TestMergeTreeFailClosedCases(t *testing.T) {
	regular := func(text string) Entry {
		return Entry{Kind: KindRegular, Mode: 0o644, Data: []byte(text)}
	}
	executable := Entry{Kind: KindRegular, Mode: 0o755, Data: []byte("#!/bin/sh\n")}
	symlink := Entry{Kind: KindSymlink, Mode: fs.ModeSymlink | 0o777, Data: []byte("../outside")}
	binaryEntry := Entry{Kind: KindRegular, Mode: 0o644, Data: []byte{'a', 0, 'b'}}

	tests := []struct {
		name     string
		base     Tree
		local    Tree
		upstream Tree
		reason   Reason
	}{
		{
			name: "delete modify",
			base: Tree{"file": regular("base\n")}, local: Tree{},
			upstream: Tree{"file": regular("changed\n")}, reason: ReasonDeleteModify,
		},
		{
			name: "add add",
			base: Tree{}, local: Tree{"file": regular("ours\n")},
			upstream: Tree{"file": regular("theirs\n")}, reason: ReasonAddAdd,
		},
		{
			name: "symlink",
			base: Tree{}, local: Tree{}, upstream: Tree{"file": symlink}, reason: ReasonSymlink,
		},
		{
			name: "new executable",
			base: Tree{}, local: Tree{}, upstream: Tree{"file": executable}, reason: ReasonModeChanged,
		},
		{
			name: "binary",
			base: Tree{}, local: Tree{}, upstream: Tree{"file": binaryEntry}, reason: ReasonBinaryChanged,
		},
		{
			name: "mode changed",
			base: Tree{"file": regular("same\n")}, local: Tree{"file": regular("same\n")},
			upstream: Tree{"file": executable}, reason: ReasonModeChanged,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := MergeTree(&test.base, test.local, test.upstream)
			if result.Status != StatusConflict || !containsReason(result.Conflicts, test.reason) {
				t.Fatalf("got %+v", result)
			}
		})
	}
}

func TestMergeTreeNoBaselineIsFork(t *testing.T) {
	result := MergeTree(nil, Tree{}, Tree{})
	if result.Status != StatusFork || !containsReason(result.Conflicts, ReasonNoBaseline) {
		t.Fatalf("got %+v", result)
	}
}

func TestMergeTreeRejectsUnsafePaths(t *testing.T) {
	base := Tree{}
	entry := Entry{Kind: KindRegular, Mode: 0o644, Data: []byte("x")}
	for _, name := range []string{"../escape", "/absolute", "a/../b", `a\b`} {
		result := MergeTree(&base, Tree{}, Tree{name: entry})
		if result.Status != StatusConflict || !containsReason(result.Conflicts, ReasonInvalidPath) {
			t.Fatalf("path %q: %+v", name, result)
		}
	}
	result := MergeTree(&base, Tree{}, Tree{"file": entry, "file/child": entry})
	if result.Status != StatusConflict || !containsReason(result.Conflicts, ReasonPathCollision) {
		t.Fatalf("path collision: %+v", result)
	}
}

func TestMergeTreeDoesNotAliasInputData(t *testing.T) {
	base := Tree{}
	upstream := Tree{"file": {Kind: KindRegular, Mode: 0o644, Data: []byte("safe")}}
	result := MergeTree(&base, Tree{}, upstream)
	if result.Status != StatusMerged {
		t.Fatalf("got %+v", result)
	}
	upstream["file"].Data[0] = 'X'
	if got := string(result.Tree["file"].Data); got != "safe" {
		t.Fatalf("merged data aliased input: %q", got)
	}
}

func FuzzMergeTextIdentities(f *testing.F) {
	f.Add([]byte("a\nb\n"), []byte("A\nb\n"))
	f.Add([]byte{}, []byte("new"))
	f.Fuzz(func(t *testing.T, base, changed []byte) {
		if binary(base) || binary(changed) {
			t.Skip()
		}
		for _, result := range []TextResult{
			MergeText(base, base, changed),
			MergeText(base, changed, base),
			MergeText(base, changed, changed),
		} {
			if result.Conflict || !bytes.Equal(result.Data, changed) {
				t.Fatalf("identity violated: %+v", result)
			}
		}
	})
}

func containsReason(conflicts []Conflict, reason Reason) bool {
	for _, conflict := range conflicts {
		if conflict.Reason == reason {
			return true
		}
	}
	return false
}
