# Dhaara

*Dhaara (धारा): a stream, a current; continuous flow, like a river.*

Keep a folder in step between your computers, Google Drive and an Android
phone: name a pairing once, then `dhaara status` and `dhaara apply`.
Two-way or follow-only per laptop, phones over adb, and snapshots made on
Drive. Transfers resume after Ctrl+C or a dropped connection, and
every file is checked by MD5. Go standard library only, no third-party code.

## Install

    go install github.com/snehilshah/dhaara/cmd/dhaara@latest

This puts `dhaara` in `$(go env GOPATH)/bin` (usually `~/go/bin`); make sure
that's on your `PATH` (`dhaara --version` shows the release). Needs Go 1.25 or newer. Linux (it uses `gio trash`
and `systemd-inhibit` when available); phones also need `adb`.

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

A **pairing** names a folder here and a folder on Drive that are kept in
step, plus any phones that follow Drive. Set it up once on each device;
after that there are two commands. It works like `apt update` and
`apt upgrade`: **`status` never changes anything**, and changes happen only
when you `apply` them.

    dhaara add music ~/Music drive:Music            # this laptop: two-way
    dhaara add music phone:/sdcard/Music            # a phone follows Drive, over adb
    dhaara status                                   # check everything; changes nothing
    dhaara apply                                    # check again, show the plan, ask, then do it

On another laptop, which only takes Drive's changes:

    dhaara add music ~/Music drive:Music --follow
    dhaara status
    dhaara apply

A Drive path starts at My Drive, like `Music` or `Work/Reports`.

### The places in a pairing

| Place | Role | What changes there |
|---|---|---|
| the folder here, two-way (default) | its changes go to Drive, and Drive's changes come back | here and Drive |
| the folder here, `--follow` | only takes Drive's changes, never sends any | here |
| `phone:/sdcard/...` | follows Drive; its files come from the folder here, over USB or wireless debugging, so nothing is downloaded | the phone |

- Pairings are stored per device in `~/.config/dhaara/config.json`.
- `--exclude PATTERN` when adding sets exclusions for that pairing only.
- `--serial` picks a phone when several are connected (as `adb devices` lists them).
- `dhaara forget NAME` (or `forget NAME phone:/sdcard/...`) takes it out
  again. No files are touched, and adding the same folders later carries on
  where they were.
- A pairing remembers which Drive folder it was set up with. If the path
  ever leads to a different folder (another folder with the same name),
  `status` stops and says so.

### status and apply

- `status` checks every place of every pairing (or `status NAME`, one) and
  shows what would change. It always lists removals; `status -v` lists
  every step and how each place was checked.
- A phone that isn't connected is listed as "not checked" and skipped.
- The plan has stages, run in this order: here and Drive first, then each
  phone. The phone is planned against Drive *as the first stage will leave
  it*, so one `apply` brings everything in step.
- `apply` checks again, shows the fresh plan (marking anything new since
  the last `status`), and asks once per pairing: `Apply these changes?
  [y/N]`. Saying no keeps the plan pending.
- `apply --yes` doesn't check again or ask. It does exactly what the last
  `status` saved: each stage reads both sides again and does only the steps
  that still hold exactly (same file, same Drive copy, same MD5). Anything
  that changed since is skipped and listed, and anything new waits for the
  next `status`.
- `apply` stops before it starts if any stage would remove more than a
  quarter of a place's files (at least 20), unless you add
  `--allow-many-removals`.
- `status --exit-code` exits with 3 when changes are pending (for scripts).

### Two-way: here and Drive

Uploads new and changed files, and moves Drive's copy of anything deleted
here to Drive's trash. A rename or move here is a move on Drive, not a new
upload. It also takes changes made on Drive by another two-way laptop. A file
changed on both sides is a conflict: neither copy is touched. Everything it
takes off Drive is first written to a **removed list** (path and MD5 of
each old file) in `dhaara data/` in My Drive. That list is how followers
know a removal was meant.

### Following Drive: other laptops and phones

Follow-only laptops and phones only take changes from Drive; they never
send anything back. For each file they hold:

| The file there is | What happens |
|---|---|
| the same as Drive's | left alone |
| missing | copied (or moved, if the same contents sit at a path the removed list says was moved away) |
| different from Drive's | replaced only if that exact old version (path and MD5) is on the removed list; otherwise a conflict, left alone |
| not on Drive | removed only if that exact file is on the removed list; otherwise it only ever existed there, so it's left alone |

A follow-only laptop moves removed files to its trash. A phone deletes
them, after checking the file's MD5 on the phone.

**Where a phone gets its files, and the checksum check.** Before sending a
file, dhaara works out the MD5 (a fingerprint of the contents) of the copy on
this computer and compares it with Drive's. Only a match is sent, so the
phone gets exactly the version Drive holds. A file that's missing here or
different (edited after the last sync, or changed on Drive by another
machine) isn't sent: the check lists it as `can't copy (missing or different
in ...)` and ends with problems, and the phone's copy, if any, is left as it
is. Everything else still goes. Files the first stage of the same plan
brings in step (an upload or download here) count as matching.

Each file arrives on the phone under a temporary name (`.dhaara-push.part`),
its MD5 is checked there, and only then is it renamed into place, so a
broken transfer never looks like a real file.

### snapshot

Copies the whole Drive folder into `<folder> snapshots/<date>` next to it,
on Drive itself: nothing is downloaded or written locally. A stopped
snapshot continues on the next run, and a final pass checks every copy. It
counts against storage.

A snapshot lists the folder once, then copies file by file. Running
`status` while it runs is fine. Changing the same Drive folder isn't:
`apply`, `upload`, `download` and `snapshot` each lock the folder, and a
second one stops straight away with "another dhaara run is changing the
Drive folder …", changing nothing (its plan stays pending). Run it again once the first has finished.

### Folders

Folders emptied on Drive are left there (dhaara can't see files other apps
put in them, so it never trashes a folder); followers don't recreate
folders that hold no files.

**Exclusions:** one pattern per line in `~/.config/dhaara/exclude` (every
folder), plus `--exclude PATTERN` when adding a pairing (that pairing only)
or for a one-off command. Without a `/` a pattern matches a file or folder name
anywhere (`*.tmp`, `node_modules`); with a `/` it matches the path from the
top (`build/`, `projects/*/cache`). Excluded files are left alone
everywhere: never uploaded, downloaded, copied to the phone or removed.

**What dhaara can see:** it asks only for the `drive.file` permission, so
it sees only files it uploaded itself. Files added to a synced folder from
Drive's website, another app or another person are invisible to it: they
are never downloaded or removed.

## Other commands

    dhaara upload ~/Reports                 # upload, or continue an earlier upload
    dhaara upload ~/Reports --dry-run       # show what would be uploaded
    dhaara upload ~/Reports --verify-only   # check Drive against the local folder
    dhaara snapshot Music                   # full copy on Drive (see above)

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
renamed locally; a two-way pairing does.

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
    internal/engine/     pairings (status, apply), sync, follow, phone, upload, download, snapshot
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
