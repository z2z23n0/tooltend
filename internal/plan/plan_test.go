package plan

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestDryRunDoesNotApply(t *testing.T) {
	called := false
	p := Plan{ID: "p", Title: "preview", Operations: []Operation{FuncOperation{
		Description: OperationPreview{ID: "op", Kind: OperationWriteFile, Summary: "write", RequiresConfirmation: true},
		ApplyFunc:   func(context.Context) error { called = true; return nil },
	}}}
	if _, err := p.Apply(context.Background(), ApplyOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("dry run applied operation")
	}
}

func TestRollbackInReverseOrder(t *testing.T) {
	var calls []string
	makeOp := func(id string, fail bool) FuncOperation {
		return FuncOperation{
			Description: OperationPreview{ID: id, Kind: OperationOther, Summary: id, Reversible: !fail},
			ApplyFunc: func(context.Context) error {
				calls = append(calls, "apply:"+id)
				if fail {
					return errors.New("boom")
				}
				return nil
			},
			RollbackFunc: func(context.Context) error { calls = append(calls, "rollback:"+id); return nil },
		}
	}
	p := Plan{ID: "p", Title: "rollback", Operations: []Operation{makeOp("one", false), makeOp("two", false), makeOp("bad", true)}}
	_, err := p.Apply(context.Background(), ApplyOptions{Confirmed: true})
	if err == nil {
		t.Fatal("expected failure")
	}
	want := []string{"apply:one", "apply:two", "apply:bad", "rollback:two", "rollback:one"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v", calls)
	}
}
