package metrics

import (
	"math"
	"testing"
)

// scipy 1.18: bootstrap((d,), np.mean, method="percentile", n_resamples=10000,
// rng=np.random.default_rng(0)) gives [0.013667, 0.109667]; seeds 0-5 span 0.0013.
func TestBootstrapMean_MatchesScipyPercentileInterval(t *testing.T) {
	differences := []float64{
		0.12, -0.05, 0.30, 0.08, -0.22, 0.15, 0.04, 0.27, -0.10, 0.18,
		0.02, 0.09, -0.15, 0.21, 0.06, -0.03, 0.25, 0.11, -0.08, 0.14,
		0.00, 0.19, -0.12, 0.07, 0.16, -0.01, 0.23, 0.05, -0.18, 0.10,
	}
	const wantMean, wantLow, wantHigh = 0.06266666666666668, 0.013666666666666667, 0.10966666666666666

	got := BootstrapMean(differences, 42)

	if math.Abs(got.Mean-wantMean) > 1e-12 {
		t.Errorf("mean = %v, want %v", got.Mean, wantMean)
	}
	if math.Abs(got.Low-wantLow) > 0.005 || math.Abs(got.High-wantHigh) > 0.005 {
		t.Errorf("interval = [%v, %v], want within 0.005 of [%v, %v]", got.Low, got.High, wantLow, wantHigh)
	}
}

func TestBootstrapMean_ConstantDifferenceHasNoSpread(t *testing.T) {
	differences := []float64{0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1}

	got := BootstrapMean(differences, 42)

	for name, v := range map[string]float64{"mean": got.Mean, "low": got.Low, "high": got.High} {
		if math.Abs(v-0.1) > 1e-12 {
			t.Errorf("%s = %v, want 0.1", name, v)
		}
	}
}
