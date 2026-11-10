package main

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/DjordjeVuckovic/tusker/internal/bench/judgment"
	"github.com/DjordjeVuckovic/tusker/internal/bench/metrics"
	"github.com/DjordjeVuckovic/tusker/internal/bench/pool"
	"github.com/DjordjeVuckovic/tusker/internal/bench/trackctx"
	"github.com/DjordjeVuckovic/tusker/internal/storage"
	"github.com/DjordjeVuckovic/tusker/internal/types/document"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const contentPreviewRunes = 800

type gradeFlags struct {
	against string
	sample  int
	seed    uint64
	pg      string
	report  bool
}

type gradeCommand struct {
	flags      gradeFlags
	openReader articleReaderOpener
}

func newGradeCmd() *cobra.Command { return newGradeCmdWithReader(openArticleReader) }

func newGradeCmdWithReader(openReader articleReaderOpener) *cobra.Command {
	var f gradeFlags
	cmd := &cobra.Command{
		Use:   "grade [track] --against <strategy|path>",
		Short: "Grade pooled docs by hand and measure a judge's agreement",
		Long: `Walks the track's pool one (query, doc) pair at a time and writes each hand
grade to trec/annotations.manual.yaml, the file bench judge --strategy manual
creates. The file is created or merged first, so hand grades survive a re-pool.

Each screen shows the query and the article. The --against judge's grade and
the engines that retrieved the doc stay hidden. Answer with one line:
0-3 grades, s skips, u undoes the last grade, q quits. Every grade is saved
before the next pair shows, so a rerun resumes where the last session stopped.

--sample N walks N pairs drawn from the pairs the --against judge graded, an
equal share per grade (a grade with fewer pairs gives the rest to the others).
The same pool, judge file and --seed give the same sample and order in every
session. Without --sample every pool pair still ungraded is walked.

A session never shows the judge's grades, so it ends with progress only. Once
the walk is graded, --report prints the agreement over the pairs graded both by
hand and by the --against judge: the exact-agreement rate, quadratic-weighted
Cohen's kappa and the 4x4 table of hand grade against judge grade.`,
		Example: `  bench grade tracks/cc-news/fts --against bm25 --sample 150 --pg "$PG_CONNECTION_STRING"
  bench grade tracks/cc-news/fts --against bm25 --report`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return gradeCommand{flags: f, openReader: openReader}.execute(cmd, args)
		},
	}
	cmd.Flags().StringVar(&f.against, "against", "", "Judgment set or annotations path to compare hand grades with")
	cmd.Flags().IntVar(&f.sample, "sample", 0, "Walk a stratified sample of N pairs (0 = every ungraded pair)")
	cmd.Flags().Uint64Var(&f.seed, "seed", 1, "Seed for the sample and the order of docs within a query")
	cmd.Flags().StringVar(&f.pg, "pg", "", "Postgres connection for the articles (or set PG_CONNECTION_STRING)")
	cmd.Flags().BoolVar(&f.report, "report", false, "Print the agreement without starting a session")
	_ = cmd.MarkFlagRequired("against")
	return cmd
}

func (g gradeCommand) execute(cmd *cobra.Command, args []string) error {
	if g.flags.sample < 0 {
		return fmt.Errorf("--sample must be 0 or more, got %d", g.flags.sample)
	}
	tr, err := trackctx.Resolve(trackInputs("", args))
	if err != nil {
		return err
	}
	pf, err := pool.ReadPoolFile(tr.Pool)
	if err != nil {
		return fmt.Errorf("read pool: %w", err)
	}
	againstPath := tr.JudgmentsPath(g.flags.against)
	against, err := judgment.ReadFile(againstPath)
	if err != nil {
		return fmt.Errorf("read --against judgments: %w", err)
	}
	if err := checkPoolBinding(against.Meta.PoolRunID, pf); err != nil {
		return fmt.Errorf("--against %s: %w", againstPath, err)
	}

	manualPath := tr.JudgmentsPath(judgment.JudgmentSetName(judgment.StrategyManual, ""))
	if g.flags.report {
		manual, err := judgment.ReadFileIfExists(manualPath)
		if err != nil {
			return fmt.Errorf("read manual judgments: %w", err)
		}
		if manual == nil {
			return fmt.Errorf("no hand grades at %s yet: run bench grade without --report first", manualPath)
		}
		return agreement{hand: manual, judge: against, judgeName: g.flags.against}.print(cmd.OutOrStdout())
	}
	manual, err := mergeManualJudgments(manualJudgments{pool: pf, poolPath: tr.Pool, path: manualPath})
	if err != nil {
		return err
	}
	session := &gradingSession{
		out:        cmd.OutOrStdout(),
		answers:    readLines(cmd.InOrStdin()),
		manual:     manual,
		manualPath: manualPath,
		hand:       indexGrades(manual),
	}
	session.walk = gradingWalk(pf, indexGrades(against), g.flags.sample, g.flags.seed)
	if session.remaining() > 0 {
		if session.reader, err = g.openReader(cmd, g.flags.pg); err != nil {
			return err
		}
	}
	if err := session.run(cmd.Context()); err != nil {
		return err
	}
	// The agreement would reveal the judge's grades mid-sample, so a session
	// reports progress only.
	w := cmd.OutOrStdout()
	printDone(w, "Hand grades saved: "+manualPath)
	walked := len(session.walk)
	if left := session.remaining(); left > 0 {
		fmt.Fprintf(w, "graded %d of %d; rerun to continue\n", walked-left, walked)
	} else {
		fmt.Fprintf(w, "all %d graded; run bench grade --against %s --report for the agreement\n", walked, g.flags.against)
	}
	return nil
}

// agreement compares hand grades with a judge's over the pairs both graded.
type agreement struct {
	hand      *judgment.File
	judge     *judgment.File
	judgeName string
}

func (a agreement) print(w io.Writer) error {
	handGrades := indexGrades(a.hand)
	var hand, judge []int
	for _, entry := range a.judge.Queries {
		for _, doc := range entry.Docs {
			handDoc, ok := handGrades[pairKey{queryID: entry.QueryID, docID: doc.DocID}]
			if !ok || handDoc.Grade == judgment.GradeUnjudged || doc.Grade == judgment.GradeUnjudged {
				continue
			}
			hand = append(hand, handDoc.Grade)
			judge = append(judge, doc.Grade)
		}
	}
	confusion, err := metrics.NewGradeConfusion(hand, judge)
	if err != nil {
		return fmt.Errorf("compare hand grades with %s: %w", a.judgeName, err)
	}

	fmt.Fprintf(w, "\n%s\n", cBold.Sprintf("Agreement with %s over %d pairs graded in both", a.judgeName, confusion.Pairs()))
	fmt.Fprintf(w, "  exact agreement  %s\n", formatRatio(confusion.ExactAgreement(), "%.1f%%", 100))
	fmt.Fprintf(w, "  quadratic κ      %s\n\n", formatRatio(confusion.QuadraticKappa(), "%.3f", 1))
	fmt.Fprintf(w, "  %-8s", "")
	for grade := range confusion {
		fmt.Fprintf(w, "  judge %d", grade)
	}
	fmt.Fprintln(w)
	for grade, row := range confusion {
		fmt.Fprintf(w, "  %-8s", fmt.Sprintf("hand %d", grade))
		for _, count := range row {
			fmt.Fprintf(w, "  %7d", count)
		}
		fmt.Fprintln(w)
	}
	return nil
}

func formatRatio(value float64, format string, scale float64) string {
	if math.IsNaN(value) {
		return "n/a"
	}
	return fmt.Sprintf(format, value*scale)
}

func mergeManualJudgments(m manualJudgments) (*judgment.File, error) {
	existing, err := judgment.ReadFileIfExists(m.path)
	if err != nil {
		return nil, fmt.Errorf("read existing manual judgments: %w", err)
	}
	if existing != nil && existing.Judge().Strategy != string(judgment.StrategyManual) {
		return nil, fmt.Errorf("%s was graded by %q, not by hand: merging would relabel its grades as manual",
			m.path, existing.Judge())
	}
	return m.write(existing)
}

type pairKey struct {
	queryID string
	docID   uuid.UUID
}

type gradingPair struct {
	query *pool.PoolEntry
	docID uuid.UUID
}

func (p gradingPair) key() pairKey { return pairKey{queryID: p.query.QueryID, docID: p.docID} }

// indexGrades points at each doc's grade inside jf, so setting one edits jf.
func indexGrades(jf *judgment.File) map[pairKey]*judgment.GradedDoc {
	grades := make(map[pairKey]*judgment.GradedDoc)
	for i := range jf.Queries {
		for j := range jf.Queries[i].Docs {
			doc := &jf.Queries[i].Docs[j]
			grades[pairKey{queryID: jf.Queries[i].QueryID, docID: doc.DocID}] = doc
		}
	}
	return grades
}

// gradingWalk orders the pairs a session visits: grouped by query, docs
// shuffled within each query. It depends on neither the pool's order nor hand
// grades, so the walk stays the same while a grader works through it.
func gradingWalk(pf *pool.PoolFile, judgeGrades map[pairKey]*judgment.GradedDoc, sampleSize int, seed uint64) []gradingPair {
	rng := rand.New(rand.NewPCG(seed, 0))
	var pairs []gradingPair
	for i := range pf.Queries {
		for _, doc := range pf.Queries[i].Docs {
			pairs = append(pairs, gradingPair{query: &pf.Queries[i], docID: doc.DocID})
		}
	}
	slices.SortFunc(pairs, func(a, b gradingPair) int {
		return cmp.Or(cmp.Compare(a.query.QueryID, b.query.QueryID), cmp.Compare(a.docID.String(), b.docID.String()))
	})
	if sampleSize > 0 {
		pairs = stratifiedSample(pairs, judgeGrades, sampleSize, rng)
	}

	byQuery := make(map[string][]gradingPair)
	for _, p := range pairs {
		byQuery[p.query.QueryID] = append(byQuery[p.query.QueryID], p)
	}
	walk := make([]gradingPair, 0, len(pairs))
	for _, queryID := range slices.Sorted(maps.Keys(byQuery)) {
		docs := byQuery[queryID]
		slices.SortFunc(docs, func(a, b gradingPair) int { return cmp.Compare(a.docID.String(), b.docID.String()) })
		rng.Shuffle(len(docs), func(i, j int) { docs[i], docs[j] = docs[j], docs[i] })
		walk = append(walk, docs...)
	}
	return walk
}

// stratifiedSample draws size pairs the judge graded, an equal share per
// grade. Strata are filled smallest first, so a grade with fewer pairs than
// its share gives the shortfall to the grades after it.
func stratifiedSample(pairs []gradingPair, judgeGrades map[pairKey]*judgment.GradedDoc, size int, rng *rand.Rand) []gradingPair {
	var strata [judgment.GradeHighly + 1][]gradingPair
	total := 0
	for _, p := range pairs {
		doc, ok := judgeGrades[p.key()]
		if !ok || doc.Grade < judgment.GradeNotRelev || doc.Grade > judgment.GradeHighly {
			continue
		}
		strata[doc.Grade] = append(strata[doc.Grade], p)
		total++
	}
	for _, stratum := range strata {
		rng.Shuffle(len(stratum), func(i, j int) { stratum[i], stratum[j] = stratum[j], stratum[i] })
	}
	bySize := make([]int, len(strata))
	for grade := range bySize {
		bySize[grade] = grade
	}
	slices.SortStableFunc(bySize, func(a, b int) int { return cmp.Compare(len(strata[a]), len(strata[b])) })

	remaining := min(size, total)
	sample := make([]gradingPair, 0, remaining)
	for i, grade := range bySize {
		take := min(len(strata[grade]), remaining/(len(bySize)-i))
		sample = append(sample, strata[grade][:take]...)
		remaining -= take
	}
	return sample
}

type gradingAction int

const (
	actionGrade gradingAction = iota
	actionSkip
	actionUndo
	actionQuit
)

type gradingSession struct {
	out        io.Writer
	answers    <-chan string
	reader     storage.Reader
	walk       []gradingPair
	manual     *judgment.File
	manualPath string
	hand       map[pairKey]*judgment.GradedDoc
}

func (s *gradingSession) ungraded(p gradingPair) bool {
	return s.hand[p.key()].Grade == judgment.GradeUnjudged
}

func (s *gradingSession) remaining() int {
	n := 0
	for _, p := range s.walk {
		if s.ungraded(p) {
			n++
		}
	}
	return n
}

func (s *gradingSession) run(ctx context.Context) error {
	var gradedPositions []int
	for position := 0; position < len(s.walk); {
		pair := s.walk[position]
		if !s.ungraded(pair) {
			position++
			continue
		}
		if err := s.show(ctx, position); err != nil {
			return err
		}
		action, grade := s.readAction(ctx, len(gradedPositions) > 0)
		switch action {
		case actionGrade:
			if err := s.setGrade(pair, grade); err != nil {
				return err
			}
			gradedPositions = append(gradedPositions, position)
			position++
		case actionSkip:
			position++
		case actionUndo:
			position = gradedPositions[len(gradedPositions)-1]
			gradedPositions = gradedPositions[:len(gradedPositions)-1]
			if err := s.setGrade(s.walk[position], judgment.GradeUnjudged); err != nil {
				return err
			}
		case actionQuit:
			return nil
		}
	}
	return nil
}

func (s *gradingSession) setGrade(p gradingPair, grade int) error {
	s.hand[p.key()].Grade = grade
	if err := judgment.WriteFile(s.manual, s.manualPath); err != nil {
		return fmt.Errorf("save hand grade: %w", err)
	}
	return nil
}

// readLines sends each line of r, closing the channel at end of input. A
// session that ends early leaves the goroutine behind; it holds only stdin.
func readLines(r io.Reader) <-chan string {
	lines := make(chan string)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	return lines
}

// readAction reads answers until one is valid. End of input or an interrupt
// quits; every grade is already saved.
func (s *gradingSession) readAction(ctx context.Context, canUndo bool) (gradingAction, int) {
	for {
		fmt.Fprint(s.out, cBold.Sprint("grade 0-3 · s skip · u undo · q quit > "))
		var line string
		select {
		case <-ctx.Done():
			fmt.Fprintln(s.out)
			return actionQuit, 0
		case answer, ok := <-s.answers:
			if !ok {
				fmt.Fprintln(s.out)
				return actionQuit, 0
			}
			line = answer
		}
		switch answer := strings.TrimSpace(line); answer {
		case "0", "1", "2", "3":
			return actionGrade, int(answer[0] - '0')
		case "s":
			return actionSkip, 0
		case "q":
			return actionQuit, 0
		case "u":
			if canUndo {
				return actionUndo, 0
			}
			printWarn(s.out, "nothing to undo in this session")
		default:
			printWarn(s.out, fmt.Sprintf("%q is not 0-3, s, u or q", answer))
		}
	}
}

func (s *gradingSession) show(ctx context.Context, position int) error {
	pair := s.walk[position]
	articles, err := s.reader.GetByIDs(ctx, []uuid.UUID{pair.docID})
	if err != nil {
		return fmt.Errorf("read article %s: %w", pair.docID, err)
	}

	w := s.out
	header := fmt.Sprintf("── %d/%d ── %s", position+1, len(s.walk), pair.query.QueryID)
	if pair.query.Category != "" {
		header += " · " + pair.query.Category
	}
	fmt.Fprintf(w, "\n%s\n", cCyan.Sprint(header))
	printField(w, "query", pair.query.QueryDesc)
	if len(articles) == 0 {
		printWarn(w, fmt.Sprintf("article %s is not in the store", pair.docID))
		return nil
	}
	a := articles[0]
	printField(w, "title", a.Title)
	printField(w, "publisher", publisher(a))
	if !a.PublishedAt.IsZero() {
		printField(w, "published", a.PublishedAt.Format("2006-01-02"))
	}
	printField(w, "description", a.Description)
	printField(w, "content", contentPreview(a.Content))
	return nil
}

func printField(w io.Writer, label, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(w, "%s %s\n", cDim.Sprintf("%-12s", label+":"), value)
}

func publisher(a document.Article) string {
	if a.Metadata.PublisherName != "" {
		return a.Metadata.PublisherName
	}
	return a.PublisherDomain
}

func contentPreview(content string) string {
	runes := []rune(strings.Join(strings.Fields(content), " "))
	if len(runes) <= contentPreviewRunes {
		return string(runes)
	}
	return string(runes[:contentPreviewRunes]) + "…"
}
