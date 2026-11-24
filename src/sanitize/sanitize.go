package sanitize

import "strings"

func Text(b []byte) string {
	var x strings.Builder
	for _, r := range string(b) {
		if r == 27 || (r < 32 && r != '\n' && r != '\t') {
			continue
		}
		x.WriteRune(r)
	}
	return x.String()
}

// Field sanitizes text for a single terminal/table field without allowing
// control characters to inject new rows or columns.
func Field(b []byte) string {
	s := Text(b)
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

func FirstLine(b []byte) string {
	s := Text(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
