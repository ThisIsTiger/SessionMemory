// Package codexgen generates deterministic synthetic Codex rollouts for tests.
// Record shapes follow what is observed in real rollouts (spec §14.2); fields
// of newer records (item_completed) are provisional until `dev inventory`
// confirms them.
package codexgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Options controls the generated session.
type Options struct {
	Seed          int64
	ThreadID      string
	Cwd           string
	Turns         int
	ToolCalls     int // per turn
	Compacted     bool
	Images        bool
	Ordinals      bool
	UILayer       bool
	CLIVersion    string
	Source        string
	Originator    string
	ModelProvider string
	// PadBytes adds that many bytes of filler text to every assistant message,
	// to build large sessions cheaply.
	PadBytes int
}

func (o *Options) defaults() {
	if o.Cwd == "" {
		o.Cwd = "/work/app"
	}
	if o.Turns == 0 {
		o.Turns = 2
	}
	if o.CLIVersion == "" {
		o.CLIVersion = "0.150.0-test"
	}
	if o.Source == "" {
		o.Source = "vscode"
	}
	if o.Originator == "" {
		o.Originator = "Codex Desktop"
	}
	if o.ModelProvider == "" {
		o.ModelProvider = "openai"
	}
}

// ThreadIDFor derives a UUID-shaped thread id from a seed.
func ThreadIDFor(seed int64) string {
	r := rand.New(rand.NewPCG(uint64(seed), 7))
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type gen struct {
	o   Options
	buf bytes.Buffer
	n   int
	t   time.Time
}

func (g *gen) rec(typ string, payload any) {
	g.t = g.t.Add(time.Second)
	env := map[string]any{"timestamp": g.t.UTC().Format("2006-01-02T15:04:05.000Z"), "type": typ, "payload": payload}
	if g.o.Ordinals {
		env["ordinal"] = g.n
	}
	g.n++
	b, _ := marshalKeyOrder(env, g.o.Ordinals)
	g.buf.Write(b)
	g.buf.WriteByte('\n')
}

// marshalKeyOrder writes timestamp, ordinal, type, payload in that order.
func marshalKeyOrder(env map[string]any, ord bool) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"timestamp":`)
	t, _ := json.Marshal(env["timestamp"])
	b.Write(t)
	if ord {
		fmt.Fprintf(&b, `,"ordinal":%d`, env["ordinal"])
	}
	b.WriteString(`,"type":`)
	ty, _ := json.Marshal(env["type"])
	b.Write(ty)
	b.WriteString(`,"payload":`)
	p, err := json.Marshal(env["payload"])
	if err != nil {
		return nil, err
	}
	b.Write(p)
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Rollout returns an uncompressed JSONL rollout, every line ending in "\n".
func Rollout(o Options) []byte {
	o.defaults()
	if o.ThreadID == "" {
		o.ThreadID = ThreadIDFor(o.Seed)
	}
	r := rand.New(rand.NewPCG(uint64(o.Seed), 11))
	g := &gen{o: o, t: time.Date(2026, 9, 1, 1, 46, 53, 0, time.UTC)}
	g.rec("session_meta", map[string]any{"session_id": o.ThreadID, "id": o.ThreadID, "timestamp": g.t.Format("2006-01-02T15:04:05.000Z"),
		"cwd": o.Cwd, "originator": o.Originator, "cli_version": o.CLIVersion, "source": o.Source, "model_provider": o.ModelProvider})
	padb := make([]byte, o.PadBytes)
	for i := range padb {
		padb[i] = byte('a' + r.IntN(26))
	}
	pad := string(padb)
	for i := 0; i < o.Turns; i++ {
		turn := fmt.Sprintf("turn-%03d", i)
		g.rec("turn_context", map[string]any{"turn_id": turn, "timestamp": g.t.Format("2006-01-02T15:04:05.000Z"), "model": "gpt-test", "cwd": o.Cwd})
		user := fmt.Sprintf("user message %d", i)
		g.rec("response_item", map[string]any{"type": "message", "id": fmt.Sprintf("msg_u%d", i), "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": user}}})
		g.rec("event_msg", map[string]any{"type": "user_message", "message": user})
		g.rec("response_item", map[string]any{"type": "reasoning", "summary": nil, "content": nil, "encrypted_content": "ZW5jcnlwdGVk"})
		for c := 0; c < o.ToolCalls; c++ {
			id := fmt.Sprintf("call_%d_%d", i, c)
			g.rec("response_item", map[string]any{"type": "function_call", "name": "exec_command",
				"arguments": `{"cmd":"ls","workdir":"` + o.Cwd + `"}`, "call_id": id})
			g.rec("response_item", map[string]any{"type": "function_call_output", "call_id": id, "output": "file1\nfile2"})
		}
		if o.Images && i == 0 {
			g.rec("response_item", map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}}})
		}
		reply := fmt.Sprintf("assistant reply %d %s", i, pad)
		g.rec("response_item", map[string]any{"type": "message", "id": fmt.Sprintf("msg_a%d", i), "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": reply}}})
		g.rec("event_msg", map[string]any{"type": "agent_message", "message": reply})
		if o.UILayer {
			g.rec("event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "agent_message", "text": reply}})
		}
		if o.Compacted && i == o.Turns/2 {
			g.rec("compacted", map[string]any{"message": "summary of earlier turns"})
		}
		g.rec("event_msg", map[string]any{"type": "task_complete"})
	}
	return g.buf.Bytes()
}

// Zstd compresses plain as a single zstd frame without checksum.
func Zstd(plain []byte) []byte {
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf, zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(1))
	w.Write(plain)
	w.Close()
	return buf.Bytes()
}
