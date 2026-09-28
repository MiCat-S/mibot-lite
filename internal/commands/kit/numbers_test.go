package kit

import "testing"

// 数量只收非空的一串数字；数字 ID 还可以带一个前导负号（群组、频道），和 MiBox 的 /^-?\d+$/ 一致。
func TestIsDigitsAndNumericID(t *testing.T) {
	for input, want := range map[string]bool{"42": true, "007": true, "": false, "-1": false, "+1": false, "4x2": false, "１２": false} {
		if got := IsDigits(input); got != want {
			t.Errorf("IsDigits(%q) = %v, want %v", input, got, want)
		}
	}
	for input, want := range map[string]bool{"42": true, "-1001234": true, "-": false, "": false, "@42": false, "4x2": false, "+42": false} {
		if got := IsNumericID(input); got != want {
			t.Errorf("IsNumericID(%q) = %v, want %v", input, got, want)
		}
	}
}
