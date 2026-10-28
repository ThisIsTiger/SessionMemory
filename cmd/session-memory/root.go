package main

import "github.com/spf13/cobra"

func newRoot(a *App) *cobra.Command {
	root := &cobra.Command{
		Use:           "session-memory",
		Short:         "Sync single agent sessions across computers and agents",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("home", "", "state directory (overrides SM_HOME)")
	root.PersistentFlags().Bool("json", false, "print one JSON document")
	root.PersistentFlags().BoolP("verbose", "v", false, "log to stderr")
	root.PersistentFlags().Bool("no-color", false, "disable colour")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error { return a.setup(cmd) }
	root.AddCommand(newVersionCmd(a), newConfigCmd(a), newAgentCmd(a), newInitCmd(a), newTransportCmd(a), newDeviceCmd(a),
		newDiscoverCmd(a), newTrackCmd(a), newListCmd(a), newStatusCmd(a), newCheckpointCmd(a), newSyncCmd(a), newRevisionsCmd(a), newChooseCmd(a), newProjectCmd(a), newRelayCmd(a), newAdoptCmd(a), newAutosyncCmd(a), newFileProjectsCmd(a), newKeyCmd(a), newRestoreCmd(a), newVerifyCmd(a), newConvertCmd(a, "convert"), newConvertCmd(a, "fidelity"), newResumePackCmd(a), newContinueCmd(a), newAutoInstallCmd(a), newOpenCmd(a), newSetupCmd(a))
	registerDev(root, a)
	return root
}
