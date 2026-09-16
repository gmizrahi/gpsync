package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// trashPath/uniqueTrashPath/moveToTrash are thin aliases over
// internal/engine's exported versions -- the actual implementations moved
// there so gpsync-tray's Duplicate Resolver tab can share the exact same
// destructive primitive (never falls back to deleting) instead of a
// second, private copy. Kept as local names here rather than rewriting
// every call site below to `engine.X`, purely to keep this diff small.
func trashPath(trashDir, originalPath string) string { return engine.TrashPath(trashDir, originalPath) }
func moveToTrash(trashDir, src string) (string, error) {
	return engine.MoveToTrash(trashDir, src)
}

// copySuffixRe matches the "_<digits>" tail that marks a file as a COPY of
// another rather than the original: DSC05754_1.JPG, DSC05754_2.JPG. It is
// the convention Windows-style copy/paste duplication leaves behind, and
// the same one uniqueTrashPath writes when disambiguating in the trash --
// deliberately one definition, so the two can never drift apart.
var copySuffixRe = regexp.MustCompile(`_\d+$`)

// stemOf is a path's basename without its extension.
func stemOf(path string) string {
	name := filepath.Base(path)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// isCopyOfSibling reports whether path looks like a "_N" duplicate OF
// another file in the same group.
//
// The marker alone is not enough, and treating it as enough is wrong on
// real libraries: "IMG_0042.jpg" ends in _<digits> but is an ordinary
// camera filename, not a copy of anything. The marker only means "copy"
// when stripping it yields the name of a file that is ACTUALLY present
// alongside it -- DSC05754_1.JPG next to DSC05754.JPG. That relative test
// is what makes this safe to act on:
//
//	DSC05754.JPG, _1, _2, _3  -> the three _N are copies, DSC05754 is not
//	IMG_0042.jpg, IMG_0042_1.jpg -> only IMG_0042_1 is a copy ("IMG" is
//	                                not present, so IMG_0042 is an original)
func isCopyOfSibling(path string, siblings map[string]bool) bool {
	stem := stemOf(path)
	base := copySuffixRe.ReplaceAllString(stem, "")
	return base != stem && siblings[base]
}

// unsuffixedPaths returns the group's copies that are NOT "_N" duplicates
// of another copy in the same group -- the ones that look like originals.
func unsuffixedPaths(paths []string) []string {
	siblings := make(map[string]bool, len(paths))
	for _, p := range paths {
		siblings[stemOf(p)] = true
	}
	var out []string
	for _, p := range paths {
		if !isCopyOfSibling(p, siblings) {
			out = append(out, p)
		}
	}
	return out
}

// pinKind selects how an [A] pin decides which copy to keep in later groups.
//
// Two strategies, because duplicates arrive in two structurally different
// shapes and neither rule can answer the other's case:
//
//   - pinFolder: the copies are spread across two or more folders, and the
//     choice is "which folder wins". Cannot disambiguate a group whose
//     copies all sit in ONE folder, since every candidate is in it.
//   - pinUnsuffixed: every copy is in a single folder and they differ only
//     by a "_1"/"_2" copy marker (Windows-style copy/paste duplication).
//     The choice is "keep the one that isn't a copy". Only meaningful when
//     the folder set has exactly one member -- with several folders in play
//     the marker says nothing about which folder the user wants.
//
// The two are mutually exclusive by construction (one folder vs. two or
// more), so no group can ever qualify for both.
type pinKind int

const (
	pinNone pinKind = iota
	pinFolder
	pinUnsuffixed
)

// dupPin is an [A] choice: a strategy, plus the folder set it was made on.
// It survives only while later groups share that exact set.
type dupPin struct {
	kind   pinKind
	folder string // pinFolder only: the folder whose copy is kept
	setKey string // the folder set this was established on
}

// candidate returns the single path this pin selects from a group, or ""
// when the pin cannot answer unambiguously -- in which case the caller must
// ask rather than guess. Guessing here trashes a real file on a coin flip.
func (p dupPin) candidate(paths []string) string {
	var candidates []string
	switch p.kind {
	case pinFolder:
		candidates = pathsInFolder(paths, p.folder)
	case pinUnsuffixed:
		candidates = unsuffixedPaths(paths)
	default:
		return ""
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

// label is how the [A] option describes this pin in a prompt.
func (p dupPin) label() string {
	if p.kind == pinUnsuffixed {
		return "A=keep the un-suffixed copy for all remaining files in this folder"
	}
	return fmt.Sprintf("A=keep %s for all remaining files in this folder", p.folder)
}

// pinFromChoice works out which strategy, if any, a manual answer
// establishes -- i.e. what [A] would mean if offered on the next group.
// Returns a pinNone pin when the answer implies no reusable rule.
func pinFromChoice(paths []string, keep string, setKey string) dupPin {
	folders := strings.Count(setKey, "\x00") + 1
	if folders == 1 {
		// One folder: the only thing that distinguishes copies here is the
		// _N marker, and only if the kept file is the sole unmarked one.
		if unsuffixed := unsuffixedPaths(paths); len(unsuffixed) == 1 && unsuffixed[0] == keep {
			return dupPin{kind: pinUnsuffixed, setKey: setKey}
		}
		return dupPin{}
	}
	// Several folders: the choice is which folder wins, and it only
	// generalises if that folder held exactly one copy here.
	dir := filepath.Dir(keep)
	if len(pathsInFolder(paths, dir)) == 1 {
		return dupPin{kind: pinFolder, folder: dir, setKey: setKey}
	}
	return dupPin{}
}

// dupSummary is what one `gpsync duplicates resolve` pass did.
type dupSummary struct {
	resolved    int   // groups where a copy was chosen and others removed
	autoApplied int   // of resolved, how many needed no prompt
	skipped     int   // groups deliberately left alone (0 = skip)
	passedOver  int   // groups that could not be resolved safely -- see the guards below
	trashed     int   // files actually moved to the trash (or, in dry-run, that would be)
	bytesFreed  int64 // reclaimed from the source location by those moves
}

// folderSetKey renders a group's set of containing folders as a single
// comparable string.
//
// The pin is by FOLDER, never by the numeric index the user typed: index 1
// can point at a different folder from one group to the next, since the
// path list order is not guaranteed to be stable across groups. Sorted and
// deduped, so the comparison is a true set comparison -- "these same two
// folders", regardless of which happened to be listed first.
func folderSetKey(paths []string) string {
	seen := map[string]bool{}
	var dirs []string
	for _, p := range paths {
		d := filepath.Dir(p)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	return strings.Join(dirs, "\x00")
}

// pathsInFolder returns the group's paths that live directly in dir.
func pathsInFolder(paths []string, dir string) []string {
	var out []string
	for _, p := range paths {
		if filepath.Dir(p) == dir {
			out = append(out, p)
		}
	}
	return out
}

// sortGroupsByFolder clusters groups that share the same folder set next to
// each other. db.DuplicateGroups orders by reclaimable space, which
// scatters groups sharing the same folder(s) throughout the whole list --
// the [A] auto-apply pin only ever helps two CONSECUTIVE groups, so under
// that ordering it reset almost immediately even when dozens of
// matching-folder groups existed elsewhere in the list. SliceStable keeps
// each folder set's own groups in their original largest-reclaimable-first
// order.
func sortGroupsByFolder(groups []statedb.DuplicateGroup) {
	sort.SliceStable(groups, func(i, j int) bool {
		return folderSetKey(groups[i].Paths) < folderSetKey(groups[j].Paths)
	})
}

// existingPaths is a thin alias over internal/engine's exported version --
// see the comment by trashPath/moveToTrash above for why the shared
// implementations moved there.
func existingPaths(paths []string) []string { return engine.ExistingPaths(paths) }

// resolveDuplicates runs the interactive resolution loop.
//
// Split from the cobra command (like recheckFailures) and driven through an
// explicit reader/writer so the whole decision machine -- prompting, the
// folder pin, auto-apply, skip -- is testable with scripted answers and no
// terminal. dryRun runs the identical flow and prints what it WOULD delete
// without touching the disk.
func resolveDuplicates(db *statedb.DB, groups []statedb.DuplicateGroup, trashDir string, dryRun bool, in io.Reader, out io.Writer) dupSummary {
	var sum dupSummary
	reader := bufio.NewReader(in)

	// pin is the active [A] choice (see dupPin); pending is the pin the
	// PREVIOUS manual answer would establish, which is what makes offering
	// [A] on this group meaningful. Both clear the moment a group's folder
	// set differs from the one they were made on.
	var pin, pending dupPin

	for _, g := range groups {
		paths := existingPaths(g.Paths)
		if len(paths) < 2 {
			// Nothing to resolve: either already cleaned up, or the ledger
			// is describing files that are gone.
			sum.passedOver++
			continue
		}
		setKey := folderSetKey(paths)

		// ── auto-apply, if a pin is active and still applies ──
		if pin.kind != pinNone {
			if setKey != pin.setKey {
				pin = dupPin{} // different folders: this pin says nothing here
			} else if keep := pin.candidate(paths); keep != "" {
				moved, freed := applyDuplicateChoice(db, g.SHA256, keep, paths, g.Size, trashDir, dryRun, out, true)
				sum.resolved++
				sum.autoApplied++
				sum.trashed += moved
				sum.bytesFreed += freed
				continue
			}
			// The pin cannot pick a single copy in THIS group (e.g. the
			// pinned folder holds two of them, or several copies are
			// unsuffixed). Fall through and ask; the pin stays, since the
			// folder set still matches.
		}

		// ── prompt ──
		// [A] is offered only when the previous manual answer established a
		// reusable rule AND that rule picks exactly one copy here.
		offerA := pin.kind == pinNone && pending.kind != pinNone &&
			setKey == pending.setKey && pending.candidate(paths) != ""

		choice, ok := promptDuplicateGroup(reader, out, g, paths, offerA, pending)
		if !ok {
			// Input exhausted (EOF): stop rather than spin. Everything
			// already deleted stays deleted; the rest is simply untouched.
			return sum
		}

		switch choice {
		case dupChoiceSkip:
			// Skip does not establish or clear a pin -- it says nothing
			// about which folder the user prefers.
			sum.skipped++
		case dupChoiceAuto:
			keep := pending.candidate(paths)
			pin = pending
			moved, freed := applyDuplicateChoice(db, g.SHA256, keep, paths, g.Size, trashDir, dryRun, out, false)
			sum.resolved++
			sum.trashed += moved
			sum.bytesFreed += freed
		default:
			keep := paths[choice-1]
			moved, freed := applyDuplicateChoice(db, g.SHA256, keep, paths, g.Size, trashDir, dryRun, out, false)
			sum.resolved++
			sum.trashed += moved
			sum.bytesFreed += freed
			pending = pinFromChoice(paths, keep, setKey)
		}
	}
	return sum
}

const (
	dupChoiceSkip = 0
	dupChoiceAuto = -1
)

// promptDuplicateGroup asks which copy to keep. Returns the 1-based index,
// dupChoiceSkip, or dupChoiceAuto; ok is false when input ran out.
func promptDuplicateGroup(reader *bufio.Reader, out io.Writer, g statedb.DuplicateGroup, paths []string, offerA bool, pending dupPin) (choice int, ok bool) {
	fmt.Fprintf(out, "\n%s %s — %d copies\n",
		colHeader("Duplicate:"), humanBytes(g.Size), len(paths))
	for i, p := range paths {
		fmt.Fprintf(out, "  %s %s\n", colHeader(fmt.Sprintf("[%d]", i+1)), p)
	}

	opts := make([]string, 0, len(paths)+2)
	for i := range paths {
		opts = append(opts, strconv.Itoa(i+1))
	}
	opts = append(opts, "0=skip")
	if offerA {
		opts = append(opts, pending.label())
	}

	for {
		fmt.Fprintf(out, "Keep which? [%s]: ", strings.Join(opts, "/"))
		line, err := reader.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == "" && err != nil {
			fmt.Fprintln(out)
			return 0, false
		}
		if offerA && strings.EqualFold(answer, "A") {
			return dupChoiceAuto, true
		}
		n, convErr := strconv.Atoi(answer)
		if convErr == nil && n == 0 {
			return dupChoiceSkip, true
		}
		if convErr == nil && n >= 1 && n <= len(paths) {
			return n, true
		}
		fmt.Fprintf(out, "  %s please enter %s\n", colWarn("?"), strings.Join(opts, ", "))
		if err != nil {
			return 0, false
		}
	}
}

// applyDuplicateChoice moves every copy in the group except keep into the
// trash dir.
//
// The kept file is stat'd again immediately before anything is moved. That
// guard is the difference between "trash the redundant copies" and "move
// away the only copies": if the chosen file has gone missing since the
// listing was built, relocating its siblings would leave nothing in place.
// Nothing is moved in that case.
func applyDuplicateChoice(db *statedb.DB, sha256, keep string, paths []string, size int64, trashDir string, dryRun bool, out io.Writer, auto bool) (moved int, freed int64) {
	if info, err := os.Stat(keep); err != nil || info.IsDir() {
		fmt.Fprintf(out, "  %s %s is no longer readable — leaving every copy of this file alone\n",
			colErr("skipped:"), keep)
		return 0, 0
	}

	var victims []string
	for _, p := range paths {
		if p != keep {
			victims = append(victims, p)
		}
	}
	if auto {
		fmt.Fprintf(out, "  %s keeping %s, moving %d other copy(ies) to the trash\n", colDim("auto:"), keep, len(victims))
	}

	// The ledger's first_source_path may currently name one of the victims
	// about to move -- repoint it at the survivor BEFORE moving anything,
	// so a still-pending/failed_retryable row is never left describing a
	// path this command is about to move out from under it. keep was just
	// confirmed to exist above, and this is a no-op write for a hash
	// that's already 'uploaded' (nothing re-reads the path in that case).
	if !dryRun {
		if err := db.UpdateFirstSourcePath(sha256, keep); err != nil {
			printDBError("repointing the ledger to the kept copy of "+keep, err)
		}
	}

	for _, p := range victims {
		if dryRun {
			fmt.Fprintf(out, "  %s %s\n    %s %s\n", colWarn("would trash:"), p, colDim("->"), trashPath(trashDir, p))
			moved++
			freed += size
			continue
		}
		dest, err := moveToTrash(trashDir, p)
		if err != nil {
			fmt.Fprintf(out, "  %s could not trash %s: %v\n", colErr("error:"), p, err)
			continue
		}
		fmt.Fprintf(out, "  %s %s\n    %s %s\n", colWarn("trashed:"), p, colDim("->"), dest)
		moved++
		freed += size
		// The scan-cache row now describes a path with no file at it.
		if err := db.DeleteFileSeen(p); err != nil {
			printDBError("clearing the scan-cache entry for "+p, err)
		}
	}
	return moved, freed
}

func duplicatesResolveCmd() *cobra.Command {
	var dryRun bool
	var trashDirFlag string
	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Choose which duplicate to keep and move the rest to a trash folder",
		Long: "Shows each duplicate group and asks which copy to keep. The other copies are moved into the trash folder under their own names; a name that is already taken gets a suffix, such as photo_1.jpg. Answer 0 to skip a group.\n" +
			"\n" +
			"When the following groups involve the same folders, [A] keeps that folder for all of them.\n" +
			"\n" +
			"The trash folder must be outside every source folder, or its files are scanned and uploaded again. Moving copies does not change what is synced: the ledger tracks content, not paths.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync duplicates resolve --trash-dir \"C:\\Trash\"\n" +
			"  gpsync duplicates resolve --dry-run",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			// --trash-dir persists as the new default, the same way
			// `gpsync backup --dest` does.
			if trashDirFlag != "" {
				cfg.TrashDir = trashDirFlag
				if err := config.Save(cfg); err != nil {
					return fmt.Errorf("saving the trash folder setting: %w", err)
				}
				fmt.Printf("Trash folder set to %s (remembered for next time).\n", cfg.TrashDir)
			}
			// Deliberately no fallback to deleting: if there's nowhere to
			// put the files, do nothing at all.
			if cfg.TrashDir == "" {
				return fmt.Errorf("no trash folder configured — set one once with:\n" +
					"    gpsync duplicates resolve --trash-dir \"C:\\Photos_Trash\"\n" +
					"Files you don't keep are moved there rather than deleted. " +
					"Pick a location outside any folder gpsync scans")
			}
			if err := os.MkdirAll(cfg.TrashDir, 0o755); err != nil {
				return fmt.Errorf("creating trash folder %s: %w", cfg.TrashDir, err)
			}

			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			groups, err := db.DuplicateGroups()
			if err != nil {
				return err
			}
			if len(groups) == 0 {
				fmt.Println("No duplicate content found.")
				return nil
			}
			sortGroupsByFolder(groups)

			if dryRun {
				fmt.Println(colWarn("--dry-run: you'll be asked the same questions, but nothing will be moved."))
			}
			fmt.Printf("Trash folder: %s\n", cfg.TrashDir)
			fmt.Printf("%d duplicate group(s) to review. Answer 0 to skip one, Ctrl+C to stop.\n", len(groups))
			fmt.Println(colDim("Copies you don't keep are moved to the trash folder, not deleted — you can restore them from there."))

			sum := resolveDuplicates(db, groups, cfg.TrashDir, dryRun, os.Stdin, os.Stdout)

			fmt.Println()
			verb := "Moved to trash"
			if dryRun {
				verb = "Would move to trash"
			}
			fmt.Printf("%s: %s file(s), %s freed at the original locations. %d group(s) resolved (%d automatically), %d skipped.\n",
				verb, colWarn(fmt.Sprintf("%d", sum.trashed)), colWarn(humanBytes(sum.bytesFreed)),
				sum.resolved, sum.autoApplied, sum.skipped)
			if sum.passedOver > 0 {
				fmt.Printf("%s\n", colDim(fmt.Sprintf("  %d group(s) had fewer than two copies still on disk and were left alone.", sum.passedOver)))
			}
			if sum.trashed > 0 && !dryRun {
				fmt.Printf("%s\n", colDim("  Review "+cfg.TrashDir+" and delete it yourself once you're happy."))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Ask the same questions but move nothing")
	cmd.Flags().StringVar(&trashDirFlag, "trash-dir", "", "Folder to move the other copies into (saved as the default)")
	return cmd
}
