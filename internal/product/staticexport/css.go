package staticexport

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/css/scanner"
)

// TransformCSS uses CSS tokens, not a regex across comments or string content.
// Unchanged tokens are emitted verbatim so unrelated template styles survive.
func TransformCSS(input string, visit func(string) (string, error)) (string, error) {
	s := scanner.New(input)
	var out strings.Builder
	importNext := false
	for {
		tok := s.Next()
		if tok.Type == scanner.TokenEOF {
			break
		}
		if tok.Type == scanner.TokenError {
			return "", exportError("STATIC_DEPENDENCY_UNRESOLVED", "invalid CSS dependency syntax", nil)
		}
		raw := tok.Value
		isURL := tok.Type == scanner.TokenURI
		isImport := importNext && tok.Type == scanner.TokenString
		if isURL || isImport {
			value := raw
			if isURL {
				value = strings.TrimSpace(value[strings.IndexByte(value, '(')+1 : len(value)-1])
			}
			if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') {
				value = value[1 : len(value)-1]
			}
			decoded, err := unescapeCSS(value)
			if err != nil {
				return "", err
			}
			changed, err := visit(decoded)
			if err != nil {
				return "", err
			}
			if changed != decoded {
				quoted := "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\a ", "\r", "\\d ").Replace(changed) + "\""
				if isURL {
					raw = "url(" + quoted + ")"
				} else {
					raw = quoted
				}
			}
			importNext = false
		} else if tok.Type == scanner.TokenFunction && strings.EqualFold(raw, "url(") {
			return "", exportError("STATIC_DEPENDENCY_UNRESOLVED", "dynamic CSS URL cannot be exported", nil)
		} else if tok.Type != scanner.TokenS && tok.Type != scanner.TokenComment {
			importNext = tok.Type == scanner.TokenAtKeyword && strings.EqualFold(raw, "@import")
		}
		out.WriteString(raw)
	}
	return out.String(), nil
}

func unescapeCSS(s string) (string, error) {
	var out strings.Builder
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		s = s[n:]
		if r != '\\' {
			out.WriteRune(r)
			continue
		}
		if len(s) == 0 {
			return "", fmt.Errorf("incomplete CSS escape")
		}
		i := 0
		for i < len(s) && i < 6 && strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i])) {
			i++
		}
		if i > 0 {
			v, _ := strconv.ParseInt(s[:i], 16, 32)
			s = s[i:]
			if len(s) > 0 && unicode.IsSpace(rune(s[0])) {
				if strings.HasPrefix(s, "\r\n") {
					s = s[2:]
				} else {
					s = s[1:]
				}
			}
			if v == 0 || v > utf8.MaxRune || (v >= 0xd800 && v <= 0xdfff) {
				out.WriteRune(utf8.RuneError)
			} else {
				out.WriteRune(rune(v))
			}
		} else {
			r, n = utf8.DecodeRuneInString(s)
			s = s[n:]
			if r != '\n' && r != '\r' {
				out.WriteRune(r)
			}
		}
	}
	return out.String(), nil
}
