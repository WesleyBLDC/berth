// berth is the command line client for berthd.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/client"
	"berth/internal/paths"
)

const usage = `usage:
  berth status                 show the daemon's state
  berth debug state <entity>   show an entity's state and recent transitions
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errOut, usage)
		return 2
	}
	home, err := paths.Home()
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, paths.Socket(home), "berth-cli")
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	switch {
	case args[0] == "status":
		return status(ctx, c, out, errOut)
	case len(args) >= 3 && args[0] == "debug" && args[1] == "state":
		fs := flag.NewFlagSet("debug state", flag.ContinueOnError)
		limit := fs.Uint("n", 10, "how many transitions to show, at most 500")
		if err := fs.Parse(args[3:]); err != nil || *limit > 500 {
			return 2
		}
		return debugState(ctx, c, args[2], uint32(*limit), out, errOut) //nolint:gosec // bounded to 500 above
	}
	fmt.Fprint(errOut, usage)
	return 2
}

func status(ctx context.Context, c *client.Client, out, errOut io.Writer) int {
	s, err := c.Daemon.Status(ctx, &berthv1.StatusRequest{})
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "state\t%s (v%d)\n", s.GetState(), s.GetStateVersion())
	fmt.Fprintf(w, "pid\t%d\n", s.GetPid())
	fmt.Fprintf(w, "version\t%s\n", s.GetDaemonVersion())
	fmt.Fprintf(w, "started\t%s\n", s.GetStartedAt().AsTime().Local().Format(time.DateTime))
	fmt.Fprintf(w, "socket\t%s\n", s.GetSocketPath())
	_ = w.Flush()
	return 0
}

func debugState(ctx context.Context, c *client.Client, entity string, limit uint32, out, errOut io.Writer) int {
	s, err := c.Debug.GetState(ctx, &berthv1.GetStateRequest{Entity: entity, HistoryLimit: limit})
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	fmt.Fprintf(out, "%s (%s): %s, version %d\n\n", s.GetEntity(), s.GetKind(), s.GetState(), s.GetVersion())
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "V\tWHEN\tEVENT\tFROM\tTO\tDETAIL")
	for _, t := range s.GetHistory() {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", t.GetVersion(), t.GetAt().AsTime().Local().Format(time.TimeOnly),
			t.GetEvent(), t.GetFromState(), t.GetToState(), t.GetDetail())
	}
	_ = w.Flush()
	return 0
}
