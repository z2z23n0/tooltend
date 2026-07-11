package merge

import (
	"slices"
	"sort"
)

const maxDiffCells = 4_000_000

// TextResult is a conflict-free textual merge or a fail-closed reason.
type TextResult struct {
	Data     []byte `json:"data,omitempty"`
	Conflict bool   `json:"conflict"`
	Reason   Reason `json:"reason,omitempty"`
}

// MergeText merges line-preserving text. Only identical edits or edits with
// unambiguous, non-touching base ranges are combined automatically.
func MergeText(base, local, upstream []byte) TextResult {
	if binary(base) || binary(local) || binary(upstream) {
		if slices.Equal(base, local) && slices.Equal(base, upstream) {
			return TextResult{Data: slices.Clone(base)}
		}
		return TextResult{Conflict: true, Reason: ReasonBinaryChanged}
	}
	if slices.Equal(local, upstream) {
		return TextResult{Data: slices.Clone(local)}
	}
	if slices.Equal(base, local) {
		return TextResult{Data: slices.Clone(upstream)}
	}
	if slices.Equal(base, upstream) {
		return TextResult{Data: slices.Clone(local)}
	}
	baseLines := splitLines(base)
	localLines := splitLines(local)
	upstreamLines := splitLines(upstream)
	localEdits, ok := edits(baseLines, localLines)
	if !ok {
		return TextResult{Conflict: true, Reason: ReasonTextTooComplex}
	}
	upstreamEdits, ok := edits(baseLines, upstreamLines)
	if !ok {
		return TextResult{Conflict: true, Reason: ReasonTextTooComplex}
	}

	combined := append(append([]edit(nil), localEdits...), upstreamEdits...)
	for _, left := range localEdits {
		for _, right := range upstreamEdits {
			if editEqual(left, right) {
				continue
			}
			if editsTouch(left, right) {
				return TextResult{Conflict: true, Reason: ReasonTextOverlap}
			}
		}
	}

	sort.SliceStable(combined, func(i, j int) bool {
		if combined[i].start == combined[j].start {
			return combined[i].end < combined[j].end
		}
		return combined[i].start < combined[j].start
	})
	combined = deduplicateEdits(combined)

	var output []string
	position := 0
	for _, change := range combined {
		if change.start < position {
			return TextResult{Conflict: true, Reason: ReasonTextOverlap}
		}
		output = append(output, baseLines[position:change.start]...)
		output = append(output, change.replacement...)
		position = change.end
	}
	output = append(output, baseLines[position:]...)
	return TextResult{Data: joinLines(output)}
}

type edit struct {
	start       int
	end         int
	replacement []string
}

func edits(base, target []string) ([]edit, bool) {
	if len(base)+1 > maxDiffCells/(len(target)+1) {
		return nil, false
	}
	width := len(target) + 1
	dp := make([]int, (len(base)+1)*width)
	for i := len(base) - 1; i >= 0; i-- {
		for j := len(target) - 1; j >= 0; j-- {
			at := i*width + j
			if base[i] == target[j] {
				dp[at] = 1 + dp[(i+1)*width+j+1]
			} else if dp[(i+1)*width+j] >= dp[i*width+j+1] {
				dp[at] = dp[(i+1)*width+j]
			} else {
				dp[at] = dp[i*width+j+1]
			}
		}
	}

	var result []edit
	i, j := 0, 0
	for i < len(base) || j < len(target) {
		if i < len(base) && j < len(target) && base[i] == target[j] {
			i++
			j++
			continue
		}
		start := i
		var replacement []string
		for i < len(base) || j < len(target) {
			if i < len(base) && j < len(target) && base[i] == target[j] {
				break
			}
			if j < len(target) && (i == len(base) || dp[i*width+j+1] > dp[(i+1)*width+j]) {
				replacement = append(replacement, target[j])
				j++
			} else if i < len(base) {
				i++
			}
		}
		result = append(result, edit{start: start, end: i, replacement: replacement})
	}
	return result, true
}

func editsTouch(left, right edit) bool {
	leftInsert := left.start == left.end
	rightInsert := right.start == right.end
	switch {
	case leftInsert && rightInsert:
		return left.start == right.start
	case leftInsert:
		return left.start >= right.start && left.start <= right.end
	case rightInsert:
		return right.start >= left.start && right.start <= left.end
	default:
		return left.start < right.end && right.start < left.end
	}
}

func editEqual(left, right edit) bool {
	return left.start == right.start && left.end == right.end && slices.Equal(left.replacement, right.replacement)
}

func deduplicateEdits(input []edit) []edit {
	result := make([]edit, 0, len(input))
	for _, candidate := range input {
		if len(result) == 0 || !editEqual(result[len(result)-1], candidate) {
			result = append(result, candidate)
		}
	}
	return result
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := make([]string, 0, 16)
	start := 0
	for i, value := range data {
		if value == '\n' {
			lines = append(lines, string(data[start:i+1]))
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	return lines
}

func joinLines(lines []string) []byte {
	size := 0
	for _, line := range lines {
		size += len(line)
	}
	result := make([]byte, 0, size)
	for _, line := range lines {
		result = append(result, line...)
	}
	return result
}
