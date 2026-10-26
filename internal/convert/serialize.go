package convert

import (
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/session"
)

// Target describes where a conversion is going to be written.
type Target struct {
	Home           string    // target agent home (read-only hints such as default provider)
	Cwd            string    // local project directory; absolute
	Title          string    // session title, "" for the agent's default
	Now            time.Time // creation time of the new native session
	SourceAgent    string    // origin agent name, used in rendered notes
	SourceRevision string
}

// Serialized is a new native session built in memory.
type Serialized struct {
	NativeID string
	Profile  session.AgentProfile
	Snapshot *agent.MemSnapshot
	Records  int    // native main-stream records written
	Last     string // id or ordinal of the last written record
	// WriterVersion identifies the serialiser in the import receipt.
	WriterVersion string
}

// Serializer turns conversion output into the target agent's native files. A
// target adapter implements it; the engine then installs the result through
// the adapter's native Writer.
type Serializer interface {
	SerializeConverted(out Output, t Target) (Serialized, error)
}
