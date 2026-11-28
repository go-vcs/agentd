// Package cli holds the two cobra command trees: forge-agentd (the daemon) and
// forge-agent (the client agents call).
package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/client"
	"github.com/go-vcs/agentd/internal/daemon"
	"github.com/go-vcs/agentd/internal/ghapi"
	"github.com/go-vcs/agentd/internal/sock"
	"github.com/go-vcs/agentd/internal/verdict"
)

// osExit is replaceable by tests.
var osExit = os.Exit

// Exit codes of forge-agent.
const (
	ExitGreen   = 0
	ExitError   = 1
	ExitRed     = 3 // a verdict that is RED, or a refused merge
	ExitPending = 4 // still PENDING when the wait ended
)

// DefaultSocket is where the daemon listens and the client looks.
func DefaultSocket() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = os.TempDir() // never reached on a machine with a home; refused later if unsafe
	}
	return filepath.Join(d, "forge-agentd", "agentd.sock")
}

// Main runs a command tree and exits with its code.
func Main(root *cobra.Command) {
	if err := root.Execute(); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			osExit(ee.code)
			return
		}
		osExit(ExitError)
	}
}

type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// DaemonCmd is forge-agentd.
func DaemonCmd() *cobra.Command {
	var socket, tokenFile, apiURL, matRoot, keychain string
	var poll time.Duration
	var cacheMB int64
	var cacheEntries int
	cmd := &cobra.Command{
		Use:           "forge-agentd",
		Short:         "local agent-VCS daemon with a GitHub backend",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var tok string
			var err error
			if keychain != "" {
				if cmd.Flags().Changed("token-file") {
					return errors.New("--token-keychain and --token-file are alternatives: give one")
				}
				tok, err = ghapi.ReadKeychainToken(cmd.Context(), keychain)
			} else {
				tok, err = ghapi.ReadToken(tokenFile)
			}
			if err != nil {
				return err
			}
			gh, err := ghapi.New(apiURL, tok)
			if err != nil {
				return err
			}
			lim := daemon.DefaultLimits()
			lim.PollInterval = poll
			lim.Cache.MaxBytes, lim.Cache.MaxEntries = cacheMB<<20, cacheEntries
			if matRoot != "" {
				if lim.MaterializeRoot, err = PrepareRoot(matRoot); err != nil {
					return err
				}
			}
			l, err := sock.Listen(socket)
			if err != nil {
				return err
			}
			l.Refused = func(uid uint32, err error) {
				fmt.Fprintf(cmd.ErrOrStderr(), "refused a connection from uid %d\n", uid)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			srv := &http.Server{Handler: daemon.New(gh, lim).Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() { <-ctx.Done(); _ = srv.Close() }()
			fmt.Fprintf(cmd.OutOrStdout(), "forge-agentd listening on %s\n", socket)
			// Serve always returns an error: ErrServerClosed is the clean stop.
			err = srv.Serve(l)
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&socket, "socket", DefaultSocket(), "unix socket to listen on (its directory is created 0700)")
	f.StringVar(&tokenFile, "token-file", defaultTokenFile(), "file holding the GitHub token (mode 0600); never read from argv or the environment")
	f.StringVar(&keychain, "token-keychain", "", "name of a system keychain item (macOS generic password, Linux Secret Service attribute service=<name>) holding the token, instead of --token-file")
	f.StringVar(&apiURL, "api-url", "https://api.github.com", "GitHub REST base URL (https, or http on loopback)")
	f.DurationVar(&poll, "poll-interval", 15*time.Second, "time between upstream reads for a commit being waited on")
	f.Int64Var(&cacheMB, "cache-mb", 256, "object cache size in MiB (in memory; least recently used evicted)")
	f.IntVar(&cacheEntries, "cache-entries", 100000, "most objects the cache holds")
	f.StringVar(&matRoot, "materialize-root", DefaultMaterializeRoot(), "durable directory checkouts are written under, created 0700, never a temporary directory (empty: Materialize is refused)")
	return cmd
}

// DefaultMaterializeRoot is where checkouts go unless told otherwise.
func DefaultMaterializeRoot() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "forge-agentd", "checkouts")
}

// PrepareRoot creates dir (0700) and returns its absolute, symlink-free path.
// A directory under the system's temporary directory is refused: checkouts are
// durable, and a temporary location can be cleaned away under a running agent.
func PrepareRoot(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("materialize root %q must be an absolute path", dir)
	}
	real := dir
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		real, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		return "", err
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	if rel, err := filepath.Rel(tmp, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("materialize root %s is under the temporary directory; choose a durable one", real)
	}
	return real, nil
}

func defaultTokenFile() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".github-token")
}

// AgentCmd is forge-agent.
func AgentCmd() *cobra.Command {
	var socket, label string
	root := &cobra.Command{
		Use:           "forge-agent",
		Short:         "talk to forge-agentd: verdicts, waits and gated merges for exact commits",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&socket, "socket", DefaultSocket(), "the daemon's socket")
	root.PersistentFlags().StringVar(&label, "label", "", "this session's name (required)")

	open := func(cmd *cobra.Command) (*client.Client, error) {
		if label == "" {
			return nil, errors.New("--label is required: every session has its own identity")
		}
		c := client.New(socket, label)
		if err := c.Hello(cmd.Context()); err != nil {
			return nil, report(cmd, err)
		}
		return c, nil
	}
	emit := func(cmd *cobra.Command, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
	}

	verdictCmd := &cobra.Command{
		Use:   "verdict <owner/repo> <full-commit-id>",
		Short: "print the gate verdict of one exact commit (exit 0 green, 3 red, 4 pending)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := open(cmd)
			if err != nil {
				return err
			}
			r, err := c.Verdict(cmd.Context(), args[0], args[1])
			if err != nil {
				return report(cmd, err)
			}
			emit(cmd, r)
			return verdictExit(r.State, false)
		},
	}
	var timeout time.Duration
	waitCmd := &cobra.Command{
		Use:   "wait <owner/repo> <full-commit-id>",
		Short: "wait for the verdict of one exact commit; one poller per machine serves every waiter",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := open(cmd)
			if err != nil {
				return err
			}
			r, err := c.Wait(cmd.Context(), args[0], args[1], timeout)
			if err != nil {
				return report(cmd, err)
			}
			emit(cmd, r)
			return verdictExit(r.State, r.TimedOut)
		},
	}
	waitCmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "longest wait")

	var evTimeout time.Duration
	eventsCmd := &cobra.Command{
		Use:   "events <owner/repo> <full-commit-id>",
		Short: "print one JSON line per change of a commit's verdict until it is final; one poller per machine serves every stream",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := open(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), evTimeout)
			defer cancel()
			since := ""
			for {
				// One call waits at most ten minutes; the flag bounds the whole stream.
				r, err := c.Events(ctx, args[0], args[1], since, 10*time.Minute)
				if err != nil {
					if ctx.Err() != nil {
						return verdictExit(verdict.Pending, true)
					}
					return report(cmd, err)
				}
				since = r.Cursor
				if r.TimedOut {
					continue
				}
				emit(cmd, r)
				if r.Final {
					return verdictExit(r.State, false)
				}
			}
		},
	}
	eventsCmd.Flags().DurationVar(&evTimeout, "timeout", 30*time.Minute, "longest the whole stream runs")

	var method, requestID string
	var pr int
	mergeCmd := &cobra.Command{
		Use:   "merge <owner/repo> <pr> <full-commit-id>",
		Short: "merge a pull request only if the commit named is its head and is green",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := fmt.Sscanf(args[1], "%d", &pr); err != nil || pr <= 0 {
				return errors.New("pr must be a positive number")
			}
			c, err := open(cmd)
			if err != nil {
				return err
			}
			if requestID == "" {
				requestID = client.NewRequestID()
				// Printed so that a retry after a timeout can reuse it.
				fmt.Fprintln(cmd.ErrOrStderr(), "request-id", requestID)
			}
			r, err := c.Merge(cmd.Context(), requestID, args[0], pr, args[2], method)
			if err != nil {
				return report(cmd, err)
			}
			emit(cmd, r)
			return nil
		},
	}
	mergeCmd.Flags().StringVar(&method, "method", "merge", "merge, squash or rebase")
	mergeCmd.Flags().StringVar(&requestID, "request-id", "", "32 hex digits; reuse it to retry safely (default: random)")
	blobCmd := &cobra.Command{
		Use:   "blob <owner/repo> <full-blob-id>",
		Short: "write a blob's bytes to stdout, verified against its id by the daemon",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := open(cmd)
			if err != nil {
				return err
			}
			r, err := c.ReadBlob(cmd.Context(), args[0], args[1])
			if err != nil {
				return report(cmd, err)
			}
			b, err := base64.StdEncoding.DecodeString(r.Content)
			if err != nil {
				return report(cmd, aerr.New(aerr.Internal, ""))
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	treeCmd := &cobra.Command{
		Use:   "tree <owner/repo> <full-tree-id>",
		Short: "print a tree's entries as JSON, verified against its id by the daemon",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := open(cmd)
			if err != nil {
				return err
			}
			r, err := c.ReadTree(cmd.Context(), args[0], args[1])
			if err != nil {
				return report(cmd, err)
			}
			emit(cmd, r)
			return nil
		},
	}
	var paths []string
	matCmd := &cobra.Command{
		Use:   "materialize <owner/repo> <full-commit-id> <dir>",
		Short: "write a sparse checkout of a commit into a new directory under the daemon's checkout root",
		Long: "Every tree and blob is verified against its id by the daemon before it is written. " +
			"<dir> is relative to the daemon's --materialize-root and must not exist; --path (repeatable) " +
			"restricts the checkout, no --path writes the whole tree. The result is a plain directory, not a git worktree.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := open(cmd)
			if err != nil {
				return err
			}
			r, err := c.Materialize(cmd.Context(), args[0], args[1], args[2], paths)
			if err != nil {
				return report(cmd, err)
			}
			emit(cmd, r)
			return nil
		},
	}
	matCmd.Flags().StringArrayVar(&paths, "path", nil, "a file or directory to check out (repeatable; default: the whole tree)")
	root.AddCommand(verdictCmd, waitCmd, eventsCmd, mergeCmd, blobCmd, treeCmd, matCmd)
	return root
}

func verdictExit(s verdict.State, timedOut bool) error {
	switch {
	case timedOut || s == verdict.Pending:
		return exitError{ExitPending}
	case s == verdict.Red:
		return exitError{ExitRed}
	}
	return nil
}

// report prints a refusal as its structured JSON body on stderr and picks the
// exit code: a refusal of the gate is 3, anything else 1.
func report(cmd *cobra.Command, err error) error {
	e := aerr.As(err)
	b, _ := json.Marshal(struct {
		Error *aerr.Error `json:"error"`
	}{e})
	fmt.Fprintln(cmd.ErrOrStderr(), string(b))
	switch e.Reason {
	case aerr.VerdictRed, aerr.RefMoved, aerr.VerdictPending, aerr.Conflict, aerr.ChangeClosed:
		return exitError{ExitRed}
	}
	return exitError{ExitError}
}

var _ = context.Background
