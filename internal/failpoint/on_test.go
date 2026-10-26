//go:build failpoint

package failpoint

import "testing"

func TestHitError(t *testing.T) {
	t.Setenv("SM_FAILPOINTS", "x.y=error")
	if err := Hit("x.y"); err == nil {
		t.Error("error action not applied")
	}
	if err := Hit("other"); err != nil {
		t.Error("unlisted point fired")
	}
	t.Setenv("SM_FAILPOINTS", "p=panic")
	defer func() {
		if recover() == nil {
			t.Error("panic action not applied")
		}
	}()
	Hit("p")
}
