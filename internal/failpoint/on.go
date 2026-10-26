//go:build failpoint

package failpoint

import (
	"os"

	"session-memory/internal/errcode"
)

func hit(name string) error {
	switch parse(os.Getenv("SM_FAILPOINTS"))[name] {
	case "crash":
		os.Exit(137)
	case "panic":
		panic("failpoint " + name)
	case "error":
		return errcode.New(errcode.Internal, "failpoint "+name)
	}
	return nil
}
