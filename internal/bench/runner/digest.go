package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/DjordjeVuckovic/tusker/internal/bench/engine"
)

// requestContent is the part of a request that decides what an engine answers.
// Query already holds a stored template's source, so its id is left out.
type requestContent struct {
	Query  string         `json:"query"`
	Args   []any          `json:"args"`
	Params map[string]any `json:"params"`
}

// requestDigest is the sha256 of the request's content, map keys sorted.
func requestDigest(request engine.Request) (string, error) {
	encoded, err := json.Marshal(requestContent{Query: request.Query, Args: request.Args, Params: request.Params})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
