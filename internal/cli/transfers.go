package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// job mirrors the /v1/jobs schema. The daemon owns that shape; this is a read
// of it, not a second definition.
type job struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	// Phase is which leg of the transfer is running (§4.4.1). With the caching
	// gateway in front, an upload has two — this machine to the gateway, then the
	// gateway to OpenDrive — and one progress figure covering both would be wrong
	// in whichever direction it was wrong.
	Phase      string  `json:"phase"`
	LocalPath  string  `json:"local_path"`
	RemotePath string  `json:"remote_path"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
	Speed      float64 `json:"speed"`
	Attempts   int     `json:"attempts"`
	Error      *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func (j job) terminal() bool {
	return j.State == "succeeded" || j.State == "failed" || j.State == "cancelled"
}

func newUploadCommand(o *Options) *cobra.Command {
	var overwrite, wait bool
	cmd := &cobra.Command{
		Use:     "up <local-file> <remote-path>",
		Short:   "Upload a file",
		Args:    remoteArgs(cobra.ExactArgs(2), 1),
		Aliases: []string{"upload", "put"},
		RunE: func(cmd *cobra.Command, args []string) error {
			local, err := absLocal(args[0])
			if err != nil {
				return err
			}
			size, err := fileSize(local)
			if err != nil {
				return err
			}

			var j job
			if err := client(o).Do(cmd.Context(), "POST", "/v1/upload", map[string]any{
				"local_path": local, "remote_path": args[1], "overwrite": overwrite,
			}, &j); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, j)
			}
			if !wait {
				_, _ = fmt.Fprintf(o.Out(), "Uploading %s (%s). Follow it with: odctl jobs %s\n",
					args[1], humanBytes(size), j.ID)
				return nil
			}
			return follow(cmd.Context(), o, j.ID, "Uploading "+localName(args[1]))
		},
	}
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace anything already there")
	cmd.Flags().BoolVar(&wait, "wait", true, "stay until the transfer finishes")
	return cmd
}

func newDownloadCommand(o *Options) *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:     "down <remote-path> [local-file]",
		Short:   "Download a file",
		Args:    remoteArgs(cobra.RangeArgs(1, 2), 0),
		Aliases: []string{"download", "get"},
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := localName(args[0])
			if len(args) == 2 {
				dest = args[1]
			}
			local, err := absLocal(dest)
			if err != nil {
				return err
			}

			var j job
			if err := client(o).Do(cmd.Context(), "POST", "/v1/download", map[string]any{
				"remote_path": args[0], "local_path": local,
			}, &j); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, j)
			}
			if !wait {
				_, _ = fmt.Fprintf(o.Out(), "Downloading to %s. Follow it with: odctl jobs %s\n", local, j.ID)
				return nil
			}
			if err := follow(cmd.Context(), o, j.ID, "Downloading "+localName(args[0])); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(o.Out(), "Saved to %s\n", local)
			return nil
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", true, "stay until the transfer finishes")
	return cmd
}

func newJobsCommand(o *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs [id]",
		Short: "See transfers in progress",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				var j job
				if err := client(o).Do(cmd.Context(), "GET", "/v1/jobs/"+args[0], nil, &j); err != nil {
					return err
				}
				if o.JSON {
					return printJSON(o, j)
				}
				_, _ = fmt.Fprintln(o.Out(), describe(j))
				if j.Error != nil {
					_, _ = fmt.Fprintln(o.Out(), j.Error.Message)
				}
				return nil
			}

			var out struct {
				Jobs []job `json:"jobs"`
			}
			if err := client(o).Do(cmd.Context(), "GET", "/v1/jobs", nil, &out); err != nil {
				return err
			}
			if o.JSON {
				return printJSON(o, out)
			}
			if len(out.Jobs) == 0 {
				_, _ = fmt.Fprintln(o.Out(), "No transfers.")
				return nil
			}
			for _, j := range out.Jobs {
				_, _ = fmt.Fprintf(o.Out(), "%s  %s\n", j.ID, describe(j))
			}
			return nil
		},
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "cancel <id>",
		Short: "Stop a transfer",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var j job
			if err := client(o).Do(cmd.Context(), "DELETE", "/v1/jobs/"+args[0], nil, &j); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(o.Out(), "Stopped. Nothing was left behind in your OpenDrive account.\n")
			return nil
		},
	})
	return cmd
}

func describe(j job) string {
	switch j.State {
	case "succeeded":
		return fmt.Sprintf("%s %s — done (%s)", j.Kind, j.RemotePath, humanBytes(j.BytesTotal))
	case "failed":
		return fmt.Sprintf("%s %s — failed", j.Kind, j.RemotePath)
	case "cancelled":
		return fmt.Sprintf("%s %s — stopped", j.Kind, j.RemotePath)
	case "queued":
		return fmt.Sprintf("%s %s — waiting to start", j.Kind, j.RemotePath)
	default:
		// The phase is only worth saying when it is not the obvious one: "uploading"
		// on an upload tells the reader nothing, but "caching" says why a big file
		// finished its first leg instantly.
		leg := ""
		if j.Phase == "caching" {
			leg = " (copying to the bridge)"
		}
		return fmt.Sprintf("%s %s — %s of %s%s", j.Kind, j.RemotePath,
			humanBytes(j.BytesDone), humanBytes(j.BytesTotal), leg)
	}
}

// follow watches a job to the end, drawing a progress bar from the figures the
// daemon reports. The bar reads bytes_done and speed straight from /v1/jobs
// rather than measuring anything itself: the daemon is doing the transfer, so
// it is the only thing that knows.
func follow(ctx context.Context, o *Options, id, label string) error {
	c := client(o)
	bar := newProgress(o.Out(), label)
	defer bar.done()

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	for {
		var j job
		if err := c.Do(ctx, "GET", "/v1/jobs/"+id, nil, &j); err != nil {
			return err
		}
		bar.update(j.BytesDone, j.BytesTotal, j.Speed)

		if j.terminal() {
			bar.done()
			switch j.State {
			case "succeeded":
				return nil
			case "cancelled":
				return &APIError{Code: "invalid_request", HTTP: 499,
					Message: "The transfer was stopped before it finished."}
			default:
				if j.Error != nil {
					// The daemon's wording, unchanged — and its retry verdict
					// with it, so that a permanent refusal exits differently
					// from a transient one. Without this every failed transfer
					// exited 4, "trying later is reasonable", including the ones
					// where it is not.
					retryable := j.Error.Retryable
					return &APIError{Code: j.Error.Code, HTTP: 502,
						Message: j.Error.Message, Retryable: &retryable}
				}
				return &APIError{Code: "upstream_error", HTTP: 502,
					Message: "The transfer failed and the bridge gave no reason."}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// progress draws a single-line bar, and stays quiet when nothing is watching.
type progress struct {
	out      io.Writer
	label    string
	terminal bool
	finished bool
	lastLen  int
}

func newProgress(out io.Writer, label string) *progress {
	// Only draw a moving bar when a person is there to see it. Redirected to a
	// file or a pipe, the carriage returns would be noise in a log.
	interactive := false
	if f, ok := out.(*os.File); ok {
		if info, err := f.Stat(); err == nil {
			interactive = info.Mode()&os.ModeCharDevice != 0
		}
	}
	p := &progress{out: out, label: label, terminal: interactive}
	if !interactive {
		_, _ = fmt.Fprintf(out, "%s...\n", label)
	}
	return p
}

func (p *progress) update(done, total int64, speed float64) {
	if !p.terminal || p.finished {
		return
	}
	line := p.label + "  "
	if total > 0 {
		pct := float64(done) / float64(total)
		if pct > 1 {
			pct = 1
		}
		const width = 24
		filled := int(pct * width)
		line += "[" + strings.Repeat("=", filled) + strings.Repeat(" ", width-filled) + "] " +
			fmt.Sprintf("%3.0f%%  %s of %s", pct*100, humanBytes(done), humanBytes(total))
	} else {
		line += humanBytes(done)
	}
	if speed > 0 {
		line += fmt.Sprintf("  %s/s", humanBytes(int64(speed)))
	}

	pad := ""
	if n := p.lastLen - len(line); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	_, _ = fmt.Fprintf(p.out, "\r%s%s", line, pad)
	p.lastLen = len(line)
}

func (p *progress) done() {
	if p.finished {
		return
	}
	p.finished = true
	if p.terminal && p.lastLen > 0 {
		_, _ = fmt.Fprintln(p.out)
	}
}
