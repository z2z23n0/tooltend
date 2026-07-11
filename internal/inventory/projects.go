package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const maxAgentStateBytes = 16 << 20

type ProjectCandidate struct {
	Path       string    `json:"path"`
	Source     string    `json:"source"`
	LastActive time.Time `json:"last_active,omitempty"`
	Current    bool      `json:"current"`
}

// DiscoverProjects reads only the current directory and the two agents'
// bounded configuration/state files. It deliberately does not walk the home
// directory or inspect transcripts.
func DiscoverProjects(home, current string, limit int) []ProjectCandidate {
	if limit <= 0 {
		limit = 20
	}
	seen := map[string]ProjectCandidate{}
	add := func(raw, source string, isCurrent bool) {
		if strings.TrimSpace(raw) == "" {
			return
		}
		path, err := filepath.Abs(raw)
		if err != nil {
			return
		}
		path = filepath.Clean(path)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return
		}
		candidate := ProjectCandidate{Path: path, Source: source, LastActive: info.ModTime().UTC(), Current: isCurrent}
		if existing, ok := seen[path]; ok {
			candidate.Current = candidate.Current || existing.Current
			if existing.Current || existing.LastActive.After(candidate.LastActive) {
				candidate.LastActive = existing.LastActive
				candidate.Source = existing.Source
			}
		}
		seen[path] = candidate
	}
	add(current, "current", true)

	if document, err := readBoundedTOML(filepath.Join(home, ".codex", "config.toml")); err == nil {
		if projects, ok := document["projects"].(map[string]any); ok {
			for path := range projects {
				add(path, "codex_config", false)
			}
		}
	}
	if document, err := readBoundedJSON(filepath.Join(home, ".claude.json")); err == nil {
		if projects, ok := document["projects"].(map[string]any); ok {
			for path := range projects {
				add(path, "claude_state", false)
			}
		}
	}

	result := make([]ProjectCandidate, 0, len(seen))
	for _, candidate := range seen {
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Current != result[j].Current {
			return result[i].Current
		}
		if !result[i].LastActive.Equal(result[j].LastActive) {
			return result[i].LastActive.After(result[j].LastActive)
		}
		return result[i].Path < result[j].Path
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func readBoundedJSON(path string) (map[string]any, error) {
	data, err := readBounded(path)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func readBoundedTOML(path string) (map[string]any, error) {
	data, err := readBounded(path)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := toml.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAgentStateBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAgentStateBytes {
		return nil, errors.New("agent state file exceeds size limit")
	}
	return data, nil
}
