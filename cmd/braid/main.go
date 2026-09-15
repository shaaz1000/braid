//go:build darwin

// Command braid downloads a file over every available uplink at once.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"braid/internal/human"
	"braid/internal/linkset"
	"braid/internal/xfer"
)

const usage = `braid — one download, every uplink at once.

usage:
  braid links                 show the uplinks braid can use
  braid get [flags] <url>     download a file over all of them

get flags:
  -o <path>       output directory or file path (default: current directory)
  -chunk <bytes>  chunk size (default 4194304)
  -workers <n>    concurrent fetches per link (default 4)
  -tail-steal     re-request a stalled chunk on an idle link near the end;
                  costs duplicate bytes on a metered link, so it is off by default
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "links":
		err = cmdLinks()
	case "get":
		err = cmdGet(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "braid: %v\n", err)
		os.Exit(1)
	}
}

// discover returns the links braid will actually use.
func discover() ([]linkset.Link, error) {
	raws, err := linkset.System()
	if err != nil {
		return nil, err
	}
	links := linkset.Discover(raws, linkset.FriendlyNames())
	if len(links) == 0 {
		return nil, fmt.Errorf("no usable uplink found; is this machine online?")
	}
	return links, nil
}

func cmdLinks() error {
	links, err := discover()
	if err != nil {
		return err
	}

	fmt.Printf("%-20s %-7s %-5s %-16s %-40s %s\n", "LINK", "IFACE", "IDX", "IPv4", "IPv6", "METERED")
	for _, l := range links {
		fmt.Printf("%-20s %-7s %-5d %-16s %-40s %s\n",
			truncate(l.Label(), 20), l.Iface, l.Index,
			addrOrDash(l, linkset.Fam4), addrOrDash(l, linkset.Fam6),
			yesNo(l.Metered))
	}

	if len(links) == 1 {
		fmt.Printf("\nOnly one uplink, so there is nothing to bond. Attach a second\n" +
			"(tether a phone over USB, or plug in Ethernet) and run this again.\n")
	} else {
		fmt.Printf("\n%d uplinks — a download will be split across all of them.\n", len(links))
	}
	return nil
}

func addrOrDash(l linkset.Link, f linkset.Family) string {
	if a, ok := l.Addr(f); ok {
		return a.String()
	}
	return "—"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dest := fs.String("o", "", "output directory or file path")
	chunk := fs.Int64("chunk", 0, "chunk size in bytes")
	workers := fs.Int("workers", 0, "concurrent fetches per link")
	tailSteal := fs.Bool("tail-steal", false, "re-request a stalled chunk near the end")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("expected exactly one URL")
	}
	url := fs.Arg(0)

	links, err := discover()
	if err != nil {
		return err
	}

	// Ctrl-C should leave the sidecar in place so the transfer can resume,
	// which is what cancelling the context does.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	names := make([]string, 0, len(links))
	for _, l := range links {
		names = append(names, l.Label())
	}
	fmt.Printf("%s\nover %d link(s): %s\n\n", url, len(links), strings.Join(names, ", "))

	opts := xfer.Options{
		URL:            url,
		Dest:           *dest,
		ChunkSize:      *chunk,
		WorkersPerLink: *workers,
		Links:          links,
		ChunkTimeout:   20 * time.Second,
	}
	if *tailSteal {
		opts.TailStealAfter = 5 * time.Second
	}

	started := time.Now()
	interactive := human.IsTerminal(os.Stdout)
	// Piped into a log, carriage returns pile up instead of redrawing, so slow
	// the cadence right down and end each update with a newline.
	painter := &human.Painter{Interval: human.DefaultInterval}
	if !interactive {
		painter.Interval = 2 * time.Second
	}
	opts.OnProgress = func(done, total int, bytesDone int64, _ string) {
		if !painter.Should(done, total, time.Now()) {
			return
		}
		drawProgress(done, total, bytesDone, time.Since(started), interactive)
	}

	out, err := xfer.Get(ctx, opts)
	if interactive {
		fmt.Println()
	}

	if err != nil {
		if out.Result.Bytes > 0 {
			fmt.Printf("stopped after %s — rerun the same command to resume\n", human.Bytes(out.Result.Bytes))
		}
		return err
	}

	elapsed := time.Since(started)
	fmt.Printf("saved %s\n", out.Path)
	fmt.Printf("%s in %s (%s)\n", human.Bytes(out.Size), human.Duration(elapsed),
		human.Rate(out.Result.Bytes, elapsed))

	if out.Resumed {
		fmt.Printf("resumed: %s of it was already on disk\n", human.Bytes(out.Size-out.Result.Bytes))
	}
	if !out.Ranges {
		fmt.Printf("note: this server ignores byte ranges, so it could not be split.\n" +
			"      only one link was used.\n")
	} else {
		printSplit(out, elapsed)
	}
	if out.Result.TailSteals > 0 {
		fmt.Printf("tail steals: %d\n", out.Result.TailSteals)
	}
	return nil
}

// printSplit shows what each link actually contributed, which is the number
// that tells you whether bonding is working.
func printSplit(out xfer.Outcome, elapsed time.Duration) {
	type row struct {
		name   string
		chunks int
	}
	var rows []row
	total := 0
	for name, n := range out.Result.ByLink {
		rows = append(rows, row{name, n})
		total += n
	}
	if total == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].chunks > rows[j].chunks })

	fmt.Println()
	for _, r := range rows {
		share := float64(r.chunks) / float64(total)
		bytes := int64(share * float64(out.Result.Bytes))
		fmt.Printf("  %-8s %s %5.1f%%  %9s  %s\n",
			r.name, human.Bar(int(share*20+0.5), 20), share*100,
			human.Bytes(bytes), human.Rate(bytes, elapsed))
	}
}

func drawProgress(done, total int, bytesDone int64, elapsed time.Duration, interactive bool) {
	pct := 0.0
	if total > 0 {
		pct = float64(done) / float64(total) * 100
	}
	lead, trail := "\r", ""
	if !interactive {
		lead, trail = "", "\n"
	}
	fmt.Printf("%s  %s %5.1f%%  %9s  %11s  eta %-6s%s",
		lead,
		human.Bar(int(float64(done)/float64(max(total, 1))*24+0.5), 24),
		pct, human.Bytes(bytesDone), human.Rate(bytesDone, elapsed),
		human.ETA(int64(done), int64(total), elapsed),
		trail)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
