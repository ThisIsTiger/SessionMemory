package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"session-memory/internal/agent"
	"session-memory/internal/bridge"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// effectiveRevision resolves the revision to work on: --revision or the
// effective head (refusing a conflict).
func effectiveRevision(r *rt, sessionID, want string) (session.Revision, error) {
	revs, err := r.Store.Revisions(sessionID)
	if err != nil {
		return session.Revision{}, err
	}
	if len(revs) == 0 {
		return session.Revision{}, errcode.New(errcode.NotFound, "no revisions for this session")
	}
	if want == "" {
		g := session.NewGraph(revs)
		ch, _ := r.Store.Choices(sessionID)
		res := session.Resolve(g, ch)
		if res.Conflict {
			return session.Revision{}, errcode.New(errcode.Conflict, "the session has more than one live head; run choose first")
		}
		want = res.Effective
	}
	for _, rv := range revs {
		if rv.RevisionID == want {
			return rv, nil
		}
	}
	return session.Revision{}, errcode.New(errcode.NotFound, "revision not found")
}

func extractorFor(a *App, name string) (agent.Extractor, error) {
	ad, err := a.adapter(name)
	if err != nil {
		return nil, err
	}
	ex, ok := ad.(agent.Extractor)
	if !ok {
		return nil, errcode.New(errcode.Unsupported, name+" cannot be a conversion source")
	}
	return ex, nil
}

func newConvertCmd(a *App, use string) *cobra.Command {
	c := &cobra.Command{Use: use + " <session_id>", Short: "Show the conversion plan and fidelity report for a target agent", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _ := cmd.Flags().GetString("agent")
			planOnly, _ := cmd.Flags().GetBool("plan")
			revID, _ := cmd.Flags().GetString("revision")
			if use == "convert" && !planOnly {
				return errcode.New(errcode.Unsupported, "writing converted sessions is not available until the P0 experiments E3/E4 decide the target write route; use --plan")
			}
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			rev, err := effectiveRevision(r, args[0], revID)
			if err != nil {
				return err
			}
			ex, err := extractorFor(a, rev.AgentProfile.Agent)
			if err != nil {
				return err
			}
			out, err := r.Eng.ConversionReport(cmd.Context(), ex, rev, target)
			if err != nil {
				return err
			}
			if a.JSON {
				return a.printJSON(out.Report)
			}
			rep := out.Report
			fmt.Fprintf(a.Out, "revision %s -> %s\nvisible items: %d  token estimate: %d\n", rev.RevisionID[:16], target, rep.VisibleItems, rep.TokenEstimate)
			for k, v := range rep.Counts {
				fmt.Fprintf(a.Out, "  %-16s kept=%d rendered=%d dropped=%d\n", k, v.Kept, v.Rendered, v.Dropped)
			}
			for _, n := range rep.ContextNotes {
				fmt.Fprintf(a.Out, "note: %s\n", n)
			}
			fmt.Fprintln(a.Out, "data egress: the full history would be sent to the target agent's model service")
			return nil
		}}
	c.Flags().String("agent", "", "target agent")
	c.Flags().Bool("plan", false, "show only; write nothing")
	c.Flags().String("revision", "", "revision id (default: effective head)")
	c.MarkFlagRequired("agent")
	return c
}

func newResumePackCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "resume-pack <session_id>", Short: "Write the context-only fallback pack", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, _ := cmd.Flags().GetString("out")
			revID, _ := cmd.Flags().GetString("revision")
			if out == "" {
				return errcode.New(errcode.Usage, "--out is required")
			}
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			rev, err := effectiveRevision(r, args[0], revID)
			if err != nil {
				return err
			}
			ex, err := extractorFor(a, rev.AgentProfile.Agent)
			if err != nil {
				return err
			}
			items, err := r.Eng.RevisionItems(cmd.Context(), ex, rev)
			if err != nil {
				return err
			}
			pack := bridge.ResumePack(rev, items, bridge.Options{})
			f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return errcode.Wrap(errcode.Usage, "cannot create the output file (it must not exist)", err)
			}
			defer f.Close()
			_, err = f.WriteString(pack)
			return err
		}}
	c.Flags().String("out", "", "output file")
	c.Flags().String("revision", "", "revision id (default: effective head)")
	return c
}

func newContinueCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "continue <session_id>", Short: "Ask another device to continue this session in an agent", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _ := cmd.Flags().GetString("agent")
			dev, _ := cmd.Flags().GetString("device")
			mode, _ := cmd.Flags().GetString("mode")
			revID, _ := cmd.Flags().GetString("revision")
			if mode == "" {
				mode = session.ModeConvert
			}
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			rev, err := effectiveRevision(r, args[0], revID)
			if err != nil {
				return err
			}
			e := session.Entry{Type: session.EntryConvertRequest, ConvertRequest: &session.ConvertRequestEntry{RequestID: session.NewRequestID(),
				SessionID: args[0], RevisionID: rev.RevisionID, SourceDeviceID: r.Info.DeviceID, TargetAgent: target, TargetDeviceID: dev, Mode: mode,
				At: session.FormatTime(a.Now())}}
			if err := e.Validate(); err != nil {
				return errcode.Wrap(errcode.Usage, err.Error(), err)
			}
			if err := r.Sync2().AppendOutbox(e); err != nil {
				return err
			}
			if r.Sync != nil {
				if _, err := r.Sync.Publish(cmd.Context()); err != nil {
					return err
				}
			}
			if a.JSON {
				return a.printJSON(json.RawMessage(`{"requested":true}`))
			}
			fmt.Fprintf(a.Out, "request %s published\n", e.ConvertRequest.RequestID)
			return nil
		}}
	c.Flags().String("agent", "", "target agent")
	c.Flags().String("device", "", "target device id")
	c.Flags().String("mode", "", "convert (default) or context")
	c.Flags().String("revision", "", "revision id")
	c.MarkFlagRequired("agent")
	c.MarkFlagRequired("device")
	return c
}
