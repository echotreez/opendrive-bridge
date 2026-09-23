package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// `odctl cache` — the command line onto the caching gateway (§3.5).
//
// One of these subcommands exists for a reason more serious than convenience.
// With write-back on, an upload is answered as soon as the bytes are on the
// bridge's disk; for a while after that the bridge is the only place the file
// exists. §3.5.2 rule 3 says the user must be able to see that, and `cache status`
// with its one-line verdict — and `cache flush --wait`, which blocks until there
// is nothing left — are what that means at a terminal.
//
// The verdict is not computed here. The daemon answers `safe_to_shut_down`, and
// this prints it. Deriving it locally from dirty_objects would be one more place
// for the answer to be different, and the answer is the kind that people act on
// by switching a machine off.

// cacheStatus mirrors the /v1/cache/status body (§4.4.1).
type cacheStatus struct {
	Enabled   bool   `json:"enabled"`
	WriteBack bool   `json:"write_back"`
	Dir       string `json:"dir"`

	Bytes    int64 `json:"bytes"`
	MaxBytes int64 `json:"max_bytes"`
	Objects  int   `json:"objects"`

	DirtyBytes     int64   `json:"dirty_bytes"`
	DirtyObjects   int     `json:"dirty_objects"`
	MaxDirtyBytes  int64   `json:"max_dirty_bytes"`
	OldestDirtyAge float64 `json:"oldest_dirty_age_seconds"`

	SafeToShutDown bool `json:"safe_to_shut_down"`

	Hits    int64   `json:"hits"`
	Misses  int64   `json:"misses"`
	HitRate float64 `json:"hit_rate"`

	Durable        bool   `json:"durable"`
	DurabilityNote string `json:"durability_note"`
}

// cacheObject mirrors one entry of /v1/cache/objects.
type cacheObject struct {
	RemotePath    string `json:"remote_path"`
	Size          int64  `json:"size"`
	State         string `json:"state"`
	FlushAttempts int    `json:"flush_attempts"`
	LastError     string `json:"last_error"`
}

func newCacheCommand(o *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "The local cache: what it holds, and what has not been uploaded yet",
		Long: "The bridge can keep a local copy of the files you read, and can accept a\n" +
			"write before OpenDrive has it. While it has not been uploaded, the bridge is\n" +
			"the only place that file exists — `odctl cache status` says how much is in\n" +
			"that state, and whether it is safe to shut the bridge down.",
	}
	cmd.AddCommand(
		newCacheStatusCommand(o),
		newCacheObjectsCommand(o),
		newCacheFlushCommand(o),
		newCacheRefreshCommand(o),
		newCacheClearCommand(o),
	)
	return cmd
}

func newCacheStatusCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "How much is cached, and whether anything is waiting to be uploaded",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var st cacheStatus
			if err := client(o).Do(cmd.Context(), "GET", "/v1/cache/status", nil, &st); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, st)
			}
			out := o.Out()
			if !st.Enabled {
				_, _ = fmt.Fprintln(out, "The cache is switched off on this bridge.")
				return nil
			}

			_, _ = fmt.Fprintf(out, "Cache:     %s\n", st.Dir)
			_, _ = fmt.Fprintf(out, "Holding:   %s of %s in %d file(s)\n",
				humanBytes(st.Bytes), humanBytes(st.MaxBytes), st.Objects)
			if st.Hits+st.Misses > 0 {
				_, _ = fmt.Fprintf(out, "Reads:     %d served locally, %d fetched (%.0f%% local)\n",
					st.Hits, st.Misses, st.HitRate*100)
			}

			if !st.WriteBack {
				_, _ = fmt.Fprintln(out, "\nWrites go straight to OpenDrive, so nothing here is "+
					"waiting to be uploaded.")
				return nil
			}

			_, _ = fmt.Fprintf(out, "\nNot yet on OpenDrive: %s in %d file(s), limit %s\n",
				humanBytes(st.DirtyBytes), st.DirtyObjects, humanBytes(st.MaxDirtyBytes))
			if st.DirtyObjects > 0 && st.OldestDirtyAge > 0 {
				_, _ = fmt.Fprintf(out, "Longest wait:         %s\n",
					(time.Duration(st.OldestDirtyAge) * time.Second).Round(time.Second))
			}

			// The line people are actually here for. Spelled out rather than left
			// as a number to interpret, because the decision it informs is "can I
			// close the lid".
			_, _ = fmt.Fprintln(out)
			if st.SafeToShutDown {
				_, _ = fmt.Fprintln(out, "Everything has reached OpenDrive. It is safe to stop the bridge.")
			} else {
				_, _ = fmt.Fprintf(out, "NOT safe to stop the bridge yet: %s has not reached "+
					"OpenDrive.\nRun `odctl cache flush --wait` to send it now.\n",
					humanBytes(st.DirtyBytes))
			}

			// §8.3.1. Printed last so it is the thing left on screen.
			if !st.Durable && st.DurabilityNote != "" {
				_, _ = fmt.Fprintf(out, "\nWarning: %s\n", st.DurabilityNote)
			}
			return nil
		},
	}
}

func newCacheObjectsCommand(o *Options) *cobra.Command {
	var unsentOnly bool
	cmd := &cobra.Command{
		Use:   "objects",
		Short: "List what is in the cache",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				Objects []cacheObject `json:"objects"`
			}
			if err := client(o).Do(cmd.Context(), "GET", "/v1/cache/objects", nil, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			w := o.Out()
			shown := 0
			for _, obj := range out.Objects {
				unsent := obj.State == "dirty" || obj.State == "uploading"
				if unsentOnly && !unsent {
					continue
				}
				shown++
				// "not uploaded" rather than "dirty": the state name is for the API,
				// and a person reading a list of their own files should be told what
				// it means for them.
				state := "on OpenDrive"
				if unsent {
					state = "NOT uploaded yet"
				}
				_, _ = fmt.Fprintf(w, "%-10s  %-18s  %s\n", humanBytes(obj.Size), state, obj.RemotePath)
				if obj.LastError != "" {
					_, _ = fmt.Fprintf(w, "            last attempt failed: %s\n", obj.LastError)
				}
			}
			if shown == 0 {
				if unsentOnly {
					_, _ = fmt.Fprintln(w, "Nothing is waiting to be uploaded.")
				} else {
					_, _ = fmt.Fprintln(w, "The cache is empty.")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&unsentOnly, "unsent", false,
		"only the files that have not reached OpenDrive yet")
	return cmd
}

func newCacheFlushCommand(o *Options) *cobra.Command {
	var (
		wait bool
		path string
	)
	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Send everything that has not reached OpenDrive yet",
		Long: "With --wait this does not return until there is nothing left to send, which\n" +
			"is the way to be sure before shutting the bridge down or turning the machine\n" +
			"off. Without it, the upload is started and this returns immediately.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"wait": wait}
			if path != "" {
				body["path"] = path
			}
			var st cacheStatus
			if err := client(o).Do(cmd.Context(), "POST", "/v1/cache/flush", body, &st); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, st)
			}
			switch {
			case st.DirtyObjects == 0:
				_, _ = fmt.Fprintln(o.Out(), "Everything has reached OpenDrive. "+
					"It is safe to stop the bridge.")
			case wait:
				// Reached only if the daemon answered while something was still
				// unsent, which should not happen — said plainly rather than
				// reported as success.
				_, _ = fmt.Fprintf(o.Out(), "The flush returned but %s is still not on "+
					"OpenDrive. Check `odctl cache objects --unsent`.\n", humanBytes(st.DirtyBytes))
			default:
				_, _ = fmt.Fprintf(o.Out(), "Uploading: %s in %d file(s). "+
					"Add --wait to block until it is done.\n",
					humanBytes(st.DirtyBytes), st.DirtyObjects)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "block until nothing is left to upload")
	cmd.Flags().StringVar(&path, "path", "", "only this file")
	return cmd
}

func newCacheRefreshCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh <path>",
		Short: "Forget one cached file, so the next read fetches it again",
		Args:  remoteArgs(cobra.ExactArgs(1), 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"path": args[0]}
			if err := client(o).Do(cmd.Context(), "POST", "/v1/cache/refresh", body, nil); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(o.Out(), "Forgotten. The next read of %s will fetch it from "+
				"OpenDrive.\n", args[0])
			return nil
		},
	}
}

func newCacheClearCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Empty the cache of everything OpenDrive already has",
		Long: "Files that have not been uploaded yet are never discarded: if any are\n" +
			"waiting, this refuses and tells you, rather than clearing what it can.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var st cacheStatus
			if err := client(o).Do(cmd.Context(), "DELETE", "/v1/cache", nil, &st); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, st)
			}
			_, _ = fmt.Fprintf(o.Out(), "Cleared. The cache now holds %s.\n", humanBytes(st.Bytes))
			return nil
		},
	}
}
