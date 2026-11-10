package metrics

import (
	"math"
	"testing"
)

// Expected values are sklearn.metrics.cohen_kappa_score(first, second,
// weights="quadratic", labels=[0, 1, 2, 3]). The labels fix the 0–3 scale;
// without them sklearn renumbers the grades seen, which differs when one is unused.
func TestGradeConfusion_QuadraticKappaMatchesSklearn(t *testing.T) {
	tests := []struct {
		name          string
		first, second []int
		want          float64
	}{
		{
			name:   "perfect agreement",
			first:  []int{0, 1, 2, 3, 3, 2, 1, 0},
			second: []int{0, 1, 2, 3, 3, 2, 1, 0},
			want:   1.0,
		},
		{
			name:   "grade 2 used by neither rater still counts on the scale",
			first:  []int{0, 0, 1, 1, 3, 3, 0, 1},
			second: []int{0, 1, 1, 0, 3, 1, 0, 3},
			want:   0.5402298850574713,
		},
		{
			name:   "partial agreement over every grade",
			first:  []int{0, 1, 2, 3, 2, 1, 0, 3, 2, 2},
			second: []int{0, 2, 2, 3, 1, 1, 1, 2, 3, 0},
			want:   0.5714285714285714,
		},
		{
			name:   "reversed scale",
			first:  []int{0, 1, 2, 3},
			second: []int{3, 2, 1, 0},
			want:   -1.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewGradeConfusion(tt.first, tt.second)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.QuadraticKappa(); math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("QuadraticKappa() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGradeConfusion_KappaIsUndefinedWithoutExpectedDisagreement(t *testing.T) {
	for name, grades := range map[string][2][]int{
		"no pairs":                  {nil, nil},
		"both raters use one grade": {{2, 2, 2}, {2, 2, 2}},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := NewGradeConfusion(grades[0], grades[1])
			if err != nil {
				t.Fatal(err)
			}
			if got := c.QuadraticKappa(); !math.IsNaN(got) {
				t.Errorf("QuadraticKappa() = %v, want NaN", got)
			}
		})
	}
}

func TestGradeConfusion_ExactAgreement(t *testing.T) {
	c, err := NewGradeConfusion([]int{2, 1, 0, 3, 3, 1}, []int{2, 2, 0, 3, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if c.Pairs() != 6 {
		t.Errorf("Pairs() = %d, want 6", c.Pairs())
	}
	if got := c.ExactAgreement(); math.Abs(got-4.0/6.0) > 1e-12 {
		t.Errorf("ExactAgreement() = %v, want %v", got, 4.0/6.0)
	}
	if c[3][1] != 1 {
		t.Errorf("first rater 3, second rater 1 counted %d times, want 1", c[3][1])
	}
}

func TestNewGradeConfusion_RejectsBadInput(t *testing.T) {
	for name, grades := range map[string][2][]int{
		"different lengths": {{0, 1}, {0}},
		"grade above 3":     {{4}, {0}},
		"unjudged grade":    {{0}, {-1}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewGradeConfusion(grades[0], grades[1]); err == nil {
				t.Error("want an error")
			}
		})
	}
}
