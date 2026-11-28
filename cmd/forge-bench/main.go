// forge-bench measures agentd's Materialize against git, on the same repository,
// commit and paths: a full clone plus checkout, and a blobless sparse clone plus
// checkout. Rounds are interleaved, the machine load is read before each, and a
// round above the bound is dropped. The upstream request count and bytes of
// agentd and of git come from counting proxies in front of the API and the git
// host.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var osExit = os.Exit

// RootCmd is the command, driven by tests through SetArgs, SetOut and SetErr.
func RootCmd() *cobra.Command {
	c := config{}
	cmd := &cobra.Command{
		Use:           "forge-bench",
		Short:         "measure agentd Materialize against git clone",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBench(cmd.OutOrStdout(), cmd.ErrOrStderr(), c)
		},
	}
	f := cmd.Flags()
	f.StringVar(&c.agentd, "agentd", "", "path of forge-agentd, built from a clean clone of the release tag")
	f.StringVar(&c.agent, "agent", "", "path of forge-agent, built from the same tag")
	f.StringVar(&c.repo, "repo", "", "owner/name, public")
	f.StringVar(&c.commit, "commit", "", "full commit id every variant checks out")
	f.StringArrayVar(&c.paths, "path", nil, "a file or directory to check out (repeatable)")
	f.StringVar(&c.tokenFile, "token-file", "", "the daemon's token file (mode 0600); read by the daemon, never printed")
	f.StringVar(&c.work, "work", "", "absolute durable working directory (not a temporary one)")
	f.StringVar(&c.out, "out", "", "write the raw samples and loads here as JSON")
	f.StringVar(&c.api, "api-url", "https://api.github.com", "the upstream the counting proxy forwards to")
	f.StringVar(&c.gitURL, "git-url", "https://github.com", "the git host the counting proxy forwards to (git and the daemon both go through it)")
	f.IntVar(&c.rounds, "rounds", 5, "rounds measured under the load bound (at least 5)")
	f.Float64Var(&c.maxLoad, "max-load", 8, "a round is dropped when the 1-minute or 5-minute load is above this")
	f.DurationVar(&c.retryWait, "retry-wait", time.Minute, "pause after a dropped round")
	for _, n := range []string{"agentd", "agent", "repo", "commit", "path", "token-file", "work"} {
		_ = cmd.MarkFlagRequired(n)
	}
	return cmd
}

func main() {
	if err := RootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "forge-bench:", err)
		osExit(1)
	}
}
