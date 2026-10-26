package engine

import (
	"session-memory/internal/device"
	"session-memory/internal/session"
)

type deviceOutbox struct{ root string }

func (d *deviceOutbox) add(e ...session.Entry) error { return device.At(d.root).AppendOutbox(e...) }
