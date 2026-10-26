package engine

import (
	"fmt"
	"slices"

	"github.com/snehilshah/dhaara/internal/term"
)

// Mirroring is for devices that only take changes from Drive (pull on a
// laptop, phone over adb). They never send anything back. A file there is:
//
//   - the same as Drive's: left alone
//   - missing: fetched, or moved from a local file with the same contents
//     that the removed list says was moved away from its old path
//   - different from Drive's: replaced only if the removed list has that
//     exact old version (path and MD5); otherwise a conflict, left alone
//   - not on Drive: removed only if the removed list has that path and MD5;
//     otherwise it's yours (only ever existed here) and is left alone

type mirrorLocal struct {
	Size    int64
	MtimeNs int64
}

type mirrorMove struct {
	From string
	To   remoteFile
}

type mirrorPlan struct {
	fetch     []remoteFile    // missing, or an old version to replace
	replace   map[string]bool // paths in fetch that replace an old version
	moves     []mirrorMove
	remove    []string // paths; each one's MD5 is in the md5 map given to planMirror
	conflicts []string
	same      int
}

func (p *mirrorPlan) empty() bool { return len(p.fetch)+len(p.moves)+len(p.remove) == 0 }

// mirrorNeeds lists the local files whose MD5 the plan needs: same size as
// Drive's copy (to tell same from different), or a path on the removed list.
func mirrorNeeds(remote map[string]remoteFile, local map[string]mirrorLocal, removed *removedList) []string {
	var out []string
	for rel, l := range local {
		r, onDrive := remote[rel]
		if (onDrive && r.Size == l.Size && r.Size > 0) || removed.HasPath(rel) {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}

func planMirror(remote map[string]remoteFile, local map[string]mirrorLocal, md5 map[string]string,
	removed *removedList, overwrite bool) mirrorPlan {
	p := mirrorPlan{replace: map[string]bool{}}
	rels := make([]string, 0, len(remote))
	for rel := range remote {
		rels = append(rels, rel)
	}
	slices.Sort(rels)

	var missing []remoteFile
	for _, rel := range rels {
		r := remote[rel]
		l, ok := local[rel]
		if !ok {
			missing = append(missing, r)
			continue
		}
		m := md5[rel]
		switch {
		case l.Size == r.Size && (r.Size == 0 || m == r.MD5):
			p.same++
		case (m != "" && removed.Has(rel, m)) || overwrite:
			p.fetch = append(p.fetch, r)
			p.replace[rel] = true
		default:
			p.conflicts = append(p.conflicts, rel)
		}
	}

	type key struct {
		md5  string
		size int64
	}
	removable := map[key][]string{}
	var localOnly []string
	for rel, l := range local {
		if _, onDrive := remote[rel]; onDrive {
			continue
		}
		if m := md5[rel]; m != "" && removed.Has(rel, m) {
			localOnly = append(localOnly, rel)
			k := key{m, l.Size}
			removable[k] = append(removable[k], rel)
		}
	}
	for k := range removable {
		slices.Sort(removable[k])
	}
	moved := map[string]bool{}
	for _, r := range missing {
		k := key{r.MD5, r.Size}
		if froms := removable[k]; len(froms) > 0 && r.MD5 != "" {
			removable[k] = froms[1:]
			moved[froms[0]] = true
			p.moves = append(p.moves, mirrorMove{From: froms[0], To: r})
			continue
		}
		p.fetch = append(p.fetch, r)
	}
	slices.Sort(localOnly)
	for _, rel := range localOnly {
		if !moved[rel] {
			p.remove = append(p.remove, rel)
		}
	}
	return p
}

// printMirrorPlan shows the plan; fetchVerb is "download" or "copy to the phone".
func printMirrorPlan(ui *term.Console, p mirrorPlan, dry bool, fetchVerb, where string) {
	would := func(s string) string {
		if dry {
			return "would " + s
		}
		return s
	}
	list := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		ui.Printf("%s: %d", title, len(lines))
		for i, l := range lines {
			if i == 100 {
				ui.Printf("  ... and %d more", len(lines)-100)
				break
			}
			ui.Printf("  %s", l)
		}
	}
	ui.Printf("Same as Drive: %d files.", p.same)
	var lines []string
	for _, m := range p.moves {
		lines = append(lines, fmt.Sprintf("%s %s → %s", would("move"), m.From, m.To.Rel))
	}
	list("Moved or renamed on Drive", lines)
	lines = nil
	for _, rel := range p.remove {
		lines = append(lines, fmt.Sprintf("%s %s", would("remove"), rel))
	}
	list("Removed from Drive", lines)
	lines = nil
	for _, r := range p.fetch {
		what := "new"
		if p.replace[r.Rel] {
			what = "replaces the old version " + where
		}
		lines = append(lines, fmt.Sprintf("%s %s (%s, %s)", would(fetchVerb), r.Rel, what, term.Size(r.Size)))
	}
	list("From Drive", lines)
	if len(p.conflicts) > 0 {
		list(fmt.Sprintf("Different from Drive and left alone %s (not an old version dhaara knows)", where), p.conflicts)
	}
	if p.empty() && len(p.conflicts) == 0 {
		ui.Printf("Nothing to do: %s matches Drive.", where)
	}
}
