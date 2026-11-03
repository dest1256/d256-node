// Package cli implements the d256-node command line.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	nodelinkv1 "github.com/dest1256/d256-panel/proto/nodelink/v1"

	"github.com/dest1256/d256-node/internal/enroll"
	"github.com/dest1256/d256-node/internal/link"
	"github.com/dest1256/d256-node/internal/logx"
	"github.com/dest1256/d256-node/internal/state"
	"github.com/dest1256/d256-node/internal/version"
)

const usage = `d256-node - Xray node for d256 panel

Usage:
  d256-node <command> [flags] [arguments]

Commands:
  run                     start the node daemon
  enroll <url> <token>    enroll this node (token "-" reads it from stdin; or set D256_ENROLL_TOKEN)
  status                  show enrollment, panel link, config version, Xray state, journal size
  logs [-n N]             print the last log lines
  reload                  ask the running daemon to reconnect and re-read its state
  reset --yes             delete keys and cache (the node must be enrolled again)
  version                 print versions

Common flags: --state-dir DIR (default $D256_STATE_DIR or /var/lib/d256-node), --log-format text|json (default $D256_LOG_FORMAT)
`

type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
}

// Run executes the command and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return run(args, env{stdin: os.Stdin, stdout: stdout, stderr: stderr})
}

func run(args []string, e env) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintf(e.stdout, "d256-node %s (%s/%s, go %s, protocol %d)\n",
			version.Version, runtime.GOOS, runtime.GOARCH, runtime.Version(), nodelinkv1.ProtocolVersion_PROTOCOL_VERSION_1)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(e.stdout, usage)
		return 0
	case "run", "enroll", "status", "logs", "reload", "reset":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		fs.SetOutput(e.stderr)
		dir := fs.String("state-dir", string(state.DefaultDir()), "state directory")
		logFormat := fs.String("log-format", envOr("D256_LOG_FORMAT", "text"), "text or json")
		opts := &cmdOpts{}
		switch cmd {
		case "enroll":
			fs.BoolVar(&opts.insecureHTTP, "allow-insecure-http", false, "allow an http:// panel URL (development only)")
			fs.BoolVar(&opts.force, "force", false, "overwrite an existing enrollment")
		case "logs":
			fs.IntVar(&opts.lines, "n", 100, "number of lines")
		case "reset":
			fs.BoolVar(&opts.yes, "yes", false, "confirm deletion")
		}
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		opts.dir, opts.logFormat, opts.args = state.Dir(*dir), *logFormat, fs.Args()
		var err error
		switch cmd {
		case "run":
			err = cmdRun(e, opts)
		case "enroll":
			err = cmdEnroll(e, opts)
		case "status":
			err = cmdStatus(e, opts)
		case "logs":
			err = cmdLogs(e, opts)
		case "reload":
			err = cmdReload(e, opts)
		case "reset":
			err = cmdReset(e, opts)
		}
		if err != nil {
			fmt.Fprintf(e.stderr, "d256-node %s: %v\n", cmd, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(e.stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

type cmdOpts struct {
	dir          state.Dir
	logFormat    string
	args         []string
	insecureHTTP bool
	force        bool
	yes          bool
	lines        int
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func cmdEnroll(e env, o *cmdOpts) error {
	if len(o.args) < 1 || len(o.args) > 2 {
		return errors.New("usage: d256-node enroll <panel-url> <token|->")
	}
	token := os.Getenv("D256_ENROLL_TOKEN")
	if len(o.args) == 2 {
		token = o.args[1]
	}
	if token == "-" {
		line, err := bufio.NewReader(e.stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		token = line
	}
	if o.dir.Enrolled() && !o.force {
		return errors.New("this node is already enrolled; use --force to enroll again (the old certificate stops working)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id, err := enroll.Enroll(ctx, o.args[0], token, enroll.Options{AllowInsecureHTTP: o.insecureHTTP})
	if err != nil {
		return err
	}
	if err := o.dir.Save(id); err != nil {
		return fmt.Errorf("store identity: %w", err)
	}
	fmt.Fprintf(e.stdout, "Enrolled as node %d. Panel link: %s\nStart the daemon with: d256-node run\n", id.Info.NodeID, id.Info.GRPCAddr)
	return nil
}

func cmdRun(e env, o *cmdOpts) error {
	if !o.dir.Enrolled() {
		return state.ErrNotEnrolled
	}
	log := logx.New(o.logFormat, o.dir.LogPath())
	if err := os.WriteFile(o.dir.PIDPath(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	defer os.Remove(o.dir.PIDPath())
	log.Info("d256-node starting", "version", version.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := link.New(o.dir, log)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-hup:
				log.Info("reload requested")
				d.Reconnect()
			case <-ctx.Done():
				return
			}
		}
	}()
	err := d.Run(ctx)
	d.Xray.Stop()
	log.Info("d256-node stopped")
	return err
}

func cmdStatus(e env, o *cmdOpts) error {
	id, err := o.dir.Load()
	if err != nil {
		fmt.Fprintln(e.stdout, "enrolled:       no")
		return err
	}
	fmt.Fprintf(e.stdout, "enrolled:       yes (node %d)\npanel:          %s\nlink address:   %s\n", id.Info.NodeID, id.Info.PanelURL, id.Info.GRPCAddr)
	st, err := o.dir.ReadStatus()
	if err != nil || time.Since(time.Unix(st.UpdatedAt, 0)) > 30*time.Second {
		fmt.Fprintln(e.stdout, "daemon:         not running")
		return nil
	}
	link := "disconnected"
	if st.Connected {
		link = fmt.Sprintf("connected (%d ms)", st.LatencyMs)
	}
	fmt.Fprintf(e.stdout, "daemon:         running (pid %d)\npanel link:     %s\n", st.PID, link)
	if st.LastError != "" && !st.Connected {
		fmt.Fprintf(e.stdout, "last error:     %s\n", st.LastError)
	}
	fmt.Fprintf(e.stdout, "config version: %d\nxray:           %s\njournal:        %d bytes\n", st.ConfigVersion, st.XrayState, st.JournalBytes)
	return nil
}

func cmdLogs(e env, o *cmdOpts) error {
	raw, err := os.ReadFile(o.dir.LogPath())
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if o.lines > 0 && len(lines) > o.lines {
		lines = lines[len(lines)-o.lines:]
	}
	fmt.Fprintln(e.stdout, strings.Join(lines, "\n"))
	return nil
}

func cmdReload(e env, o *cmdOpts) error {
	raw, err := os.ReadFile(o.dir.PIDPath())
	if err != nil {
		return errors.New("daemon is not running")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return errors.New("pid file is corrupt")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("signal daemon: %w", err)
	}
	fmt.Fprintln(e.stdout, "reload requested")
	return nil
}

func cmdReset(e env, o *cmdOpts) error {
	if !o.yes {
		return errors.New("this deletes the node's keys and certificate; re-run with --yes")
	}
	if err := o.dir.Reset(); err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, "node state deleted; enroll again with a new token from the panel")
	return nil
}
