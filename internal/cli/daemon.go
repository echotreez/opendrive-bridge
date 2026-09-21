package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// The daemon commands wrap kardianos/service, which speaks launchd and systemd.
// Registering a background service is the one part of odctl that changes the
// machine rather than the account, so each command says what it did and where to
// look when it goes wrong.

// serviceProgram satisfies service.Interface. odctl never runs the daemon
// in-process — it registers opendrived — so these are the no-ops the library
// requires.
type serviceProgram struct{}

func (serviceProgram) Start(service.Service) error { return nil }
func (serviceProgram) Stop(service.Service) error  { return nil }

// newDaemonService is a variable so that tests can substitute one. What needs
// testing here is not kardianos/service — it is the part around it: which binary
// is chosen, what the user is told, and whether the shell profile is touched.
// Reaching that through a real Install() would mean writing to the machine
// running the tests.
var newDaemonService = daemonService

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
	// Linux joins it since v1.1, for a reason that is the same shape: the
	// credentials are an encrypted .env in the directory the user unpacked, and
	// a system unit with DynamicUser=yes runs as a user that cannot read the
	// user's home. §8.2 settles it — a user service, started with
	// `systemctl --user`, running as whoever installed it.
	//
	// Both supported platforms get a user service; there is no longer a third
	// with its own rules (§8.1).
	cfg.Option["UserService"] = true
	cfg.Option["KeepAlive"] = true
	cfg.Option["RunAtLoad"] = true
	// The working directory is where .env lives, and it is set explicitly
	// because no service manager inherits the shell's. Without it the daemon
	// would start in / and look for credentials that are not there.
	cfg.WorkingDirectory = filepath.Dir(execPath)
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
	var modifyProfile bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Register the bridge to start automatically",
		Long: "Registers the bridge with this machine's service manager, running it from\n" +
			"the folder you unpacked. Nothing is copied anywhere: the programs, your .env\n" +
			"and your .env.key stay together, and uninstalling is deleting the folder.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := resolveDaemonPath(execPath)
			if err != nil {
				return err
			}
			dir := filepath.Dir(path)

			args := []string{}
			if addr != "" {
				args = append(args, "--addr", addr)
			}

			svc, err := newDaemonService(path, args)
			if err != nil {
				return serviceError(err)
			}
			if err := svc.Install(); err != nil {
				return serviceError(err)
			}
			_, _ = fmt.Fprintf(o.Out(), "The bridge is installed and will start when you log in.\n"+
				"It runs from %s, where your .env lives.\n"+
				"Start it now with: odctl daemon start\n", dir)

			// The PATH suggestion. Printed by default; written only when asked.
			if !modifyProfile {
				_, _ = fmt.Fprintf(o.Out(), "\nTo run odctl from anywhere, add this to your shell profile:\n"+
					"  %s\n"+
					"Or re-run with --modify-shell-profile and it will be added for you,\n"+
					"in a marked block that uninstall removes again.\n", pathLine(dir))
				return nil
			}
			profile, changed, err := addToShellProfile(dir)
			if err != nil {
				// The service is installed; this part failing is worth saying
				// plainly rather than unwinding the whole command.
				_, _ = fmt.Fprintf(o.Err(), "\nThe bridge is installed, but your shell profile was not changed: %v\n", err)
				return nil
			}
			if changed {
				_, _ = fmt.Fprintf(o.Out(), "\nAdded %s to %s. Open a new terminal for it to take effect.\n",
					dir, profile)
			} else {
				_, _ = fmt.Fprintf(o.Out(), "\n%s already has the block; nothing to change.\n", profile)
			}
			return nil
		},
	}
	install.Flags().StringVar(&execPath, "exec", "", "path to the opendrived binary; defaults to the one beside odctl")
	install.Flags().StringVar(&addr, "addr", "", "address for the daemon to listen on")
	install.Flags().BoolVar(&modifyProfile, "modify-shell-profile", false,
		"add the folder to your shell profile's PATH, in a block uninstall can remove")

	control := func(use, short, done string, action func(service.Service) error) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				svc, err := newDaemonService("", nil)
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
		uninstallCommand(o),
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

// resolveDaemonPath finds opendrived, preferring the copy beside odctl.
//
// §8.2: the programs run from the folder they were unpacked into, so the one
// next to this binary is almost always the right one — and looking there first
// means a user who has an older copy somewhere on their PATH does not silently
// install that instead.
func resolveDaemonPath(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", usage("Cannot work out the full path of %s: %v", override, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", usage("There is no opendrived at %s.", abs)
		}
		return abs, nil
	}

	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		candidate := filepath.Join(filepath.Dir(self), daemonBinaryName())
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if found, err := exec.LookPath("opendrived"); err == nil {
		abs, absErr := filepath.Abs(found)
		if absErr == nil {
			return abs, nil
		}
		return found, nil
	}
	return "", usage("Cannot find 'opendrived'. It should be in the same folder as odctl — " +
		"if you moved one of them, point at it with --exec.")
}

// daemonBinaryName is a function rather than a constant because it used to
// answer differently on Windows. It is kept as one so that the call sites read
// the same, and so that restoring a platform with its own executable suffix is a
// change in one place.
func daemonBinaryName() string { return "opendrived" }

// uninstallCommand removes the service and the PATH block, and deliberately
// leaves the credentials alone.
//
// §8.2: deleting .env and .env.key must be something the user asks for, not a
// side effect of removing a service. Somebody uninstalling to reinstall a newer
// version would otherwise have to sign in again for no reason.
func uninstallCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the bridge from this machine's services",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			svc, err := newDaemonService("", nil)
			if err != nil {
				return serviceError(err)
			}
			if err := svc.Uninstall(); err != nil {
				return serviceError(err)
			}
			_, _ = fmt.Fprintln(o.Out(), "The bridge will no longer start on its own.")

			if profile, changed, err := removeFromShellProfile(); err != nil {
				_, _ = fmt.Fprintf(o.Err(), "Your shell profile was left alone: %v\n", err)
			} else if changed {
				_, _ = fmt.Fprintf(o.Out(), "Removed the PATH block from %s.\n", profile)
			}

			_, _ = fmt.Fprintln(o.Out(),
				"Your .env and .env.key are untouched, so reinstalling will not ask you to\n"+
					"sign in again. Delete the folder if you want them gone.")
			return nil
		},
	}
}
