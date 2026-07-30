package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// The daemon commands wrap kardianos/service, which speaks launchd, systemd and
// the Windows Service Manager. Registering a background service is the one part
// of odctl that changes the machine rather than the account, so each command
// says what it did and where to look when it goes wrong.

// serviceProgram satisfies service.Interface. odctl never runs the daemon
// in-process — it registers opendrived — so these are the no-ops the library
// requires.
type serviceProgram struct{}

func (serviceProgram) Start(service.Service) error { return nil }
func (serviceProgram) Stop(service.Service) error  { return nil }

func daemonService(execPath string, args []string) (service.Service, error) {
	cfg := &service.Config{
		Name:        "opendrive-bridge",
		DisplayName: "OpenDrive Bridge",
		Description: "Keeps your OpenDrive account available to local applications.",
		Executable:  execPath,
		Arguments:   args,
		Option:      service.KeyValue{},
	}

	// On macOS this must be a LaunchAgent, not a LaunchDaemon, and the
	// difference is not cosmetic: an agent runs inside the login session, which
	// is what gives it a Keychain to read. Installed as a system daemon — which
	// is the library's default — it would need root to install and would then
	// have no credential store, so the bridge would start and immediately report
	// that it cannot reach one.
	//
	// Linux and Windows go the other way. A machine running this as a service is
	// usually a server with no desktop session, so a system service plus the
	// encrypted-file store (see deploy/systemd/opendrived.service) is both what
	// people want and the only thing that works there.
	if runtime.GOOS == "darwin" {
		cfg.Option["UserService"] = true
		cfg.Option["KeepAlive"] = true
		cfg.Option["RunAtLoad"] = true
	}
	return service.New(serviceProgram{}, cfg)
}

func newDaemonCommand(o *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Install, start and stop the background bridge",
		Long: "The bridge runs in the background and keeps itself signed in.\n" +
			"'daemon install' registers it with this machine's service manager so it\n" +
			"starts on login; the rest control it once installed.",
	}

	var execPath, addr string
	install := &cobra.Command{
		Use:   "install",
		Short: "Register the bridge to start automatically",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := execPath
			if path == "" {
				found, err := exec.LookPath("opendrived")
				if err != nil {
					return usage("Cannot find 'opendrived' on this machine. " +
						"Install it alongside odctl, or point at it with --exec.")
				}
				path = found
			}
			args := []string{}
			if addr != "" {
				args = append(args, "--addr", addr)
			}

			svc, err := daemonService(path, args)
			if err != nil {
				return serviceError(err)
			}
			if err := svc.Install(); err != nil {
				return serviceError(err)
			}
			_, _ = fmt.Fprintf(o.Out(), "The bridge is installed and will start when you log in.\n"+
				"Start it now with: odctl daemon start\n")
			return nil
		},
	}
	install.Flags().StringVar(&execPath, "exec", "", "path to the opendrived binary")
	install.Flags().StringVar(&addr, "addr", "", "address for the daemon to listen on")

	control := func(use, short, done string, action func(service.Service) error) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				svc, err := daemonService("", nil)
				if err != nil {
					return serviceError(err)
				}
				if err := action(svc); err != nil {
					return serviceError(err)
				}
				_, _ = fmt.Fprintln(o.Out(), done)
				return nil
			},
		}
	}

	cmd.AddCommand(
		install,
		control("start", "Start the background bridge",
			"The bridge is running.", func(s service.Service) error { return s.Start() }),
		control("stop", "Stop the background bridge",
			"The bridge has stopped. Transfers in progress were cancelled cleanly.",
			func(s service.Service) error { return s.Stop() }),
		control("uninstall", "Remove the bridge from this machine's services",
			"The bridge will no longer start on its own. Your credentials are untouched; "+
				"run 'odctl logout' if you want those gone too.",
			func(s service.Service) error { return s.Uninstall() }),
	)
	return cmd
}

// serviceError turns the service library's wording into something a person can
// act on. Its messages are written for developers — "The system has not been
// booted with systemd" — and the usual cause is simply not being root.
func serviceError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(strings.ToLower(msg), "permission denied"),
		strings.Contains(strings.ToLower(msg), "access is denied"):
		return &APIError{Code: "invalid_request", HTTP: 403,
			Message: "This machine will not let you change its services from here. " +
				"Try again with administrator rights."}
	case strings.Contains(msg, "already exists"):
		return &APIError{Code: "conflict", HTTP: 409,
			Message: "The bridge is already installed. Use 'odctl daemon start' to run it, " +
				"or 'odctl daemon uninstall' first if you want to reinstall."}
	case strings.Contains(strings.ToLower(msg), "not installed"),
		strings.Contains(strings.ToLower(msg), "does not exist"):
		return &APIError{Code: "not_found", HTTP: 404,
			Message: "The bridge is not installed on this machine yet. " +
				"Run 'odctl daemon install' first."}
	default:
		return &APIError{Code: "upstream_error", HTTP: 500,
			Message: "This machine's service manager refused: " + msg}
	}
}

// readPassword asks for a password without echoing it, and refuses to read one
// from a pipe silently — a password arriving on stdin in a script is a password
// in somebody's shell history.
func readPassword(o *Options, prompt string) (string, error) {
	f, ok := o.Out().(*os.File)
	if !ok {
		f = os.Stdout
	}
	_, _ = fmt.Fprint(f, prompt)

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		raw, err := term.ReadPassword(fd)
		_, _ = fmt.Fprintln(f)
		if err != nil {
			return "", usage("Could not read the password: %v", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}

	// Not a terminal: read the line, but say plainly that it was visible.
	_, _ = fmt.Fprintln(f)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", usage("No password was given. Run 'odctl login <username>' from a terminal, " +
			"or pass --password.")
	}
	return strings.TrimSpace(line), nil
}
