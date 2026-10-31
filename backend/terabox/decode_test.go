package terabox

import "testing"

func TestDecodeMD5PlainHexUnchanged(t *testing.T) {
	cases := []string{
		"170b40ada8ccda21c07cb568be03c95e",
		"779b71ae356dcc8689c6416b8160e338",
		"545a3bbeadac676c88aa27ad639921bc",
		"d41d8cd98f00b204e9800998ecf8427e",
	}
	for _, in := range cases {
		if got := decodeMD5(in); got != in {
			t.Errorf("decodeMD5(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestDecodeMD5Normalizes(t *testing.T) {
	if got := decodeMD5("  D41D8CD98F00B204E9800998ECF8427E "); got != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Errorf("decodeMD5 did not normalize: %q", got)
	}
}
