package failpoint

import "testing"

func TestParse(t *testing.T) {
	m := parse("a.b=crash, c.d=error,bad,=x")
	if m["a.b"] != "crash" || m["c.d"] != "error" || len(m) != 2 {
		t.Errorf("%v", m)
	}
}

func TestHitDefaultIsNoop(t *testing.T) {
	if err := Hit("anything"); err != nil {
		t.Errorf("Hit without failpoints must be a no-op: %v", err)
	}
}
