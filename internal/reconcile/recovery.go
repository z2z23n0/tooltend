package reconcile

import (
	"context"
	"fmt"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/lifecycle"
	"github.com/z2z23n0/tooltend/internal/store"
)

type RecoveryFunc func(context.Context, *store.Store, config.Paths) (int, error)

func RecoverActivations(ctx context.Context, database *store.Store, paths config.Paths) (int, error) {
	adoptions, err := lifecycle.RecoverAdoptions(ctx, database, paths)
	if err != nil {
		return 0, fmt.Errorf("recover adoptions: %w", err)
	}
	activations, err := lifecycle.RecoverActivations(ctx, database, paths)
	if err != nil {
		return adoptions.Total(), fmt.Errorf("recover activations: %w", err)
	}
	return adoptions.Total() + activations, nil
}
