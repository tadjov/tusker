package metrics

import (
	"math"
	"math/rand/v2"
	"slices"
)

// Interval is a sample mean and its 95% bootstrap confidence interval.
type Interval struct {
	Mean, Low, High float64
}

const bootstrapResamples = 10000

// BootstrapMean returns the mean of values with its 95% percentile bootstrap
// interval over 10,000 resamples drawn from a source seeded with seed, so one
// seed always gives the same interval. Percentiles interpolate linearly, as
// numpy.percentile does. values must not be empty.
func BootstrapMean(values []float64, seed uint64) Interval {
	random := rand.New(rand.NewPCG(seed, seed))
	resampledMeans := make([]float64, bootstrapResamples)
	for r := range resampledMeans {
		var sum float64
		for range values {
			sum += values[random.IntN(len(values))]
		}
		resampledMeans[r] = sum / float64(len(values))
	}
	slices.Sort(resampledMeans)
	return Interval{
		Mean: mean(values),
		Low:  percentile(resampledMeans, 0.025),
		High: percentile(resampledMeans, 0.975),
	}
}

func mean(values []float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func percentile(sorted []float64, q float64) float64 {
	position := q * float64(len(sorted)-1)
	below := int(math.Floor(position))
	return sorted[below] + (position-float64(below))*(sorted[below+1]-sorted[below])
}
