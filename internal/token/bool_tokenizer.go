package token

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/tadjov/tusker/internal/apperr"
)

type BoolTokenizer struct {
	input []rune
	pos   int
}

func NewBoolTokenizer() *BoolTokenizer {
	return &BoolTokenizer{}
}

// Tokenize converts the input string into a slice of Tokens.
// Example: Input: `(apple AND "banana split") OR NOT cherry`
func (t *BoolTokenizer) Tokenize(input string) []Token {
	t.input = []rune(strings.Trim(input, " "))
	t.pos = 0

	var tokens []Token

	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		switch {
		case ch == '(':
			tokens = append(tokens, Token{Type: LPAREN, Value: "("})
			t.pos++
		case ch == ')':
			tokens = append(tokens, Token{Type: RPAREN, Value: ")"})
			t.pos++
		case ch == '"':
			tokens = append(tokens, t.readQuoted())
		case isWordChar(ch):
			tokens = append(tokens, t.readWord())
		default:
			t.pos++
		}
		t.skipWhitespace()
	}

	tokens = append(tokens, Token{Type: EOF})
	return tokens
}

func (t *BoolTokenizer) skipWhitespace() {
	for t.pos < len(t.input) && unicode.IsSpace(t.input[t.pos]) {
		t.pos++
	}
}

func (t *BoolTokenizer) readWord() Token {
	start := t.pos
	for t.pos < len(t.input) && isWordChar(t.input[t.pos]) {
		t.pos++
	}

	word := string(t.input[start:t.pos])

	switch strings.ToUpper(word) {
	case "AND":
		return Token{Type: AND, Value: word}
	case "OR":
		return Token{Type: OR, Value: word}
	case "NOT":
		return Token{Type: NOT, Value: word}
	default:
		return Token{Type: WORD, Value: word}
	}
}

func (t *BoolTokenizer) readQuoted() Token {
	t.pos++ // skip opening quote
	start := t.pos
	for t.pos < len(t.input) && t.input[t.pos] != '"' {
		t.pos++
	}
	value := string(t.input[start:t.pos])
	if t.pos < len(t.input) {
		t.pos++ // skip closing quote
	}
	return Token{Type: WORD, Value: value}
}

func isWordChar(ch rune) bool {
	return unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_'
}

func (t *BoolTokenizer) Validate(tokens []Token) error {
	depth := 0
	hasWord := false

	for i, tok := range tokens {
		if tok.Type == EOF {
			break
		}

		switch tok.Type {
		case WORD:
			hasWord = true
		case LPAREN:
			depth++
			if i+1 < len(tokens) && tokens[i+1].Type == RPAREN {
				return apperr.NewValidation("empty parentheses")
			}
		case RPAREN:
			depth--
			if depth < 0 {
				return apperr.NewValidation("unexpected closing parenthesis")
			}
		case AND, OR:
			if i == 0 {
				return apperr.NewValidation(fmt.Sprintf("expression cannot start with %s", tok.Value))
			}
			prev := tokens[i-1].Type
			if prev != WORD && prev != RPAREN {
				return apperr.NewValidation(fmt.Sprintf("unexpected %s operator", tok.Value))
			}
		case NOT:
			if i+1 >= len(tokens) || (tokens[i+1].Type != WORD && tokens[i+1].Type != LPAREN && tokens[i+1].Type != NOT) {
				return apperr.NewValidation("NOT must be followed by a term or group")
			}
		default:
			return apperr.NewValidation(fmt.Sprintf("invalid token: %s", tok.Value))
		}
	}

	if depth != 0 {
		return apperr.NewValidation(fmt.Sprintf("unbalanced parentheses: %d unclosed", depth))
	}

	if !hasWord {
		return apperr.NewValidation("expression must contain at least one search term")
	}

	return nil
}
