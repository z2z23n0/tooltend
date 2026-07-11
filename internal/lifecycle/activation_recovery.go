package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/z2z23n0/tooltend/internal/activation"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

// RecoverActivations converges all non-terminal pointer journals. Callers must
// hold activation.lock so recovery cannot race a new lifecycle mutation.
func RecoverActivations(ctx context.Context, database *store.Store, paths config.Paths) (int, error) {
	sqlStore, err := activation.NewSQLStore(database)
	if err != nil {
		return 0, err
	}
	pending, err := sqlStore.Pending(ctx)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}
	shapes, err := lifecycleBindingShapes(ctx, database)
	if err != nil {
		return 0, err
	}
	seen := make(map[string]struct{})
	recovered := 0
	for _, intent := range pending {
		if _, ok := seen[intent.BindingID]; ok {
			continue
		}
		seen[intent.BindingID] = struct{}{}
		filtered := &bindingActivationStore{Store: sqlStore, bindingID: intent.BindingID}
		shape := shapes[intent.BindingID]
		root := filepath.Join(paths.GenerationsDir, intent.BindingID)
		manager := activation.Manager{Root: root, Store: filtered}
		if shape.runtime() {
			root = filepath.Join(paths.RuntimesDir, intent.BindingID)
			manager.Root = root
			manager.Hash = activation.HashRuntimeGeneration
		}
		results, recoverErr := manager.Recover(ctx)
		if recoverErr != nil {
			return recovered, fmt.Errorf("recover binding %s: %w", intent.BindingID, recoverErr)
		}
		recovered += len(results)
	}
	return recovered, nil
}

type lifecycleBindingShape struct {
	component model.ComponentKind
	source    model.SourceKind
}

func (s lifecycleBindingShape) runtime() bool {
	packageSource := s.source == model.SourceNPM || s.source == model.SourcePyPI
	return packageSource && (s.component == model.ComponentCLI || s.component == model.ComponentStdioMCP)
}

func lifecycleBindingShapes(ctx context.Context, database *store.Store) (map[string]lifecycleBindingShape, error) {
	rows, err := database.DB().QueryContext(ctx, `SELECT b.id,c.kind,s.kind FROM bindings b JOIN components c ON c.id=b.component_id JOIN sources s ON s.id=c.source_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]lifecycleBindingShape)
	for rows.Next() {
		var bindingID string
		var shape lifecycleBindingShape
		if err := rows.Scan(&bindingID, &shape.component, &shape.source); err != nil {
			return nil, err
		}
		result[bindingID] = shape
	}
	return result, rows.Err()
}

type bindingActivationStore struct {
	activation.Store
	bindingID string
}

func (s *bindingActivationStore) Pending(ctx context.Context) ([]activation.Intent, error) {
	all, err := s.Store.Pending(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]activation.Intent, 0, len(all))
	for _, intent := range all {
		if intent.BindingID == s.bindingID {
			result = append(result, intent)
		}
	}
	return result, nil
}
