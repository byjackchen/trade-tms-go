package hyperopt

// safe_eval.go ports src/strategies/params/safe_eval.py (spec §2.4 [MUST-MATCH]).
// A whitelisted expression evaluator for constraint expressions in strategy
// param JSON (e.g. "min(1.0, entry_z - 0.1)"), supporting numeric literals,
// variable lookup against the sampled scope, binary + - * / (true division),
// unary +/-, and calls to exactly min/max/abs with positional args only.
// Anything else errors. Implemented as a tiny recursive-descent parser since
// Go's expression grammar differs from Python's.
//
// Error parity with the reference:
//   - unknown variable                -> "undefined: <name>"
//   - keyword args                    -> "keyword arguments not supported"
//   - unknown function / other node   -> "unsupported ..."
//   - division by zero                -> "division by zero"

import (
	"fmt"
	"strconv"
)

// safeEval evaluates expression against scope (variable -> float64), matching
// safe_eval.safe_eval. All values are float64 (the param ranges are numeric).
func safeEval(expression string, scope map[string]float64) (float64, error) {
	p := &exprParser{src: expression}
	p.next()
	v, err := p.parseExpr(scope)
	if err != nil {
		return 0, err
	}
	if p.tok.kind != tokEOF {
		return 0, fmt.Errorf("unsupported expression node: trailing token %q", p.tok.text)
	}
	return v, nil
}

// ---- tokenizer ----

type tokKind int

const (
	tokEOF tokKind = iota
	tokNum
	tokIdent
	tokPlus
	tokMinus
	tokStar
	tokSlash
	tokLParen
	tokRParen
	tokComma
)

type token struct {
	kind tokKind
	text string
}

type exprParser struct {
	src string
	pos int
	tok token
}

func (p *exprParser) next() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
	if p.pos >= len(p.src) {
		p.tok = token{kind: tokEOF}
		return
	}
	c := p.src[p.pos]
	switch c {
	case '+':
		p.pos++
		p.tok = token{tokPlus, "+"}
		return
	case '-':
		p.pos++
		p.tok = token{tokMinus, "-"}
		return
	case '*':
		p.pos++
		p.tok = token{tokStar, "*"}
		return
	case '/':
		p.pos++
		p.tok = token{tokSlash, "/"}
		return
	case '(':
		p.pos++
		p.tok = token{tokLParen, "("}
		return
	case ')':
		p.pos++
		p.tok = token{tokRParen, ")"}
		return
	case ',':
		p.pos++
		p.tok = token{tokComma, ","}
		return
	}
	// number
	if c >= '0' && c <= '9' || c == '.' {
		start := p.pos
		for p.pos < len(p.src) {
			ch := p.src[p.pos]
			if (ch >= '0' && ch <= '9') || ch == '.' || ch == 'e' || ch == 'E' ||
				((ch == '+' || ch == '-') && p.pos > start && (p.src[p.pos-1] == 'e' || p.src[p.pos-1] == 'E')) {
				p.pos++
				continue
			}
			break
		}
		p.tok = token{tokNum, p.src[start:p.pos]}
		return
	}
	// identifier
	if isIdentStart(c) {
		start := p.pos
		for p.pos < len(p.src) && isIdentPart(p.src[p.pos]) {
			p.pos++
		}
		p.tok = token{tokIdent, p.src[start:p.pos]}
		return
	}
	p.tok = token{tokEOF, string(c)} // unknown char surfaces as a parse error upstream
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// ---- recursive descent: expr -> term (('+'|'-') term)* ----

func (p *exprParser) parseExpr(scope map[string]float64) (float64, error) {
	left, err := p.parseTerm(scope)
	if err != nil {
		return 0, err
	}
	for p.tok.kind == tokPlus || p.tok.kind == tokMinus {
		op := p.tok.kind
		p.next()
		right, err := p.parseTerm(scope)
		if err != nil {
			return 0, err
		}
		if op == tokPlus {
			left += right
		} else {
			left -= right
		}
	}
	return left, nil
}

// term -> unary (('*'|'/') unary)*
func (p *exprParser) parseTerm(scope map[string]float64) (float64, error) {
	left, err := p.parseUnary(scope)
	if err != nil {
		return 0, err
	}
	for p.tok.kind == tokStar || p.tok.kind == tokSlash {
		op := p.tok.kind
		p.next()
		right, err := p.parseUnary(scope)
		if err != nil {
			return 0, err
		}
		if op == tokStar {
			left *= right
		} else {
			if right == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			left /= right
		}
	}
	return left, nil
}

// unary -> ('+'|'-') unary | primary
func (p *exprParser) parseUnary(scope map[string]float64) (float64, error) {
	if p.tok.kind == tokMinus {
		p.next()
		v, err := p.parseUnary(scope)
		return -v, err
	}
	if p.tok.kind == tokPlus {
		p.next()
		return p.parseUnary(scope)
	}
	return p.parsePrimary(scope)
}

// primary -> number | name | call | '(' expr ')'
func (p *exprParser) parsePrimary(scope map[string]float64) (float64, error) {
	switch p.tok.kind {
	case tokNum:
		f, err := strconv.ParseFloat(p.tok.text, 64)
		if err != nil {
			return 0, fmt.Errorf("unsupported expression node: bad number %q", p.tok.text)
		}
		p.next()
		return f, nil
	case tokLParen:
		p.next()
		v, err := p.parseExpr(scope)
		if err != nil {
			return 0, err
		}
		if p.tok.kind != tokRParen {
			return 0, fmt.Errorf("unsupported expression node: expected ')'")
		}
		p.next()
		return v, nil
	case tokIdent:
		name := p.tok.text
		p.next()
		if p.tok.kind == tokLParen {
			return p.parseCall(name, scope)
		}
		v, ok := scope[name]
		if !ok {
			return 0, fmt.Errorf("undefined: %s", name)
		}
		return v, nil
	default:
		return 0, fmt.Errorf("unsupported expression node: %q", p.tok.text)
	}
}

func (p *exprParser) parseCall(name string, scope map[string]float64) (float64, error) {
	switch name {
	case "min", "max", "abs":
	default:
		return 0, fmt.Errorf("unsupported function: %s", name)
	}
	p.next() // consume '('
	var args []float64
	if p.tok.kind != tokRParen {
		for {
			v, err := p.parseExpr(scope)
			if err != nil {
				return 0, err
			}
			args = append(args, v)
			if p.tok.kind == tokComma {
				p.next()
				continue
			}
			break
		}
	}
	if p.tok.kind != tokRParen {
		return 0, fmt.Errorf("unsupported expression node: expected ')' in call to %s", name)
	}
	p.next()
	return applyFunc(name, args)
}

func applyFunc(name string, args []float64) (float64, error) {
	if len(args) == 0 {
		return 0, fmt.Errorf("%s expected at least 1 arg", name)
	}
	switch name {
	case "abs":
		if len(args) != 1 {
			return 0, fmt.Errorf("abs expected 1 arg, got %d", len(args))
		}
		if args[0] < 0 {
			return -args[0], nil
		}
		return args[0], nil
	case "min":
		m := args[0]
		for _, a := range args[1:] {
			if a < m {
				m = a
			}
		}
		return m, nil
	case "max":
		m := args[0]
		for _, a := range args[1:] {
			if a > m {
				m = a
			}
		}
		return m, nil
	}
	return 0, fmt.Errorf("unsupported function: %s", name)
}
