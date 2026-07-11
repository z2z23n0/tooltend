package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

var exactRuntimeVersion = regexp.MustCompile(`^[vV]?[0-9]+(?:\.[0-9]+){0,3}(?:[-+][0-9A-Za-z.-]+)?$`)

// EnqueueRuntimeMigrations records only exact, explicitly observed npm/Python
// runtimes. It is called from the confirmed init transaction boundary; a
// regular scan never opts an unmanaged native command into adoption.
func EnqueueRuntimeMigrations(ctx context.Context, database *store.Store, now time.Time) (int, error) {
	rows, err := database.DB().QueryContext(ctx, `
		SELECT b.id,b.install_path,b.config_path,b.config_pointer,b.observed_version,c.kind,s.kind,s.identity_hash
		FROM bindings b
		JOIN components c ON c.id=b.component_id
		JOIN sources s ON s.id=c.source_id
		WHERE b.managed=0 AND c.kind IN ('cli','stdio_mcp') AND s.kind IN ('npm','pypi')`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type candidate struct {
		bindingID, installPath, configPath, configPointer, version string
		componentKind                                              model.ComponentKind
		sourceKind                                                 model.SourceKind
		identity                                                   string
	}
	var candidates []candidate
	for rows.Next() {
		var value candidate
		if err := rows.Scan(&value.bindingID, &value.installPath, &value.configPath, &value.configPointer, &value.version, &value.componentKind, &value.sourceKind, &value.identity); err != nil {
			return 0, err
		}
		value.version = strings.TrimSpace(value.version)
		if !exactRuntimeVersion.MatchString(value.version) {
			continue
		}
		if value.componentKind == model.ComponentStdioMCP && (value.configPath == "" || value.configPointer == "") {
			continue
		}
		if value.componentKind == model.ComponentCLI && !filepath.IsAbs(value.installPath) {
			continue
		}
		candidates = append(candidates, value)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	inserted := 0
	for _, value := range candidates {
		key := strings.Join([]string{"adopt-runtime-v1", value.bindingID, value.version, value.identity}, "\x00")
		digest := sha256.Sum256([]byte(key))
		encoded := hex.EncodeToString(digest[:])
		ok, err := database.EnqueueTask(ctx, model.Task{
			ID: "task_" + encoded[:26], Kind: "adopt_runtime_auto", BindingID: value.bindingID,
			IdempotencyKey: "adopt-runtime:" + encoded, Status: model.TaskPending,
			NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			return inserted, err
		}
		if ok {
			inserted++
		}
	}
	return inserted, nil
}
