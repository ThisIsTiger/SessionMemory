package engine

import (
	"bytes"
	"io"
)

func bytesNewReader(b []byte) io.Reader { return bytes.NewReader(b) }
