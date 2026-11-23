// Command dhaara keeps a folder in step between this computer, Google Drive
// and an Android phone.
//
//   - Go standard library only: no third-party code.
//   - Uses Google's resumable upload API, so a Wi-Fi drop, server error or
//     rate limit only costs a retry, never the whole file. Downloads resume too.
//   - Safe to stop (Ctrl+C) and run again: finished files are skipped and
//     half-done files continue where they stopped.
//   - Every file is checked against Google's MD5 checksum, and the whole folder
//     is verified again at the end.
//   - Asks only for the "drive.file" permission: it can see and change only the
//     files it created itself, never the rest of your Drive.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/snehilshah/dhaara/internal/engine"
	"github.com/snehilshah/dhaara/internal/exclude"
	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/platform"
	"github.com/snehilshah/dhaara/internal/term"
)

const (
	exitOK       = 0
	exitProblems = 1
	exitFatal    = 2
	exitPending  = 3 // status --exit-code: changes are waiting
	exitStopped  = 130
)

const usageText = `dhaara: keep a folder in step between computers, Google Drive and a phone.

Set up once per device (a "pairing" of a folder here and one on Drive):
  dhaara add NAME FOLDER drive:PATH            two-way: this folder <-> Drive
  dhaara add NAME FOLDER drive:PATH --follow   this folder only takes Drive's changes
  dhaara add NAME phone:/sdcard/PATH           a phone follows Drive (files from FOLDER, over adb)
  dhaara forget NAME [phone:/sdcard/PATH]      take it out again (touches no files)

Day to day (like apt update and apt upgrade):
  dhaara status [NAME]                         check everything; changes nothing
  dhaara apply [NAME]                          check again, show the plan, ask, then do it

One-off commands:
  dhaara upload [options] FOLDER               add and update on Drive, never removes
  dhaara download [options] DRIVE_PATH [FOLDER]
  dhaara snapshot [options] DRIVE_PATH         full copy, made on Drive

A Drive path starts at My Drive, like "Reports" or "Work/Reports".

Examples:
  dhaara add music ~/Music drive:Music
  dhaara add music phone:/sdcard/Music
  dhaara status                                what would change, everywhere
  dhaara status -v                             ... every step, and how it was worked out
  dhaara apply                                 carry it out: Drive first, then the phone
  dhaara apply --yes                           exactly what the last status showed, without asking

Exclusions: one pattern per line in ~/.config/dhaara/exclude (every folder),
plus --exclude when adding a pairing (that pairing only) or for one-off commands.
A pattern without "/" matches a file or folder name anywhere ("*.tmp", "node_modules");
with "/" it matches the path from the top ("build/", "projects/*/cache").
Excluded files are left alone everywhere: never sent, fetched or removed.

Options:
`

// replaced tells users of the old commands what to run now.
var replaced = map[string]string{
	"sync":    "dhaara add NAME FOLDER drive:PATH once, then dhaara status and dhaara apply",
	"pull":    "dhaara add NAME FOLDER drive:PATH --follow once, then dhaara status and dhaara apply",
	"phone":   "dhaara add NAME phone:/sdcard/PATH once (to a pairing), then dhaara status and dhaara apply",
	"pending": "dhaara status (or dhaara status -v for every step)",
}

// command is what the user asked for.
type command struct {
	cfg      engine.Config
	name     string   // pairing name (add, forget, status, apply)
	args     []string // add, forget: locations
	verbose  bool
	exitCode bool
	add      engine.AddOptions
}

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-version" || args[0] == "version") {
		fmt.Println("dhaara", version())
		return exitOK
	}
	c, err := parseFlags(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return exitFatal
	}
	cfg := c.cfg
	ui := term.NewConsole(os.Stderr)

	logPath, closeLog, err := setupLog(cfg.StateDir, cfg.Mode)
	if err != nil {
		ui.Printf("ERROR: cannot create log file: %v", err)
		return exitFatal
	}
	defer closeLog()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		ui.Println("\nStopping... (press Ctrl+C again to quit at once)")
		cancel()
		<-sigs
		os.Exit(exitStopped)
	}()

	changes := cfg.Mode == "apply" || cfg.Mode == "upload" || cfg.Mode == "download" || cfg.Mode == "snapshot"
	if changes && !cfg.NoInhibit && !cfg.VerifyOnly && !cfg.DryRun {
		release := platform.InhibitSleep()
		defer release()
	}

	switch cfg.Mode {
	case "add":
		err = engine.Add(ctx, cfg, ui, c.name, c.args, c.add)
	case "forget":
		err = engine.Forget(cfg, ui, c.name, c.args)
	case "status":
		err = engine.Status(ctx, cfg, ui, c.name, c.verbose, c.exitCode)
	case "apply":
		err = engine.ApplyPairings(ctx, cfg, ui, c.name)
	case "download":
		_, err = engine.Download(ctx, cfg, ui)
	case "snapshot":
		err = engine.Snapshot(ctx, cfg, ui)
	default:
		_, err = engine.Upload(ctx, cfg, ui)
	}
	code := exitCode(ctx, err, ui)
	if code != exitOK || c.verbose || changes {
		ui.Printf("Log file: %s", logPath)
	}
	return code
}

func exitCode(ctx context.Context, err error, ui *term.Console) int {
	ui.ClearStatus()
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, engine.ErrPending):
		return exitPending
	case errors.Is(err, engine.ErrProblems):
		return exitProblems // details were already printed
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		ui.Println("Stopped. Run the same command again to continue where it left off.")
		return exitStopped
	default:
		ui.Printf("\nERROR: %v", err)
		slog.Error("fatal", "err", err)
		return exitFatal
	}
}

func parseFlags(args []string, stderr io.Writer) (command, error) {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".config", "dhaara")
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		configDir = filepath.Join(x, "dhaara")
	}
	stateDir := filepath.Join(home, ".local", "state", "dhaara")
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		stateDir = filepath.Join(x, "dhaara")
	}
	c := command{cfg: engine.Config{
		ConfigDir:    configDir,
		StateDir:     stateDir,
		Endpoints:    gdrive.GoogleEndpoints,
		Retry:        gdrive.DefaultRetry,
		PaceInterval: 100 * time.Millisecond, // at most 10 requests/second, well under Google's limits
		Trash:        platform.Trash,
	}}
	cfg := &c.cfg

	usage := func() { fmt.Fprint(stderr, usageText) }
	if len(args) == 0 {
		usage()
		return c, errors.New("say what to do, like: dhaara status")
	}
	switch cmd := args[0]; cmd {
	case "add", "forget", "status", "apply", "upload", "download", "snapshot":
		cfg.Mode = cmd
		args = args[1:]
	case "help", "-h", "-help", "--help":
		usage()
		return c, flag.ErrHelp
	default:
		if hint, ok := replaced[cmd]; ok {
			return c, fmt.Errorf("dhaara %s is replaced by pairings: %s. See dhaara help", cmd, hint)
		}
		if strings.HasPrefix(cmd, "-") {
			return c, fmt.Errorf("say what to do before the options, like: dhaara status %s", cmd)
		}
		if strings.ContainsAny(cmd, "/~.") {
			return c, fmt.Errorf("unknown command %q. To upload a folder: dhaara upload %s", cmd, cmd)
		}
		return c, fmt.Errorf("unknown command %q. See dhaara help", cmd)
	}
	cfg.Download = cfg.Mode == "download"

	fs := flag.NewFlagSet("dhaara "+cfg.Mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(); fs.PrintDefaults() }
	fs.BoolVar(&c.add.Follow, "follow", false, "add: this folder only takes Drive's changes, never sends any")
	fs.StringVar(&c.add.Serial, "serial", "", "add: which phone, as adb devices lists it (when several are connected)")
	fs.BoolVar(&c.verbose, "v", false, "status: show every step, and how each place was checked")
	fs.BoolVar(&c.verbose, "verbose", false, "status: same as -v")
	fs.BoolVar(&c.exitCode, "exit-code", false, "status: exit with 3 when changes are pending")
	fs.BoolVar(&cfg.Yes, "yes", false, "apply: don't check again or ask; do exactly what the last status saved")
	fs.BoolVar(&cfg.AllowManyRemovals, "allow-many-removals", false, "apply: allow removing more than a quarter of a place's files")
	fs.StringVar(&cfg.RemoteName, "dest-name", "", "upload: Drive path, like Work/Reports (default: the folder's name)")
	fs.IntVar(&cfg.Workers, "workers", 3, "files transferred at the same time, 1-8")
	chunkMB := fs.Int("chunk-mb", 8, "upload, apply: chunk size in MiB, 1-256; smaller files go up in a single request")
	fs.BoolVar(&cfg.VerifyOnly, "verify-only", false, "upload: change nothing; check Drive against the folder")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "upload, download, snapshot: change nothing; show what would be done")
	fs.BoolVar(&cfg.Overwrite, "overwrite", false, "download: replace local files that differ from Drive")
	fs.BoolVar(&cfg.NoInhibit, "no-inhibit", false, "let the laptop sleep during transfers")
	var flagExclude []string
	fs.Func("exclude", "leave out files matching this pattern (repeatable; see above)", func(v string) error {
		flagExclude = append(flagExclude, v)
		return nil
	})
	excludeFile := fs.String("exclude-file", filepath.Join(configDir, "exclude"), "file with exclusion patterns, one per line")
	fs.StringVar(&cfg.ADB, "adb", "", "status, apply: path to adb (default: found on PATH or in ~/Android/Sdk)")
	fs.StringVar(&cfg.ClientSecret, "client-secret", filepath.Join(configDir, "client_secret.json"),
		"Google OAuth client file (type: Desktop app)")

	// Allow options before or after the other arguments.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return c, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if term.IsTerminal(os.Stdin) {
		cfg.Confirm = askYesNo
	}
	allowed := map[string][]string{ // options that only some commands take
		"follow":              {"add"},
		"serial":              {"add"},
		"v":                   {"status"},
		"verbose":             {"status"},
		"exit-code":           {"status"},
		"yes":                 {"apply"},
		"allow-many-removals": {"apply"},
		"dest-name":           {"upload"},
		"chunk-mb":            {"upload", "apply"},
		"verify-only":         {"upload"},
		"dry-run":             {"upload", "download", "snapshot"},
		"overwrite":           {"download"},
		"exclude":             {"add", "upload", "download", "snapshot"},
		"adb":                 {"add", "status", "apply"},
		"workers":             {"upload", "download", "snapshot", "apply"},
	}
	for name, modes := range allowed {
		if set[name] && !slices.Contains(modes, cfg.Mode) {
			return c, fmt.Errorf("--%s is only for %s", name, strings.Join(modes, ", "))
		}
	}

	need := func(lo, hi int, form string) error {
		if len(positional) < lo || len(positional) > hi {
			fs.Usage()
			return fmt.Errorf("usage: dhaara %s %s", cfg.Mode, form)
		}
		return nil
	}
	switch cfg.Mode {
	case "add":
		if err := need(2, 8, "NAME FOLDER drive:PATH [phone:/sdcard/PATH] [--follow], or NAME phone:/sdcard/PATH"); err != nil {
			return c, err
		}
		c.name = positional[0]
		for _, a := range positional[1:] {
			c.args = append(c.args, expandHome(a, home))
		}
		c.add.Exclude = flagExclude
		flagExclude = nil
		if err := exclude.Check(c.add.Exclude); err != nil {
			return c, err
		}
	case "forget":
		if err := need(1, 8, "NAME [phone:/sdcard/PATH]"); err != nil {
			return c, err
		}
		c.name, c.args = positional[0], positional[1:]
	case "status", "apply":
		if err := need(0, 1, "[NAME]"); err != nil {
			return c, err
		}
		if len(positional) == 1 {
			c.name = positional[0]
		}
	case "download":
		if err := need(1, 2, "DRIVE_PATH [FOLDER]"); err != nil {
			return c, err
		}
		cfg.RemoteName = strings.TrimPrefix(positional[0], "drive:")
		cfg.Source = path.Base(strings.Trim(cfg.RemoteName, "/"))
		if len(positional) == 2 {
			cfg.Source = expandHome(positional[1], home)
		}
	case "snapshot":
		if err := need(1, 1, "DRIVE_PATH"); err != nil {
			return c, err
		}
		cfg.RemoteName = strings.TrimPrefix(positional[0], "drive:")
	default: // upload
		if err := need(1, 1, "FOLDER"); err != nil {
			return c, err
		}
		cfg.Source = expandHome(positional[0], home)
	}
	if cfg.RemoteName != "" {
		if _, err := gdrive.SplitDrivePath(cfg.RemoteName); err != nil {
			return c, err
		}
	}
	if cfg.Workers < 1 || cfg.Workers > 8 {
		return c, errors.New("--workers must be between 1 and 8")
	}
	if *chunkMB < 1 || *chunkMB > 256 {
		return c, errors.New("--chunk-mb must be between 1 and 256")
	}
	if cfg.VerifyOnly && cfg.DryRun {
		return c, errors.New("use either --verify-only or --dry-run, not both")
	}
	cfg.ChunkSize = int64(*chunkMB) << 20 // a multiple of 256 KiB, as Google requires

	fromFile, err := exclude.ReadFile(*excludeFile)
	if err != nil {
		return c, fmt.Errorf("cannot read %s: %v", *excludeFile, err)
	}
	cfg.Exclude = append(fromFile, flagExclude...)
	if err := exclude.Check(cfg.Exclude); err != nil {
		return c, err
	}
	return c, nil
}

// askYesNo asks on the terminal; only "y" or "yes" counts as yes.
func askYesNo(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// version is the module version "go install ...@v0.0.1" stamps into the
// program, or "(devel)" for a local build.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func setupLog(stateDir string, kind string) (string, func(), error) {
	dir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	if kind == "" {
		kind = "upload"
	}
	path := filepath.Join(dir, time.Now().Format(kind+"-20060102-150405.log"))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", nil, err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	return path, func() { f.Close() }, nil
}
