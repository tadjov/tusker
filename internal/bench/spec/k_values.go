package spec

import "fmt"

// CheckKValues rejects a cut-off that is not positive, is repeated, or lies
// deeper than poolDepth, where docs past the pool were never judged. A zero
// poolDepth bounds nothing.
func CheckKValues(kValues []int, poolDepth int) error {
	seen := make(map[int]bool, len(kValues))
	for _, k := range kValues {
		switch {
		case k <= 0:
			return fmt.Errorf("cut-off %d is not positive", k)
		case seen[k]:
			return fmt.Errorf("cut-off %d is repeated", k)
		case poolDepth > 0 && k > poolDepth:
			return fmt.Errorf("cut-off %d is deeper than the pool depth %d", k, poolDepth)
		}
		seen[k] = true
	}
	return nil
}
