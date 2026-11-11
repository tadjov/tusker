package suite

import (
	"fmt"

	"github.com/tadjov/tusker/internal/token"
)

// BooleanKind names the node a BooleanExpr is.
type BooleanKind int

const (
	BooleanOperand BooleanKind = iota
	BooleanAnd
	BooleanOr
	BooleanNot
)

// BooleanExpr is a boolean query's text parsed.
type BooleanExpr struct {
	Kind BooleanKind
	// Operand is a BooleanOperand's text, a word or a quoted phrase without its
	// quotes.
	Operand string
	// Operands hold an AND's or OR's operands, or a NOT's one.
	Operands []BooleanExpr
}

// ParseBoolean reads text with internal/token's grammar, as the native
// Postgres boolean parser reads it: AND, OR and NOT in any case, parentheses,
// quoted phrases, and AND between adjacent operands. NOT binds tighter than
// AND, and AND tighter than OR.
func ParseBoolean(text string) (BooleanExpr, error) {
	tokenizer := token.NewBoolTokenizer()
	tokens := tokenizer.Tokenize(text)
	if err := tokenizer.Validate(tokens); err != nil {
		return BooleanExpr{}, fmt.Errorf("boolean text %q: %w", text, err)
	}
	p := &booleanParser{tokens: tokens}
	expr, err := p.or()
	if err == nil && p.peek().Type != token.EOF {
		err = fmt.Errorf("unexpected %q", p.peek().Value)
	}
	if err != nil {
		return BooleanExpr{}, fmt.Errorf("boolean text %q: %w", text, err)
	}
	return expr, nil
}

type booleanParser struct {
	tokens []token.Token
	pos    int
}

func (p *booleanParser) peek() token.Token { return p.tokens[p.pos] }

func (p *booleanParser) next() token.Token {
	tok := p.tokens[p.pos]
	if tok.Type != token.EOF {
		p.pos++
	}
	return tok
}

func (p *booleanParser) or() (BooleanExpr, error) {
	var operands []BooleanExpr
	for {
		operand, err := p.and()
		if err != nil {
			return BooleanExpr{}, err
		}
		operands = append(operands, operand)
		if p.peek().Type != token.OR {
			break
		}
		p.next()
	}
	if len(operands) == 1 {
		return operands[0], nil
	}
	return BooleanExpr{Kind: BooleanOr, Operands: operands}, nil
}

func (p *booleanParser) and() (BooleanExpr, error) {
	var operands []BooleanExpr
	for {
		operand, err := p.unary()
		if err != nil {
			return BooleanExpr{}, err
		}
		operands = append(operands, operand)
		switch p.peek().Type {
		case token.AND:
			p.next()
			continue
		case token.WORD, token.LPAREN, token.NOT:
			continue
		}
		break
	}
	if len(operands) == 1 {
		return operands[0], nil
	}
	return BooleanExpr{Kind: BooleanAnd, Operands: operands}, nil
}

func (p *booleanParser) unary() (BooleanExpr, error) {
	if p.peek().Type == token.NOT {
		p.next()
		operand, err := p.unary()
		if err != nil {
			return BooleanExpr{}, err
		}
		return BooleanExpr{Kind: BooleanNot, Operands: []BooleanExpr{operand}}, nil
	}
	return p.primary()
}

func (p *booleanParser) primary() (BooleanExpr, error) {
	tok := p.next()
	switch tok.Type {
	case token.WORD:
		return BooleanExpr{Kind: BooleanOperand, Operand: tok.Value}, nil
	case token.LPAREN:
		expr, err := p.or()
		if err != nil {
			return BooleanExpr{}, err
		}
		if p.next().Type != token.RPAREN {
			return BooleanExpr{}, fmt.Errorf("unclosed parenthesis")
		}
		return expr, nil
	case token.EOF:
		return BooleanExpr{}, fmt.Errorf("ends where an operand belongs")
	default:
		return BooleanExpr{}, fmt.Errorf("unexpected %q", tok.Value)
	}
}
