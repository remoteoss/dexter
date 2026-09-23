package parser

import (
	"crypto/sha256"
)

func setDefinitionFingerprint(definitions []Definition, from, to int, source []byte, start, end int) {
	fingerprint := sha256.Sum256(source[start:end])
	for i := from; i < to; i++ {
		definitions[i].Fingerprint = fingerprint
	}
}

func sourceTestRootFingerprint(module string) [sha256.Size]byte {
	return sha256.Sum256([]byte("dexter:source-test-root:v1\x00" + module))
}

func scanFingerprintStatementEnd(source []byte, tokens []Token, start int) int {
	depth := 0
	blocks := 0
	lastSignificant := start
	for i := start + 1; i < len(tokens); i++ {
		token := tokens[i]
		switch token.Kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace, TokOpenAngle:
			depth++
			lastSignificant = i
		case TokCloseParen, TokCloseBracket, TokCloseBrace, TokCloseAngle:
			if depth > 0 {
				depth--
			}
			lastSignificant = i
		case TokDo, TokFn:
			blocks++
			lastSignificant = i
		case TokEnd:
			if blocks > 0 {
				blocks--
				lastSignificant = i
				continue
			}
			if depth == 0 {
				return i
			}
		case TokEOL:
			if depth == 0 && blocks == 0 && !expressionContinuesAfter(source, tokens, lastSignificant) {
				return i
			}
		case TokComment:
			continue
		case TokEOF:
			return i
		case TokOther:
			if depth == 0 && blocks == 0 && TokenText(source, token) == ";" {
				return i
			}
			lastSignificant = i
		default:
			lastSignificant = i
		}
	}
	return len(tokens)
}
