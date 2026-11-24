package sanitize

import "testing"

func TestText(t *testing.T) {
	if got := string([]byte(Text([]byte("ok\x1b[31m\x07\n")))); got != "ok[31m\n" {
		t.Fatalf("%q", got)
	}
}

func TestFieldEscapesRowAndColumnSeparators(t *testing.T) {
	if got := Field([]byte("a\\b\n c\td")); got != "a\\\\b\\n c\\td" {
		t.Fatalf("%q", got)
	}
}
