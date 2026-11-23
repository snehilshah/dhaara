# Dhaara redesign plan

Status: agreed direction, not started. Based on the advisory review of
2026-11-15, plus later decisions. Phases are built and released in order.

## Goals

1. A CLI you can remember: name a pairing once, then `status` / `apply`.
2. One-off upload and download of single files or folders.
3. Fast checks: hashes and Drive's change feed, not re-reading everything.
4. Cheap backups with real restore.
5. Edits made on any device (laptop, Drive, phone) are handled cleanly,
   reviewed, and never lost.
6. Full-quality streaming of Drive videos to mpv on the phone.
7. Room for a cloud runner later (deferred).

Unchanged rules: Go standard library only; nothing changes without review;
no music-specific logic; nothing is ever lost (the losing copy of any change
goes to history or a trash).

## Commands, old to new

| Today | After |
|---|---|
| `sync ~/M` | `add music ~/M drive:M` once; then `status` / `apply` |
| `pull M ~/M` | `add music ~/M drive:M --follow` once; then `status` / `apply` |
| `phone M` | `add music phone:/sdcard/M [--two-way]` once; part of `status` / `apply` |
| `pending` | `status -v` |
| `apply` | `apply`: re-checks, shows the fresh plan, asks once, runs all stages in order |
| `snapshot M` | `backup music` (restore point) / `backup music --full` (skipped if unchanged) |
| `dhaara DIR`, `upload DIR` | `copy DIR drive:Work/` |
| `download X DIR` | `copy drive:X DIR` |
| `--overwrite`, `--dest-name` | `--replace`; `--dest-name` is gone |
| (none) | `ls`, `link`, `share`, `history`, `restore`, `prune`, `resolve`, `forget`, `login`, `doctor`, `serve`, `playlist`, `--json`, zsh completion |

Grammar: everything not on this computer has a prefix (`drive:`, `phone:`),
source before destination. Unknown commands are errors. Results on stdout,
progress on stderr. Exit codes: 0 ok, 1 finished with problems, 2 fatal or
usage, 130 stopped; `status --exit-code` returns 3 when changes are pending.

Config: `~/.config/dhaara/config.json`, per device, written by `add` /
`forget`. Each pairing records its local folder, Drive path and pinned Drive
folder ID, its role, its phones (dir, serial, role) and excludes. Existing
state (`synced.json`, `md5.json`) is migrated so nothing is re-hashed and
first-sync rules don't return.

## Edits on every device

Every place in a pairing remembers the last version it agreed on with the
place it syncs with (its *base*: path, MD5). Comparing the base with both
sides says who changed a file:

| Here | Other side | Meaning | Plan |
|---|---|---|---|
| = base | = base | nothing changed | nothing |
| changed | = base | changed here only | send it |
| = base | changed | changed there only | take it |
| changed | changed, same MD5 | same edit on both | update the base only |
| changed | changed, differ | real conflict | show it; `resolve` decides |

Sync already works this way between the laptop and Drive. The plan extends
it to the phone, and to followers' local edits.

**Phone roles**
- `--two-way` (for example the music phone, where Geet saves lyrics): phone
  edits, additions and removals come back. The merged plan on the curating
  laptop runs: phone → here, here ↔ Drive, Drive → phone. Each step is shown
  in `status`, and a phone edit lands on Drive only after you apply it.
- `follow` (default): the phone takes changes only. A file edited on the
  phone shows up as a conflict, with its sizes, dates and the `resolve`
  command to run.

**Followers (Vostro)** keep a base too. A local edit there is listed rather
than silently overwritten, so `resolve --keep here` can upload it, as an
explicit reviewed one-off.

**`dhaara resolve NAME PATH --keep here|drive|phone`**
- A reviewed plan, shown with sizes, dates and MD5s of every copy before you
  say yes.
- `--keep phone`: copy the phone's version to the curating laptop, sync it
  to Drive, and the phone then matches.
- `--keep drive` / `--keep here`: the chosen version is written everywhere.
- Every losing copy goes to `history` on Drive (or the local trash for
  laptop-only copies), so `restore` can bring it back.
- `resolve NAME` with no path walks through all conflicts one by one.

**Detecting a phone edit:** the phone's base is stored next to the existing
phone MD5 cache, and written after every phone apply. The first run after
upgrading takes the current matching files as the base, and lists anything
different as a conflict, never as an edit to send.

Example (the Nov 23 conflict, "For A Reason.mp3": the phone's copy has
timed lyrics from Nov 9; the laptop and Drive copies are from May):
- With the phone two-way: `status` shows "phone → here: update For A
  Reason.mp3 (edited on the phone Nov 9)", then "here → Drive: update", and
  `apply` does both.
- With the phone follow: `status` shows the conflict and suggests
  `dhaara resolve music "hindi/deemix/Karan Aujla - For A Reason.mp3" --keep phone`.

## Faster checks

Requests for the 51-folder / 2,870-file music folder:

| | Now | Index (phase 1) | + changes feed (phase 5) |
|---|---|---|---|
| Check | 56 (about 27 s, measured 2026-11-23) | 4 | 1–2 |
| Laptop cycle: sync and phone, check and apply | 287 | about 12 | about 6 |

- Index: one flat `files.list` (about 3 pages) instead of one request per
  folder. The tree is rebuilt from parents, and the result cached in
  `~/.local/state/dhaara/drive/index.json`. The removed list is cached by
  its MD5.
- **Changed while building phase 1 (2026-11-23):**
  - Measured live: asking for each item's parents caps a page at 460
    items. dhaara can see 5,906 items (snapshots double the music folder),
    so a flat listing took 13 pages and 15.7 s. It also grows with every
    snapshot and history file.
  - So phase 1 is instead:
    - a parallel walk, level by level, 8 folders at a time;
    - one in-memory listing cache per run (`gdrive.Listings`), so every
      folder is read once per check; verifies read again.
  - Result: a sync check went from 27.2 s to 10.2 s, and a phone check from
    29.4 s to 10.6 s. The listing part is about 52 requests; at 10 per
    second, the pacing sets the floor.
  - The flat index waits for phase 5: the changes feed refreshes it, so
    it's built once, not on every run.
  - `files/root` returns 404 under `drive.file`. Under the flat index, the
    top folder's ID comes from the parents of `'root' in parents` items.
- Changes feed: save `startPageToken` and ask Drive "what changed since?".
  It sees edits from the Vostro and the website too. Full relist when the
  index is missing or old, the token is rejected, a reply contradicts the
  index, or with `--relist`.
- Apply keeps the full final verify; with the index it costs 1–3 requests.

## Backup

- History: sync moves replaced and removed Drive files into
  `<folder> history/<date time>/<path>` instead of the trash. A move copies
  no bytes and keeps the file ID.
- Restore point: a JSON list of path, ID, MD5 and size in `dhaara data/`
  (about 350 KB). Written after every apply that changes Drive, and by
  `backup NAME`.
- `restore NAME [PATH] --at POINT [--to DIR]`: a reviewed plan. On Drive,
  the old version is copied back and the current one moves to history.
  Normal sync then carries the result to every device. `--to` only
  downloads.
- `prune NAME --keep 90d|N`: reviewed. History files no longer referenced
  by any kept restore point go to Drive's trash (30 more days).
  `status` and `history` always show history size.
- `backup --full`: today's snapshot, skipped when nothing has changed.

## One-off files

- `copy SRC... DST`: exactly one side is `drive:`. A trailing `/` means
  "into this folder"; without it, the name is the exact path (rename). A
  second run never creates `X/X`.
- Collisions: same MD5 is skipped; different contents are replaced only
  after a prompt or `--replace`, and the old copy goes to the trash.
- When dhaara creates a top-level folder, it warns that a same-named folder
  made on Drive's website is a different one, invisible to dhaara.
- `ls` (marks dhaara's own folders), `link` (private link), `share` (anyone
  with the link, only when asked).
- Resume, progress and MD5 checks reuse the current upload code.

## Streaming to mpv on the phone

- Credential: a service account `dhaara-stream` in the existing project
  (no roles). You share chosen folders with its email as Viewer. It never
  expires and needs no consent screen. Sync keeps using your own sign-in.
- `dhaara serve`: a relay on `127.0.0.1` (run on the phone in Termux, or on
  the laptop over Wi-Fi). It adds the credential and passes requests,
  including `Range`, to Drive's `alt=media`: original quality, all tracks,
  seeking.
- `dhaara playlist drive:PATH > x.m3u`: one entry per video, in name order,
  pointing at the relay.
- Check first: the service account sees the shared folder, and a signed-in
  download avoids Drive's "Quota exceeded" page.

## Plan and apply internals

- Structured plan steps: op, path, MD5, size, file ID, expected state, and
  `runs_on: drive|device`. The step key is derived from these.
- One plan per pairing, in ordered stages. Later stages are planned against
  the *predicted* state after earlier stages. At apply, each stage is
  re-planned against reality, and only matching steps run.
- All mass-removal guards are checked before stage 1.
- Per-pairing lock (`flock`).
- Drive-only work (`backup --full`, restore, prune) is written as a job file
  of `runs_on: drive` steps, run by one function: the slot for the cloud
  runner.

## Phases

| Phase | Work | Release |
|---|---|---|
| 0 | Structured steps, Drive prediction, lock (done 2026-11-23) | |
| 1 | Parallel listing and per-run cache (done 2026-11-23; the flat index moved to 5) | v0.0.4 |
| 2 | Config, `add` / `status` / `apply` / `forget`, merged plan, state migration, base per place (phone included), conflicts shown with dates and sizes | v0.1.0 |
| 3 | `copy`, `ls`, `link`, `share` | |
| 4 | History, restore points, `restore`, `prune`, `backup --full` skip, `resolve`, phone `--two-way` | v0.2.0 |
| 5 | Flat index and changes feed | |
| 6 | Streaming: service-account sign-in, `serve`, `playlist` | |
| 7 | `doctor`, `login`, `--json`, completion, batch requests, phone case-clash warning | |
| 8 | Cloud runner: job files in `dhaara data/jobs/`, run on Google's side (deferred) | |

Streaming (6) depends on nothing above and can move earlier.

Tests per phase:
- the index builds the same tree as today's walk;
- predicted keys equal actual keys after earlier stages;
- migration runs without re-hashing;
- the base table above holds, for laptop, phone and follower;
- `resolve` round-trips, and the loser is restorable;
- path rules for `copy`;
- restore round-trips;
- prune never removes a referenced file;
- the changes feed gives the same index as a full listing.

The fake Drive needs a flat `files.list` and a changes feed.

## Decided

- Backup model: history and restore points; `--full` stays as an option.
- Phone file source: the laptop's copy (USB); no automatic fallback to
  Drive. An explicit `--from-drive` may come later.
- Streaming credential: a service account with folders shared to it.
- OAuth app published (2026-11-23), so sign-ins no longer expire after
  7 days.
- Cloud runner: preferred, deferred.

## Left out on purpose

- Watch mode: it fights review before apply.
- Keep-both on name collisions.
- Automatic phone fallback to Drive.
