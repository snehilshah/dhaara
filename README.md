# Dhaara

*Dhaara (धारा): a stream, a current; continuous flow, like a river.*

Keep a folder in step between your computers, Google Drive and an Android
phone: two-way sync, follow-only pull, phone sync over adb, and snapshots
made on Drive. Transfers resume after Ctrl+C or a dropped connection, and
every file is checked by MD5. Go standard library only, no third-party code.

## Install

    go install github.com/snehilshah/dhaara/cmd/dhaara@latest

This puts `dhaara` in `$(go env GOPATH)/bin` (usually `~/go/bin`); make sure
that's on your `PATH` (`dhaara --version` shows the release). Needs Go 1.25 or newer. Linux (it uses `gio trash`
and `systemd-inhibit` when available); `dhaara phone` also needs `adb`.

## Setup (once)

1. Create a project at https://console.cloud.google.com/ and enable the
   **Google Drive API**.
2. In **Google Auth Platform**, set up the consent screen (External), then
   either add your Gmail address as a **Test user** or **Publish app**.
3. Create an OAuth client of type **Desktop app**, download the JSON and save it
   as `~/.config/dhaara/client_secret.json`.

On another computer, copy the same `client_secret.json` there: Drive's
`drive.file` permission is tied to the app, so every computer must use the
same Cloud project and client to see the same files. Never make a new
project for an existing Drive folder; the new one can't see its files.

The first run opens a browser to sign in. dhaara asks only for the
`drive.file` permission, so it can see and change only files it created.

Problems you may hit:

- **"… has not completed the Google verification process … can only be
  accessed by developer-approved testers":** the app is in Testing mode and
  your account isn't a test user. Add it, or publish the app.
- **"… not eligible for designation as a test user":** publish the app
  instead (Google Auth Platform → Audience → Publish app). Sign-in then shows
  an "unverified app" warning: click Advanced → continue. That's expected for
  a personal app; `drive.file` needs no verification.
- gcloud has no command for test users or publishing; use the web console.
- In Testing mode the saved sign-in expires every 7 days. If a sign-in is
  revoked or expired, dhaara deletes `token.json` and stops; the next run
  signs in again.

## Keeping devices in step

One machine (or more) changes the folder and syncs with Drive; Drive is the
main copy; other laptops and an Android phone can follow it without ever
sending anything back. `DRIVE_FOLDER` is a path from My Drive, like
`Reports` or `Work/Reports`.

It works like `apt update` and `apt upgrade`: **checking never changes
anything**, and changes happen only when you apply them.

    dhaara sync ~/Reports                             # check: this folder <-> My Drive/Reports
    dhaara pull Work/Reports ~/Reports                # check, on another laptop: Drive -> here
    dhaara phone Work/Reports                         # check: Drive -> phone, over adb
    dhaara pending                                    # read every pending change, in full
    dhaara apply                                      # asks "Apply these changes? [y/N]", then does them
    dhaara apply --yes                                # ... without asking
    dhaara snapshot Work/Reports                      # full copy on Drive, only when asked

### What each command looks at

| Command | First argument | Second argument | Changes |
|---|---|---|---|
| `sync FOLDER` | the local folder | (none; `--dest-name` picks the Drive folder, default its name at the top of My Drive) | Drive |
| `pull DRIVE_FOLDER [LOCAL]` | the Drive folder to follow | the local folder (default `~/<last part>`) | this computer |
| `phone DRIVE_FOLDER [LOCAL]` | the Drive folder the phone should match | where the files come from (default `~/<last part>`) | the phone, in `--phone-dir` (default `/sdcard/<last part>`) |

So `dhaara phone Work/Reports` makes `/sdcard/Reports` match Drive's
`Work/Reports`, sending the files from `~/Reports` over USB or wireless
debugging. Nothing is downloaded from Drive.

### Plans: check, pending, apply

- A check saves a plan. There's one plan per kind and set of folders (sync
  `~/Reports`, phone `Work/Reports` into `/sdcard/Reports`, ...), and a new
  check of the same replaces the old one. Plans live in
  `~/.local/state/dhaara/plans/`.
- `pending` shows every plan in full. `apply` goes through them oldest first,
  shows each one and asks `[y/N]` **for each plan separately**. Saying no
  skips that plan and keeps it pending; the others still run. `--yes`
  applies them all without asking.
- `apply` reads both sides again and does only the planned steps that still
  hold exactly (same file, same Drive copy, same MD5). Anything that changed
  since the check is skipped and listed, and anything new waits for the
  next check, so nothing happens that you didn't review.
- Order matters when plans depend on each other. A phone or pull plan
  checked before a sync was applied doesn't know what that sync changed on
  Drive, so its new steps are skipped as "appeared since the check". Apply
  the sync, then check the phone or the other laptop again.
- `dhaara apply ~/Reports` (or `pending ~/Reports`) handles only the plans
  whose local folder is `~/Reports`. A sync and a phone plan that both use
  `~/Reports` both match; answer no to the one you want to leave.
- `apply` stops before removing more than a quarter of a side's files (at
  least 20) unless you add `--allow-many-removals`; the check warns first.

### sync

Uploads new and changed files, and moves Drive's copy of anything deleted
here to Drive's trash. A rename or move here is a move on Drive, not a new
upload. It also follows changes made on Drive by another machine's sync. A
file changed on both sides is a conflict: neither copy is touched.
Everything it takes off Drive is first written to a **removed list** (path
and MD5 of each old file) in `dhaara data/` in My Drive. That list is how
pull and phone know a removal was meant.

### pull and phone: following Drive

Laptops that pull and phones only take changes from Drive; they never send
anything back. For each file they hold:

| The file there is | What happens |
|---|---|
| the same as Drive's | left alone |
| missing | copied (or moved, if the same contents sit at a path the removed list says was moved away) |
| different from Drive's | replaced only if that exact old version (path and MD5) is on the removed list; otherwise a conflict, left alone (pull: `--overwrite` replaces it) |
| not on Drive | removed only if that exact file is on the removed list; otherwise it only ever existed there, so it's left alone |

Pull moves removed files to the computer's trash. Phone deletes them, after
checking the file's MD5 on the phone.

**Where phone gets its files, and the checksum check.** Before sending a
file, phone works out the MD5 (a fingerprint of the contents) of the copy on
this computer and compares it with Drive's. Only a match is sent, so the
phone gets exactly the version Drive holds. A file that's missing here or
different (edited after the last sync, or changed on Drive by another
machine) isn't sent: the check lists it as `can't copy (missing or different
in ...)` and ends with problems, and the phone's copy, if any, is left as it
is. Everything else still goes. Fix it by running `dhaara sync` on this
folder (and `apply`), then check the phone again.

Each file arrives on the phone under a temporary name (`.dhaara-push.part`),
its MD5 is checked there, and only then is it renamed into place, so a
broken transfer never looks like a real file.

### snapshot

Copies the whole Drive folder into `<folder> snapshots/<date>` next to it,
on Drive itself: nothing is downloaded or written locally. A stopped
snapshot continues on the next run, and a final pass checks every copy. It
counts against storage.

A snapshot lists the folder once, then copies file by file. Checking
(`sync`, `pull`, `phone`, `pending`) while it runs is fine. Changing the
same Drive folder isn't: `apply`, `upload`, `download` and `snapshot` each
lock the folder, and a second one stops straight away with "another dhaara
run is changing the Drive folder …", changing nothing (its plan stays
pending). Run it again once the first has finished.

### Folders

Folders emptied on Drive are left there (dhaara can't see files other apps
put in them, so it never trashes a folder); pull doesn't recreate folders
that hold no files.

**Exclusions:** one pattern per line in `~/.config/dhaara/exclude`, plus
`--exclude PATTERN`. Without a `/` a pattern matches a file or folder name
anywhere (`*.tmp`, `node_modules`); with a `/` it matches the path from the
top (`build/`, `projects/*/cache`). Excluded files are left alone
everywhere: never uploaded, downloaded, copied to the phone or removed. The
same exclude file applies to every folder.

**What dhaara can see:** it asks only for the `drive.file` permission, so
it sees only files it uploaded itself. Files added to a synced folder from
Drive's website, another app or another person are invisible to it: they
are never downloaded or removed.

## Other commands

    dhaara ~/Reports                 # upload, or continue an earlier upload
    dhaara ~/Reports --dry-run       # show what would be uploaded
    dhaara ~/Reports --verify-only   # check Drive against the local folder

Download (dhaara can only download folders it uploaded):

    dhaara download Reports ~/Downloads/Reports
    dhaara download Reports --dry-run

Downloads resume after a stop, are checked against Drive's MD5 before being
put in place (never a half-written file under the real name), get their
original modified time back, and skip files that are already identical.
Local files that differ from Drive are left alone unless you add
`--overwrite`. Unsafe names on Drive (like `..` or names with `/`) are skipped.

Uploading again adds new files and replaces changed ones (the old copy goes to
Drive's trash). It does not remove files from Drive that you deleted or
renamed locally; `dhaara sync` does.

Run the same command again at any time; finished files are skipped and
half-uploaded files continue where they stopped. Options: `dhaara --help`.

## How it works

- Files up to 8 MiB go up in one request; larger files in 8 MiB chunks over a
  resumable session. After a failed chunk, dhaara asks Google how much
  arrived and continues from there.
- Retries network errors, rate limits and server errors with exponential
  backoff (honours `Retry-After`); keeps retrying through Wi-Fi drops for up
  to 15 minutes. A request with no data moving for 90 s is abandoned and retried.
- HTTP/1.1 only (Drive uploads can stall over HTTP/2), at most 10 requests/s.
- Reads a Drive folder level by level, up to 8 folders at a time, and
  remembers each listing for the rest of the run, so every folder is read
  once per check. The final check after changes reads Drive again.
- Every upload is checked against Google's MD5; a bad copy is trashed and
  re-sent. A final pass re-reads the Drive folder and verifies every file.
- Never deletes: replaced or duplicate copies go to Drive's trash.
- Keeps the laptop awake during uploads (systemd-inhibit); closing the lid
  still suspends.

Files: sign-in in `~/.config/dhaara/`, resume data and logs in
`~/.local/state/dhaara/` (all readable only by you).

## Code layout

    cmd/dhaara/          the command: options, help, exit codes
    internal/engine/     upload, download, sync, pull, phone, snapshot (and their tests)
    internal/gdrive/     Google sign-in, Drive API calls, retries and pacing
    internal/adb/        file operations on an Android phone through adb
    internal/exclude/    exclusion patterns
    internal/term/       terminal output and the live progress line
    internal/platform/   OS bits: sleep inhibit, free space, trash, private files

Build from a checkout with `go build ./cmd/dhaara`.

## Tests

    go test -race ./...

Runs against an in-process fake Drive that injects errors, dropped
connections, half-stored chunks, lost replies, stalls, expired sessions and
corrupted checksums.
