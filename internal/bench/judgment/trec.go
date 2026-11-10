package judgment

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/meta"
)

// WriteQrels exports a JudgmentFile in TREC qrels format:
//
//	query_id  0  doc_id  relevance_grade
//
// Unannotated entries (grade < 0) are skipped.
func WriteQrels(jf *File, path string) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create qrels file: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close qrels file: %w", closeErr)
		}
	}()

	w := bufio.NewWriter(f)
	for _, entry := range jf.Queries {
		for _, doc := range entry.Docs {
			if doc.Grade < 0 {
				continue
			}
			if _, err := fmt.Fprintf(w, "%s\t0\t%s\t%d\n", entry.QueryID, doc.DocID, doc.Grade); err != nil {
				return fmt.Errorf("write qrels file: %w", err)
			}
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write qrels file: %w", err)
	}
	return nil
}

// ReadQrels imports a TREC qrels file into a JudgmentFile.
// Format per line: query_id  0  doc_id  relevance_grade
// Lines starting with '#' and blank lines are ignored.
func ReadQrels(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open qrels file: %w", err)
	}
	defer f.Close()

	byQuery := make(map[string]*Entry)
	var order []string

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 4 {
			return nil, fmt.Errorf("qrels line %d: expected 4 fields, got %d", lineNum, len(fields))
		}

		qid := fields[0]
		docIDStr := fields[2]
		grade, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil, fmt.Errorf("qrels line %d: invalid grade %q", lineNum, fields[3])
		}

		docID, err := uuid.Parse(docIDStr)
		if err != nil {
			return nil, fmt.Errorf("qrels line %d: invalid doc UUID %q", lineNum, docIDStr)
		}

		if _, ok := byQuery[qid]; !ok {
			byQuery[qid] = &Entry{QueryID: qid}
			order = append(order, qid)
		}
		byQuery[qid].Docs = append(byQuery[qid].Docs, GradedDoc{DocID: docID, Grade: grade})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read qrels file: %w", err)
	}

	jf := &File{Meta: meta.Meta{Judge: &meta.Judge{Strategy: "trec-qrels"}}}
	for _, qid := range order {
		jf.Queries = append(jf.Queries, *byQuery[qid])
	}
	return jf, nil
}
