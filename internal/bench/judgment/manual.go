package judgment

import (
	"context"

	"github.com/tadjov/tusker/internal/bench/meta"
)

// ManualStrategy emits GradeUnjudged (-1) for every doc. Useful when a human
// wants to grade from scratch — the runner writes a JudgmentFile with
// placeholders the user can fill in by hand. Pairs with `bench show
// judgments` to review what's still unjudged.
type ManualStrategy struct{}

func NewManualStrategy() *ManualStrategy { return &ManualStrategy{} }

func (ManualStrategy) Name() string { return string(StrategyManual) }

func (ManualStrategy) Describe() meta.Judge {
	return meta.Judge{Strategy: string(StrategyManual)}
}

func (ManualStrategy) Grade(_ context.Context, _ GradingQuery, _ GradingDoc) (int, error) {
	return GradeUnjudged, nil
}
