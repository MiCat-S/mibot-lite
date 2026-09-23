package kit

import (
	"testing"
)

func TestUTF16Helpers(t *testing.T) {
	if got := UTF16Len("a中𝚊"); got != 4 {
		t.Fatalf("UTF16Len = %d, want 4 (surrogate pair counts twice)", got)
	}
	if got := UTF16Slice("hello 世界", 6, 2); got != "世界" {
		t.Fatalf("UTF16Slice = %q", got)
	}
}
