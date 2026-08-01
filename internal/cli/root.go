package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/echotreez/opendrive-bridge/internal/server"
)

// Version is set at build time.
var Version = "0.0.0-dev"

// coded is an error that knows what the process should exit with.
type coded interface {
	error
	ExitCode() int
}

// Options are the flags every command shares.
type Options struct {
	Addr    string
	APIKey  string
	Timeout time.Duration
	Verbose bool
	JSON    bool
	// Direct embeds the SDK and talks to OpenDrive without a daemon.
	Direct bool

	out io.Writer
	err io.Writer
	// direct holds the in-process bridge when --direct is in force.
	direct *directBridge
}

// Out and Err are where output goes; tests replace them.
func (o *Options) Out() io.Writer {
	if o.out == nil {
		return os.Stdout
	}
	return o.out
}

func (o *Options) Err() io.Writer {
	if o.err == nil {
		return os.Stderr
	}
	return o.err
}

// SetOutput redirects both streams, for tests.
func (o *Options) SetOutput(out, errOut io.Writer) { o.out, o.err = out, errOut }

// Execute runs odctl and returns the process exit code.
//
// Errors are printed here, once, in the Bridge's own words, and turned into a
// code a script can branch on.
func Execute(args []string, opts *Options) int {
	if opts == nil {
		opts = &Options{}
	}
	root := NewRootCommand(opts)
	root.SetArgs(args)
	root.SetOut(opts.Out())
	root.SetErr(opts.Err())

	// --direct is set up once, after flag parsing but before any command runs,
	// and torn down however the command ends — a cancelled transfer has to
	// reclaim what it created upstream (D39).
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if !opts.Direct {
			return nil
		}
		bridge, err := newDirectBridge(cmd.Context(), opts)
		if err != nil {
			return err
		}
		opts.direct = bridge
		return nil
	}
	defer func() {
		if opts.direct != nil {
			opts.direct.Close()
		}
	}()

	if err := root.Execute(); err != nil {
		return report(opts, err)
	}
	return ExitOK
}

func report(opts *Options, err error) int {
	// The Bridge already wrote something for a person to read. Printing it
	// unchanged is the whole contract; a second wording here would drift from
	// the first.
	_, _ = fmt.Fprintln(opts.Err(), err.Error())

	var api *APIError
	if errors.As(err, &api) && opts.Verbose && api.Upstream != nil && api.Upstream.Message != "" {
		_, _ = fmt.Fprintf(opts.Err(), "\n(what OpenDrive said, for a bug report: %s)\n", api.Upstream.Message)
	}

	var c coded
	if errors.As(err, &c) {
		return c.ExitCode()
	}
	return ExitUsage
}

// NewRootCommand builds the command tree.
func NewRootCommand(opts *Options) *cobra.Command {
	root := &cobra.Command{
		Use:   "odctl",
		Short: "Work with your OpenDrive files from the command line",
		Long: "odctl talks to the OpenDrive Bridge running on this machine.\n\n" +
			"Start with 'odctl login', then use ls, up, down and the rest as you would\n" +
			"expect. Paths are the ones you see in OpenDrive, starting with a slash:\n" +
			"  odctl ls /Documents\n" +
			"  odctl up report.pdf /Documents/report.pdf\n\n" +
			"Exit codes, for scripts:\n" +
			"  0  it worked\n" +
			"  2  something in the command was wrong; fix it and try again\n" +
			"  3  the bridge needs you to sign in again\n" +
			"  4  OpenDrive failed; trying later is reasonable\n" +
			"  5  there is nothing at that path\n" +
			"  6  no bridge is running to talk to\n" +
			"  7  OpenDrive refused this and will refuse it again",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&opts.Addr, "addr", defaultAddr(),
		"where the bridge is listening (or set ODB_ADDR)")
	root.PersistentFlags().StringVar(&opts.APIKey, "api-key", os.Getenv("ODB_API_KEY"),
		"API key, when the bridge requires one")
	root.PersistentFlags().DurationVar(&opts.Timeout, "timeout", 0,
		"give up on a single request after this long")
	root.PersistentFlags().BoolVarP(&opts.Verbose, "verbose", "v", false,
		"also print what OpenDrive itself said, for bug reports")
	root.PersistentFlags().BoolVar(&opts.JSON, "json", false,
		"print raw JSON instead of a table")
	root.PersistentFlags().BoolVar(&opts.Direct, "direct", false,
		"run the bridge inside this command instead of talking to a daemon; "+
			"uses the credentials already in your keychain")

	root.AddCommand(
		newLoginCommand(opts),
		newLogoutCommand(opts),
		newStatusCommand(opts),
		newListCommand(opts),
		newStatCommand(opts),
		newMkdirCommand(opts),
		newUploadCommand(opts),
		newDownloadCommand(opts),
		newMoveCommand(opts),
		newCopyCommand(opts),
		newRenameCommand(opts),
		newRemoveCommand(opts),
		newTrashCommand(opts),
		newVersionsCommand(opts),
		newShareCommand(opts),
		newJobsCommand(opts),
		newDaemonCommand(opts),
	)
	return root
}

// defaultAddr lets a container or a shell profile point odctl somewhere else
// without repeating a flag on every command.
func defaultAddr() string {
	if v := os.Getenv("ODB_ADDR"); v != "" {
		return v
	}
	return server.DefaultAddr
}

// usage is a mistake in the command itself, phrased for the person who made it.
func usage(format string, args ...any) error {
	return &APIError{Code: "invalid_request", HTTP: 400, Message: fmt.Sprintf(format, args...)}
}
