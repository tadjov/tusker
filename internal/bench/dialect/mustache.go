package dialect

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/tadjov/tusker/internal/bench/engine"
	"github.com/tadjov/tusker/internal/bench/suite"
)

// Mustache is Elasticsearch: suite templates render on the engine, other blocks are sent as written.
type Mustache struct{}

var mustacheTag = regexp.MustCompile(`\{\{\{?\s*([#^/!>&]?)\s*([^}]*?)\s*\}?\}\}`)

func (Mustache) Name() string { return "mustache" }

func (Mustache) StoresTemplates() bool { return true }

func (Mustache) Check(block suite.Block) error {
	names := readMustache(block.Statement)
	if block.Template == "" {
		if len(names.all) > 0 {
			return fmt.Errorf("reads %v, but only a suite template is rendered as a search template", names.all)
		}
		if len(block.Args) > 0 {
			return fmt.Errorf("args %v: only a suite template receives params", block.Args)
		}
		return nil
	}
	for _, name := range names.topLevel {
		if !slices.Contains(block.Args, name) {
			return fmt.Errorf("template %q reads {{%s}} but args leaves it out: %v", block.Template, name, block.Args)
		}
	}
	for _, arg := range block.Args {
		if !slices.Contains(names.all, arg) {
			return fmt.Errorf("template %q args names %q but the source never reads it", block.Template, arg)
		}
	}
	return nil
}

func (Mustache) Request(track string, query *suite.ResolvedQuery) engine.Request {
	if query.Template == "" {
		return engine.Request{Query: query.Query}
	}
	return engine.Request{
		Query:            query.Query,
		SearchTemplateID: engine.SearchTemplateID(track, query.Template),
		Params:           query.Params,
	}
}

// mustacheNames splits out topLevel names, since one inside a section may be a list element's field.
type mustacheNames struct {
	topLevel []string
	all      []string
}

// readMustache follows Elasticsearch's lambdas: toJson and join read the param their body names.
func readMustache(source string) mustacheNames {
	var names mustacheNames
	var openSections []string
	insideSection := func() bool {
		return slices.ContainsFunc(openSections, func(s string) bool { return s != "toJson" && s != "join" && s != "url" })
	}
	record := func(name string) {
		name, _, _ = strings.Cut(strings.TrimSpace(name), ".")
		if name == "" {
			return
		}
		names.all = append(names.all, name)
		if !insideSection() {
			names.topLevel = append(names.topLevel, name)
		}
	}

	tags := mustacheTag.FindAllStringSubmatchIndex(source, -1)
	for i, tag := range tags {
		sigil := source[tag[2]:tag[3]]
		name, _, _ := strings.Cut(source[tag[4]:tag[5]], " ")
		switch sigil {
		case "!", ">":
		case "/":
			if n := len(openSections); n > 0 {
				openSections = openSections[:n-1]
			}
		case "#", "^":
			switch name {
			case "toJson", "join":
				bodyEnd := len(source)
				if i+1 < len(tags) {
					bodyEnd = tags[i+1][0]
				}
				record(source[tag[1]:bodyEnd])
			case "url":
			default:
				record(name)
			}
			openSections = append(openSections, name)
		default:
			record(name)
		}
	}
	slices.Sort(names.topLevel)
	names.topLevel = slices.Compact(names.topLevel)
	slices.Sort(names.all)
	names.all = slices.Compact(names.all)
	return names
}
