package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------- auth

func newLoginCommand(o *Options) *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "login <username>",
		Short: "Sign in and let the bridge remember it",
		Long: "Signs in once and hands the credentials to your system keychain, after\n" +
			"which the bridge keeps itself signed in. You will only be asked again if\n" +
			"you change your OpenDrive password.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pw := password
			if pw == "" {
				var err error
				pw, err = readPassword(o, "Password for "+args[0]+": ")
				if err != nil {
					return err
				}
			}
			if pw == "" {
				return usage("A password is needed to sign in.")
			}

			var status statusResponse
			if err := client(o).Do(cmd.Context(), "POST", "/v1/auth/login",
				map[string]string{"username": args[0], "password": pw}, &status); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, status)
			}
			_, _ = fmt.Fprintf(o.Out(), "Signed in as %s.\n", args[0])
			if status.Seamless {
				_, _ = fmt.Fprintln(o.Out(), "The bridge will keep itself signed in from now on.")
			} else {
				_, _ = fmt.Fprintln(o.Out(), "Note: your password was not stored, so you will need to sign in again "+
					"after a restart.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&password, "password", "",
		"password (leave this out to be prompted, which keeps it out of your shell history)")
	return cmd
}

func newLogoutCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the stored credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out map[string]any
			if err := client(o).Do(cmd.Context(), "POST", "/v1/auth/logout", nil, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			_, _ = fmt.Fprintln(o.Out(), detailOf(out, "Signed out."))
			return nil
		},
	}
}

type statusResponse struct {
	Account *struct {
		Username string `json:"username"`
		UserID   string `json:"user_id"`
	} `json:"account"`
	AuthMode       string  `json:"auth_mode"`
	State          string  `json:"state"`
	Seamless       bool    `json:"seamless"`
	TokenExpiresAt *string `json:"token_expires_at"`
	Keystore       struct {
		Backend   string `json:"backend"`
		Available bool   `json:"available"`
	} `json:"keystore"`
	Quota *struct {
		StorageUsed int64 `json:"storage_used"`
		StorageMax  int64 `json:"storage_max"`
		BWUsed      int64 `json:"bw_used"`
		BWMax       int64 `json:"bw_max"`
	} `json:"quota"`
}

func newStatusCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the bridge is signed in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var s statusResponse
			if err := client(o).Do(cmd.Context(), "GET", "/v1/auth/status", nil, &s); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, s)
			}

			// The state is the thing somebody ran this to find out, so it is
			// said in a sentence rather than printed as an enum.
			switch s.State {
			case "not_configured":
				_, _ = fmt.Fprintln(o.Out(), "Not signed in yet. Run 'odctl login <username>' to start.")
				return nil
			case "authenticated":
				_, _ = fmt.Fprintf(o.Out(), "Signed in as %s.\n", accountName(s))
			case "refreshing":
				_, _ = fmt.Fprintf(o.Out(), "Signed in as %s; the bridge is renewing its session.\n", accountName(s))
			case "reauth_required":
				_, _ = fmt.Fprintf(o.Out(), "Your saved password is no longer accepted. "+
					"Run 'odctl login %s' with your current password.\n", accountName(s))
			case "captcha_required":
				_, _ = fmt.Fprintln(o.Out(), "OpenDrive is asking for a captcha the bridge cannot answer. "+
					"Sign in once at opendrive.com, then try again.")
			case "keystore_unavailable":
				_, _ = fmt.Fprintln(o.Out(), "The bridge cannot reach its credential store. "+
					"Unlock your login keychain and it will recover on its own.")
			default:
				_, _ = fmt.Fprintf(o.Out(), "State: %s\n", s.State)
			}

			if !s.Seamless && s.State == "authenticated" {
				_, _ = fmt.Fprintln(o.Out(), "Your password is not stored, so a restart will need you to sign in again.")
			}
			if s.Quota != nil && s.Quota.StorageMax > 0 {
				_, _ = fmt.Fprintf(o.Out(), "Storage: %s of %s used.\n",
					humanBytes(s.Quota.StorageUsed), humanBytes(s.Quota.StorageMax))
			}
			return nil
		},
	}
}

func accountName(s statusResponse) string {
	if s.Account != nil && s.Account.Username != "" {
		return s.Account.Username
	}
	return "your account"
}

// ---------------------------------------------------------------- files

type entry struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Kind     string  `json:"kind"`
	Size     int64   `json:"size"`
	Modified *string `json:"modified"`
	Public   bool    `json:"public"`
}

type listResponse struct {
	Path          string  `json:"path"`
	Entries       []entry `json:"entries"`
	DirUpdateTime int64   `json:"dir_update_time"`
	NextOffset    *int    `json:"next_offset"`
}

func newListCommand(o *Options) *cobra.Command {
	var long bool
	cmd := &cobra.Command{
		Use:     "ls [path]",
		Short:   "List a folder",
		Args:    cobra.MaximumNArgs(1),
		Aliases: []string{"list"},
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/"
			if len(args) == 1 {
				path = args[0]
			}
			var out listResponse
			if err := client(o).Do(cmd.Context(), "GET", query("/v1/ls", "path", path), nil, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			if len(out.Entries) == 0 {
				_, _ = fmt.Fprintf(o.Out(), "%s is empty.\n", out.Path)
				return nil
			}

			sort.Slice(out.Entries, func(i, j int) bool {
				if out.Entries[i].Kind != out.Entries[j].Kind {
					return out.Entries[i].Kind == "folder"
				}
				return out.Entries[i].Name < out.Entries[j].Name
			})

			w := tabwriter.NewWriter(o.Out(), 0, 0, 2, ' ', 0)
			for _, e := range out.Entries {
				name := e.Name
				if e.Kind == "folder" {
					name += "/"
				}
				if long {
					_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, kindLabel(e), humanBytes(e.Size), modified(e))
				} else {
					_, _ = fmt.Fprintf(w, "%s\t%s\n", name, humanBytes(e.Size))
				}
			}
			_ = w.Flush()

			if out.NextOffset != nil {
				_, _ = fmt.Fprintf(o.Out(), "\nThere is more. For the next page:\n"+
					"  odctl ls %s --json  # then use offset=%d and dir_update_time=%d\n",
					out.Path, *out.NextOffset, out.DirUpdateTime)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&long, "long", "l", false, "show kind, size and modified time")
	return cmd
}

func kindLabel(e entry) string {
	if e.Kind == "folder" {
		return "folder"
	}
	if e.Public {
		return "file (public)"
	}
	return "file"
}

func modified(e entry) string {
	if e.Modified == nil {
		return "-"
	}
	if t, err := time.Parse(time.RFC3339, *e.Modified); err == nil {
		return t.Local().Format("2006-01-02 15:04")
	}
	return *e.Modified
}

func newStatCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "stat <path>",
		Short: "Show details of one file or folder",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var e entry
			if err := client(o).Do(cmd.Context(), "GET", query("/v1/stat", "path", args[0]), nil, &e); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, e)
			}
			w := tabwriter.NewWriter(o.Out(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintf(w, "path\t%s\n", e.Path)
			_, _ = fmt.Fprintf(w, "kind\t%s\n", kindLabel(e))
			_, _ = fmt.Fprintf(w, "size\t%s\n", humanBytes(e.Size))
			_, _ = fmt.Fprintf(w, "modified\t%s\n", modified(e))
			return w.Flush()
		},
	}
}

func newMkdirCommand(o *Options) *cobra.Command {
	var parents bool
	var access string
	cmd := &cobra.Command{
		Use:   "mkdir <path>",
		Short: "Create a folder",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"path": args[0], "parents": parents}
			if access != "" {
				body["access"] = access
			}
			var e entry
			if err := client(o).Do(cmd.Context(), "POST", "/v1/mkdir", body, &e); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, e)
			}
			_, _ = fmt.Fprintf(o.Out(), "Created %s\n", e.Path)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&parents, "parents", "p", false, "create missing folders along the way")
	cmd.Flags().StringVar(&access, "access", "", "private (the default), public or hidden")
	return cmd
}

func newMoveCommand(o *Options) *cobra.Command {
	return srcDstCommand(o, "mv", "Move a file or folder", "/v1/mv", "Moved")
}

func newCopyCommand(o *Options) *cobra.Command {
	return srcDstCommand(o, "cp", "Copy a file or folder", "/v1/cp", "Copied")
}

func srcDstCommand(o *Options, use, short, path, verb string) *cobra.Command {
	var overwrite bool
	cmd := &cobra.Command{
		Use:   use + " <source> <destination>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out map[string]any
			if err := client(o).Do(cmd.Context(), "POST", path, map[string]any{
				"src": args[0], "dst": args[1], "overwrite": overwrite,
			}, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			_, _ = fmt.Fprintf(o.Out(), "%s %v to %v\n", verb, out["src"], out["dst"])
			return nil
		},
	}
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace anything already at the destination")
	return cmd
}

func newRenameCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <path> <new-name>",
		Short: "Rename in place",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out map[string]any
			if err := client(o).Do(cmd.Context(), "POST", "/v1/rename", map[string]any{
				"path": args[0], "new_name": args[1],
			}, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			_, _ = fmt.Fprintf(o.Out(), "Renamed to %v\n", out["path"])
			return nil
		},
	}
}

func newRemoveCommand(o *Options) *cobra.Command {
	var permanent bool
	cmd := &cobra.Command{
		Use:     "rm <path>",
		Short:   "Move to the trash, or delete for good",
		Args:    cobra.ExactArgs(1),
		Aliases: []string{"remove"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var out map[string]any
			if err := client(o).Do(cmd.Context(), "POST", "/v1/rm", map[string]any{
				"path": args[0], "permanent": permanent,
			}, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			_, _ = fmt.Fprintln(o.Out(), detailOf(out, "Removed."))
			return nil
		},
	}
	cmd.Flags().BoolVar(&permanent, "permanent", false,
		"delete outright instead of using the trash; this cannot be undone")
	return cmd
}

func newTrashCommand(o *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "See or empty the trash",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out struct {
				Entries []entry `json:"entries"`
			}
			if err := client(o).Do(cmd.Context(), "GET", "/v1/trash", nil, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			if len(out.Entries) == 0 {
				_, _ = fmt.Fprintln(o.Out(), "The trash is empty.")
				return nil
			}
			for _, e := range out.Entries {
				_, _ = fmt.Fprintf(o.Out(), "%s\t%s\n", e.Name, humanBytes(e.Size))
			}
			return nil
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "empty",
		Short: "Delete everything in the trash for good",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out map[string]any
			if err := client(o).Do(cmd.Context(), "POST", "/v1/trash/empty", nil, &out); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(o.Out(), detailOf(out, "The trash is empty."))
			return nil
		},
	})
	return cmd
}

func newVersionsCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "versions <path>",
		Short: "List earlier versions of a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				Path     string           `json:"path"`
				Versions []map[string]any `json:"versions"`
			}
			if err := client(o).Do(cmd.Context(), "GET",
				query("/v1/versions", "path", args[0]), nil, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			if len(out.Versions) == 0 {
				_, _ = fmt.Fprintf(o.Out(), "%s has no earlier versions.\n", out.Path)
				return nil
			}
			w := tabwriter.NewWriter(o.Out(), 0, 0, 2, ' ', 0)
			for _, v := range out.Versions {
				_, _ = fmt.Fprintf(w, "%v\t%v\n", v["version"], v["name"])
			}
			return w.Flush()
		},
	}
}

// ---------------------------------------------------------------- sharing

func newShareCommand(o *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share <path>",
		Short: "Make a link anyone can use",
		Args:  cobra.ExactArgs(1),
	}

	var expires string
	var maxUses int
	create := &cobra.Command{
		Use:   "link <path>",
		Short: "Create a share link",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"path": args[0]}
			if expires != "" {
				body["expires_at"] = expires
			}
			if maxUses > 0 {
				body["max_uses"] = maxUses
			}
			var out map[string]any
			if err := client(o).Do(cmd.Context(), "POST", "/v1/share/link", body, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			_, _ = fmt.Fprintf(o.Out(), "%v\n", out["url"])
			_, _ = fmt.Fprintf(o.Out(), "Anyone with this link can open %v until %v.\n", out["path"], out["expires_at"])
			return nil
		},
	}
	create.Flags().StringVar(&expires, "expires", "",
		"last day the link works, as 2026-12-31 (OpenDrive takes a date, not a time)")
	create.Flags().IntVar(&maxUses, "max-uses", 0, "stop the link working after this many opens")

	list := &cobra.Command{
		Use:   "list <path>",
		Short: "Show the link on a path, if there is one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				Path   string           `json:"path"`
				Shares []map[string]any `json:"shares"`
			}
			if err := client(o).Do(cmd.Context(), "GET",
				query("/v1/share/list", "path", args[0]), nil, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			if len(out.Shares) == 0 {
				_, _ = fmt.Fprintf(o.Out(), "%s is not shared.\n", out.Path)
				return nil
			}
			for _, s := range out.Shares {
				_, _ = fmt.Fprintf(o.Out(), "%v (until %v)\n", s["url"], s["expires_at"])
			}
			return nil
		},
	}

	revoke := &cobra.Command{
		Use:   "revoke <path>",
		Short: "Stop a share link working",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out map[string]any
			if err := client(o).Do(cmd.Context(), "DELETE",
				query("/v1/share", "path", args[0]), nil, &out); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(o.Out(), detailOf(out, "The link no longer works."))
			return nil
		},
	}

	cmd.RunE = create.RunE
	cmd.Flags().AddFlagSet(create.Flags())
	cmd.AddCommand(create, list, revoke)
	return cmd
}

// ---------------------------------------------------------------- helpers

// client returns whatever the command should talk to: the daemon, or the
// bridge running inside this process when --direct was given.
//
// Both are the same server behind the same client, so nothing downstream needs
// to know which one it got.
func client(o *Options) *Client {
	if o.direct != nil {
		return o.direct.client
	}
	return NewClient(o.Addr, o.APIKey, o.Timeout)
}

func printJSON(o *Options, v any) error {
	enc := json.NewEncoder(o.Out())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// detailOf prints the daemon's own sentence when it sent one. The daemon is
// where the wording lives, so odctl repeats it rather than inventing a second.
func detailOf(out map[string]any, fallback string) string {
	if d, ok := out["detail"].(string); ok && d != "" {
		return d
	}
	return fallback
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func localName(remote string) string {
	return filepath.Base(strings.TrimSuffix(remote, "/"))
}

// absLocal makes a local path absolute, because the daemon resolves it in its
// own working directory, which is rarely the user's.
func absLocal(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", usage("Cannot work out the full path of %s: %v", p, err)
	}
	return abs, nil
}

func fileSize(p string) (int64, error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, usage("There is no readable file at %s.", p)
	}
	if info.IsDir() {
		return 0, usage("%s is a folder. Upload files one at a time.", p)
	}
	return info.Size(), nil
}
