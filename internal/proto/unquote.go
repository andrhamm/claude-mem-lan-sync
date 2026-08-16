package proto

import (
	"unicode/utf16"
	"unicode/utf8"
)

// UnquoteJSONString decodes a JSON string literal (quotes included) into the
// bytes the client hashed.
//
// The semantics deliberately match Node, because the client computes
// operation_sha256 as sha256(body, 'utf8') and Node's UTF-8 encoder replaces an
// unpaired surrogate with U+FFFD rather than failing. Verified:
//
//	JSON.stringify("a\ud800b")            -> "a\ud800b"
//	Buffer.from("a\ud800b", "utf8")       -> 61 ef bf bd 62
//
// So an unpaired surrogate is a value the client considers perfectly valid and
// has already hashed as U+FFFD. Rejecting it here would 400 a legitimate body,
// and a 400 parks that op at the head of the client's outbox forever, blocking
// every op behind it. Substituting is the compatible behaviour, not a shortcut.
//
// Genuinely malformed input — a bare control character, a truncated escape, a
// missing quote — is still an error: no conforming client can produce it.
func UnquoteJSONString(literal []byte) ([]byte, error) {
	if len(literal) < 2 || literal[0] != '"' || literal[len(literal)-1] != '"' {
		return nil, Reject(ReasonBodyShape)
	}
	in := literal[1 : len(literal)-1]
	out := make([]byte, 0, len(in))

	for i := 0; i < len(in); {
		c := in[i]

		switch {
		case c == '"':
			// An unescaped quote cannot appear inside a well-formed literal.
			return nil, Reject(ReasonBodyShape)

		case c < 0x20:
			// JSON forbids raw control characters; JSON.stringify always escapes them.
			return nil, Reject(ReasonBodyShape)

		case c == '\\':
			i++
			if i >= len(in) {
				return nil, Reject(ReasonBodyShape)
			}
			esc := in[i]
			switch esc {
			case '"':
				out = append(out, '"')
				i++
			case '\\':
				out = append(out, '\\')
				i++
			case '/':
				out = append(out, '/')
				i++
			case 'b':
				out = append(out, '\b')
				i++
			case 'f':
				out = append(out, '\f')
				i++
			case 'n':
				out = append(out, '\n')
				i++
			case 'r':
				out = append(out, '\r')
				i++
			case 't':
				out = append(out, '\t')
				i++
			case 'u':
				r, size, err := decodeUnicodeEscape(in[i-1:])
				if err != nil {
					return nil, err
				}
				out = utf8.AppendRune(out, r)
				i += size - 1 // size counts the leading backslash
			default:
				return nil, Reject(ReasonBodyShape)
			}

		case c < utf8.RuneSelf:
			out = append(out, c)
			i++

		default:
			// Raw multi-byte sequence. Invalid UTF-8 is replaced one byte at a
			// time, which is what a UTF-8 decoder does with a bad lead byte.
			r, size := utf8.DecodeRune(in[i:])
			if r == utf8.RuneError && size <= 1 {
				out = utf8.AppendRune(out, utf8.RuneError)
				i++
				continue
			}
			out = append(out, in[i:i+size]...)
			i += size
		}
	}

	return out, nil
}

// decodeUnicodeEscape reads a \uXXXX escape at the start of b, pairing
// surrogates where possible. It returns the rune and how many bytes were
// consumed including the leading backslash.
func decodeUnicodeEscape(b []byte) (rune, int, error) {
	// b[0] == '\\', b[1] == 'u'
	if len(b) < 6 {
		return 0, 0, Reject(ReasonBodyShape)
	}
	r1, ok := hex4(b[2:6])
	if !ok {
		return 0, 0, Reject(ReasonBodyShape)
	}

	if !utf16.IsSurrogate(r1) {
		return r1, 6, nil
	}

	// A high surrogate may be followed by a low surrogate escape.
	if len(b) >= 12 && b[6] == '\\' && b[7] == 'u' {
		if r2, ok := hex4(b[8:12]); ok {
			if combined := utf16.DecodeRune(r1, r2); combined != utf8.RuneError {
				return combined, 12, nil
			}
		}
	}

	// Unpaired surrogate: exactly what Node hashes as U+FFFD.
	return utf8.RuneError, 6, nil
}

// hex4 decodes exactly four hex digits into the code unit they represent. The
// result is at most 0xFFFF, so it always fits a rune.
func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var v rune
	for i := 0; i < 4; i++ {
		var d rune
		switch c := b[i]; {
		case c >= '0' && c <= '9':
			d = rune(c - '0')
		case c >= 'a' && c <= 'f':
			d = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = rune(c-'A') + 10
		default:
			return 0, false
		}
		v = v<<4 | d
	}
	return v, true
}
