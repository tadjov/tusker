package report

import "strings"

func repoRelPaths(paths []string) string {
	rel := make([]string, len(paths))
	for i, path := range paths {
		rel[i] = repoRelPath(path)
	}
	return strings.Join(rel, ", ")
}
