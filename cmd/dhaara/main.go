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
	exitStopped  = 130
)

const usageText = `dhaara: keep a folder in step between computers, Google Drive and a phone.

Check for changes (like apt update; these change nothing, they save a plan):
  dhaara sync [options] FOLDER                         two-way: this folder <-> Drive
  dhaara pull [options] DRIVE_FOLDER [LOCAL_FOLDER]    Drive -> this laptop, following removals
  dhaara phone [options] DRIVE_FOLDER [LOCAL_FOLDER]   Drive -> phone over adb, data from LOCAL_FOLDER

Review and carry out the plans (like apt upgrade):
  dhaara pending [FOLDER]                              show what's waiting, in full
  dhaara apply [options] [FOLDER]                      ask, then make exactly those changes

Direct commands:
  dhaara snapshot [options] DRIVE_FOLDER               full copy on Drive, made on Drive
  dhaara [upload] [options] FOLDER                     add and update only, never removes
  dhaara download [options] DRIVE_FOLDER [LOCAL_FOLDER]

DRIVE_FOLDER is a path from My Drive, like "Reports" or "Work/Reports". By
default sync and upload use the local folder's name at the top of My Drive.

Examples:
  dhaara sync ~/Reports                              check what differs (changes nothing)
  dhaara pending                                     read the plan
  dhaara apply                                       confirm and carry it out
  dhaara sync ~/Reports --dest-name Work/Reports     sync with a folder inside Work
  dhaara pull Work/Reports ~/Reports                 on another laptop
  dhaara phone Work/Reports                          into /sdcard/Reports, files from ~/Reports
  dhaara snapshot Work/Reports                       copy into "Work/Reports snapshots/<date>"

Exclusions: one pattern per line in ~/.config/dhaara/exclude, plus --exclude.
A pattern without "/" matches a file or folder name anywhere ("*.tmp", "node_modules");
with "/" it matches the path from the top ("build/", "projects/*/cache").
Excluded files are left alone everywhere: never sent, fetched or removed.

Options:
`

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-version" || args[0] == "version") {
		fmt.Println("dhaara", version())
		return exitOK
	}
	cfg, err := parseFlags(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return exitFatal
	}
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

	if cfg.Mode == "apply" && cfg.Confirm == nil && !cfg.Yes {
		ui.Println("ERROR: not running in a terminal, so dhaara can't ask before applying. Add --yes to apply without asking.")
		return exitFatal
	}
	changes := cfg.Mode == "apply" || cfg.Mode == "upload" || cfg.Mode == "download" || cfg.Mode == "snapshot"
	if changes && !cfg.NoInhibit && !cfg.VerifyOnly && !cfg.DryRun {
		release := platform.InhibitSleep()
		defer release()
	}

	switch cfg.Mode {
	case "download", "pull":
		_, err = engine.Download(ctx, cfg, ui)
	case "sync":
		_, err = engine.Sync(ctx, cfg, ui)
	case "snapshot":
		err = engine.Snapshot(ctx, cfg, ui)
	case "phone":
		err = engine.Phone(ctx, cfg, ui)
	case "pending":
		err = engine.Pending(ctx, cfg, ui)
	case "apply":
		err = engine.Apply(ctx, cfg, ui)
	default:
		_, err = engine.Upload(ctx, cfg, ui)
	}
	code := exitCode(ctx, err, ui)
	ui.Printf("Log file: %s", logPath)
	return code
}

func exitCode(ctx context.Context, err error, ui *term.Console) int {
	ui.ClearStatus()
	switch {
	case err == nil:
		return exitOK
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

func parseFlags(args []string, stderr io.Writer) (engine.Config, error) {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".config", "dhaara")
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		configDir = filepath.Join(x, "dhaara")
	}
	stateDir := filepath.Join(home, ".local", "state", "dhaara")
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		stateDir = filepath.Join(x, "dhaara")
	}
	cfg := engine.Config{
		Mode:         "upload",
		ConfigDir:    configDir,
		StateDir:     stateDir,
		Endpoints:    gdrive.GoogleEndpoints,
		Retry:        gdrive.DefaultRetry,
		PaceInterval: 100 * time.Millisecond, // at most 10 requests/second, well under Google's limits
		Trash:        platform.Trash,
	}

	if len(args) > 0 {
		switch args[0] {
		case "upload", "download", "sync", "pull", "snapshot", "phone", "pending", "apply":
			cfg.Mode = args[0]
			args = args[1:]
		}
	}
	cfg.Download = cfg.Mode == "download" || cfg.Mode == "pull"
	cfg.Pull = cfg.Mode == "pull"

	fs := flag.NewFlagSet("dhaara", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText); fs.PrintDefaults() }
	fs.StringVar(&cfg.RemoteName, "dest-name", "", "upload, sync: Drive folder path, like Work/Reports (default: the local folder's name)")
	fs.IntVar(&cfg.Workers, "workers", 3, "files transferred at the same time, 1-8")
	chunkMB := fs.Int("chunk-mb", 8, "upload, sync: chunk size in MiB, 1-256; smaller files go up in a single request")
	fs.BoolVar(&cfg.VerifyOnly, "verify-only", false, "upload: change nothing; check Drive against the local folder")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "upload, download, snapshot: change nothing; show what would be done")
	fs.BoolVar(&cfg.Yes, "yes", false, "apply: don't ask before making the changes")
	fs.BoolVar(&cfg.Overwrite, "overwrite", false, "download, pull: replace local files that differ from Drive")
	fs.BoolVar(&cfg.NoInhibit, "no-inhibit", false, "let the laptop sleep during the transfer")
	fs.BoolVar(&cfg.AllowManyRemovals, "allow-many-removals", false, "apply: allow removing more than a quarter of a side's files")
	fs.Func("exclude", "leave out files matching this pattern (repeatable; see above)", func(v string) error {
		cfg.Exclude = append(cfg.Exclude, v)
		return nil
	})
	excludeFile := fs.String("exclude-file", filepath.Join(configDir, "exclude"), "file with exclusion patterns, one per line")
	fs.StringVar(&cfg.PhoneDir, "phone-dir", "", "phone: folder on the phone (default /sdcard/<last part of DRIVE_FOLDER>)")
	fs.StringVar(&cfg.ADB, "adb", "", "phone: path to adb (default: found on PATH or in ~/Android/Sdk)")
	fs.StringVar(&cfg.ClientSecret, "client-secret", filepath.Join(configDir, "client_secret.json"),
		"Google OAuth client file (type: Desktop app)")

	// Allow options before or after the folder names.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return cfg, err
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
		"dest-name":           {"upload", "sync"},
		"chunk-mb":            {"upload", "sync"},
		"verify-only":         {"upload"},
		"overwrite":           {"download", "pull"},
		"allow-many-removals": {"apply"},
		"yes":                 {"apply"},
		"dry-run":             {"upload", "download", "snapshot"},
		"phone-dir":           {"phone"},
		"adb":                 {"phone"},
	}
	for name, modes := range allowed {
		if set[name] && !slices.Contains(modes, cfg.Mode) {
			return cfg, fmt.Errorf("--%s is only for %s", name, strings.Join(modes, ", "))
		}
	}

	switch cfg.Mode {
	case "download", "pull", "phone":
		if len(positional) < 1 || len(positional) > 2 {
			fs.Usage()
			return cfg, fmt.Errorf("usage: dhaara %s DRIVE_FOLDER [LOCAL_FOLDER]", cfg.Mode)
		}
		cfg.RemoteName = positional[0]
		switch {
		case len(positional) == 2:
			cfg.Source = expandHome(positional[1], home)
		case cfg.Mode == "phone":
			cfg.Source = filepath.Join(home, path.Base(strings.Trim(cfg.RemoteName, "/")))
		default:
			cfg.Source = path.Base(strings.Trim(cfg.RemoteName, "/"))
		}
		if cfg.Mode == "phone" && cfg.PhoneDir == "" {
			cfg.PhoneDir = "/sdcard/" + path.Base(strings.Trim(cfg.RemoteName, "/"))
		}
	case "pending", "apply":
		if len(positional) > 1 {
			fs.Usage()
			return cfg, fmt.Errorf("usage: dhaara %s [FOLDER]", cfg.Mode)
		}
		if len(positional) == 1 {
			cfg.Source = expandHome(positional[0], home)
		}
	case "snapshot":
		if len(positional) != 1 {
			fs.Usage()
			return cfg, errors.New("usage: dhaara snapshot DRIVE_FOLDER")
		}
		cfg.RemoteName = positional[0]
	default: // upload, sync
		if len(positional) != 1 {
			fs.Usage()
			return cfg, errors.New("give exactly one folder")
		}
		cfg.Source = expandHome(positional[0], home)
	}
	if cfg.PhoneDir != "" && !strings.HasPrefix(cfg.PhoneDir, "/") {
		return cfg, errors.New("--phone-dir must be a full path, like /sdcard/Reports")
	}
	if cfg.RemoteName != "" {
		if _, err := gdrive.SplitDrivePath(cfg.RemoteName); err != nil {
			return cfg, err
		}
	}
	if cfg.Workers < 1 || cfg.Workers > 8 {
		return cfg, errors.New("--workers must be between 1 and 8")
	}
	if *chunkMB < 1 || *chunkMB > 256 {
		return cfg, errors.New("--chunk-mb must be between 1 and 256")
	}
	if cfg.VerifyOnly && cfg.DryRun {
		return cfg, errors.New("use either --verify-only or --dry-run, not both")
	}
	cfg.ChunkSize = int64(*chunkMB) << 20 // a multiple of 256 KiB, as Google requires

	fromFile, err := exclude.ReadFile(*excludeFile)
	if err != nil {
		return cfg, fmt.Errorf("cannot read %s: %v", *excludeFile, err)
	}
	cfg.Exclude = append(fromFile, cfg.Exclude...)
	if err := exclude.Check(cfg.Exclude); err != nil {
		return cfg, err
	}
	return cfg, nil
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
