package backup

import (
	"archive/zip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// openTestDB points the package-level statedb/config paths at an isolated
// temp dir (mirroring internal/statedb's own test helper) and additionally
// resets config.ConfigPath, which -- unlike statedb.StateDir/StateDBPath --
// no other test in the repo needs to touch, since it's computed once from
// statedb.StateDir at package-init time and won't follow a later
// reassignment of that var on its own.
func openTestDB(t *testing.T) *statedb.DB {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", dir)
	statedb.StateDir = dir
	statedb.StateDBPath = filepath.Join(dir, "state.sqlite")
	config.ConfigPath = filepath.Join(dir, "config.toml")

	db, err := statedb.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func zipEntryNames(t *testing.T, path string) []string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names
}

// TestCreate_ProducesArchiveWithDBAndConfig_AndPrunesOldBackups proves the
// two things `gpsync backup` promises: the archive actually contains both
// state.sqlite and config.toml (readable via VACUUM INTO + zip, not just a
// non-error return), and that repeated backups beyond keepCount get pruned
// oldest-first rather than accumulating forever.
func TestCreate_ProducesArchiveWithDBAndConfig_AndPrunesOldBackups(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsurePending("hash-a", 100, "image/jpeg", "/a/one.jpg", nil); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(config.Defaults()); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()

	first, err := Create(db, destDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	names := zipEntryNames(t, first.Path)
	if len(names) != 2 {
		t.Fatalf("archive entries = %v, want state.sqlite and config.toml", names)
	}
	if len(first.Files) != 2 {
		t.Errorf("Result.Files = %v, want 2 entries matching the archive", first.Files)
	}

	// Two more backups, still under a keepCount of 2 -- older ones must be pruned.
	if _, err := Create(db, destDir, 2); err != nil {
		t.Fatal(err)
	}
	third, err := Create(db, destDir, 2)
	if err != nil {
		t.Fatal(err)
	}

	kept, err := List(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 {
		t.Fatalf("kept backups = %d, want 2 (keepCount=2 should have pruned the oldest)", len(kept))
	}
	if kept[len(kept)-1] != third.Path {
		t.Errorf("newest kept backup = %s, want %s", kept[len(kept)-1], third.Path)
	}
	for _, k := range kept {
		if k == first.Path {
			t.Errorf("the first (oldest) backup %s should have been pruned, but is still present: %v", first.Path, kept)
		}
	}
}

// TestCreate_IncludesOAuthCredentials proves the fix for an incomplete
// backup: everything actually present in ~/.gpsync must be included, not just
// a hardcoded state.sqlite/config.toml pair -- client_secret.json and
// token.json (written by `gpsync setup`/`gpsync import-rclone`) are exactly as
// much a part of "back up ~/.gpsync" as the ledger and settings are, even
// though they're security-sensitive enough that gpsync calls them out
// explicitly when it backs them up (see cmd/gpsync's hasCredentials note).
func TestCreate_IncludesOAuthCredentials(t *testing.T) {
	db := openTestDB(t)
	if err := os.WriteFile(filepath.Join(statedb.StateDir, "client_secret.json"), []byte(`{"installed":{"client_id":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statedb.StateDir, "token.json"), []byte(`{"refresh_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	result, err := Create(db, destDir, 0)
	if err != nil {
		t.Fatal(err)
	}

	names := zipEntryNames(t, result.Path)
	for _, want := range []string{"state.sqlite", "client_secret.json", "token.json"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("archive entries = %v, missing %q", names, want)
		}
	}
}

// TestRestore_OverwritesCurrentState_AndSavesPreRestoreSafetyCopy proves
// the full round trip: back up an initial state, mutate the live DB and
// config to simulate work done since, restore from the earlier backup, and
// confirm (a) the ledger and config really do revert to the backed-up
// content, not just "restore returned no error", and (b) a pre-restore
// safety copy of the state that was about to be overwritten was saved
// first, containing the MUTATED (pre-restore) content, not the original.
func TestRestore_OverwritesCurrentState_AndSavesPreRestoreSafetyCopy(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsurePending("hash-original", 100, "image/jpeg", "/original.jpg", nil); err != nil {
		t.Fatal(err)
	}
	originalCfg := config.Defaults()
	originalCfg.Concurrency = 3
	if err := config.Save(originalCfg); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	backupResult, err := Create(db, destDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := backupResult.Path

	// Simulate further work after the backup: a new pending file, and a
	// changed setting.
	if err := db.EnsurePending("hash-mutated", 200, "image/jpeg", "/mutated.jpg", nil); err != nil {
		t.Fatal(err)
	}
	mutatedCfg := originalCfg
	mutatedCfg.Concurrency = 99
	if err := config.Save(mutatedCfg); err != nil {
		t.Fatal(err)
	}
	// Restore overwrites state.sqlite on disk directly -- must not fight an
	// open connection to the same file (see Restore's doc comment).
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	restoreResult, err := Restore(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	preRestoreDir := restoreResult.PreRestoreDir

	// The pre-restore safety copy must hold the MUTATED config (what was
	// about to be overwritten), not the original.
	preCfgBytes, err := os.ReadFile(filepath.Join(preRestoreDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(preCfgBytes), "99") {
		t.Errorf("pre-restore safety copy should contain the mutated concurrency=99, got: %s", preCfgBytes)
	}

	// After Restore, the live config.toml must be back to the ORIGINAL.
	restored, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if restored.Concurrency != 3 {
		t.Errorf("restored config Concurrency = %d, want 3 (the value at backup time)", restored.Concurrency)
	}

	// And the ledger itself must be back to the original content -- the
	// mutated-state file must be gone, reopening the (now-restored) DB.
	restoredDB, err := statedb.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDB.Close()
	if up, err := restoredDB.GetUpload("hash-original"); err != nil || up == nil {
		t.Errorf("expected hash-original to be present after restore: up=%+v err=%v", up, err)
	}
	if up, err := restoredDB.GetUpload("hash-mutated"); err != nil || up != nil {
		t.Errorf("expected hash-mutated to be GONE after restore (it postdates the backup): up=%+v err=%v", up, err)
	}
}

// TestCreate_FailedWriteLeavesNoArchiveForPruneToMistakeForAGoodOne proves
// the fix for a failure mode that destroys real backups: writeZip failing
// partway (disk full mid-snapshot of a big library) used to leave the
// truncated file sitting at the FINAL destPath. That file still matched
// List/prune's "gpsync-backup-*.zip" glob and, being the newest, made prune
// delete the OLDEST genuine backups to stay under keepCount -- repeat it a
// few times against a flaky drive and every real backup is gone, replaced
// by corrupt ones. The archive is now staged as ".partial" and only renamed
// into place once complete, so a failure leaves nothing List can see.
//
// The failure is induced by making StateDir unreadable, which is exactly
// where writeZip's os.ReadDir step fails -- after the destination file has
// already been created.
func TestCreate_FailedWriteLeavesNoArchiveForPruneToMistakeForAGoodOne(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsurePending("hash-a", 100, "image/jpeg", "/a/one.jpg", nil); err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()

	good, err := Create(db, destDir, 2)
	if err != nil {
		t.Fatal(err)
	}

	// Break the source directory so the next Create fails partway.
	stateDir := statedb.StateDir
	missing := stateDir + "-moved-away"
	if err := os.Rename(stateDir, missing); err != nil {
		t.Fatal(err)
	}

	if _, err := Create(db, destDir, 2); err == nil {
		t.Fatal("expected Create to fail once its source directory is gone")
	}

	if err := os.Rename(missing, stateDir); err != nil {
		t.Fatal(err)
	}

	all, err := List(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0] != good.Path {
		t.Fatalf("List after a failed backup = %v, want only the one good archive %s -- a partial archive must never look like a real backup", all, good.Path)
	}

	// And prune must not be able to see it either: another good backup with
	// keepCount=1 should evict the earlier GOOD one, never a phantom.
	if _, err := Create(db, destDir, 1); err != nil {
		t.Fatal(err)
	}
	kept, err := List(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Fatalf("kept = %v, want exactly 1 archive", kept)
	}
	if _, err := zip.OpenReader(kept[0]); err != nil {
		t.Errorf("the surviving backup %s is not a readable zip: %v", kept[0], err)
	}

	// No ".partial" litter left behind either.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") {
			t.Errorf("leftover partial file in the backup destination: %s", e.Name())
		}
	}
}

// TestRestore_InvalidArchiveLeavesLiveStateCompletelyUntouched proves the
// fix for a half-restore reported as "nothing was restored": the sawDB
// check used to run AFTER the extraction loop had already overwritten files
// directly in StateDir with O_TRUNC, so an archive without state.sqlite
// could clobber config.toml and the OAuth credentials and THEN return an
// error claiming nothing had changed. Validation now happens before a
// single byte is written.
func TestRestore_InvalidArchiveLeavesLiveStateCompletelyUntouched(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsurePending("hash-live", 100, "image/jpeg", "/live.jpg", nil); err != nil {
		t.Fatal(err)
	}
	liveCfg := config.Defaults()
	liveCfg.Concurrency = 7
	if err := config.Save(liveCfg); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(statedb.StateDir, "token.json")
	if err := os.WriteFile(credPath, []byte(`{"refresh_token":"live-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	before := snapshotDirContents(t, statedb.StateDir)

	// An archive that carries a config.toml (so it WOULD overwrite live
	// state) but no state.sqlite.
	destDir := t.TempDir()
	badArchive := filepath.Join(destDir, "gpsync-backup-19700101-000000-000000000.zip")
	writeZipWith(t, badArchive, map[string]string{
		"config.toml": "concurrency = 999\n",
		"token.json":  `{"refresh_token":"archive-secret"}`,
	})

	if _, err := Restore(badArchive); err == nil {
		t.Fatal("expected Restore to reject an archive with no state.sqlite")
	}

	after := snapshotDirContents(t, statedb.StateDir)
	for name, want := range before {
		got, ok := after[name]
		if !ok {
			t.Errorf("%s disappeared during a failed restore", name)
			continue
		}
		if got != want {
			t.Errorf("%s was modified by a failed restore -- the live state must be untouched when Restore reports an error.\n got: %q\nwant: %q", name, got, want)
		}
	}
}

// TestRestore_WritesCredentialsAt0600 proves restored files keep the
// restrictive mode `gpsync setup`/`gpsync import-rclone` originally wrote them
// with. extractFile used 0o644, so every restore silently loosened
// client_secret.json/token.json -- and copyFile's os.Create did the same to
// the pre-restore safety copy, which additionally lived in a 0755 directory
// right next to the (often cloud-synced) archive.
func TestRestore_WritesCredentialsAt0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	db := openTestDB(t)
	if err := db.EnsurePending("hash-a", 1, "image/jpeg", "/a.jpg", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statedb.StateDir, "token.json"), []byte(`{"refresh_token":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(config.Defaults()); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	result, err := Create(db, destDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Loosen the live copy first, so the assertion below can only pass if
	// the restore actively sets the mode rather than inheriting it.
	if err := os.Chmod(filepath.Join(statedb.StateDir, "token.json"), 0o644); err != nil {
		t.Fatal(err)
	}

	restoreResult, err := Restore(result.Path)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range restoreResult.Files {
		info, err := os.Stat(filepath.Join(statedb.StateDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("restored %s mode = %o, want 600", name, perm)
		}
		preInfo, err := os.Stat(filepath.Join(restoreResult.PreRestoreDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := preInfo.Mode().Perm(); perm != 0o600 {
			t.Errorf("pre-restore copy of %s mode = %o, want 600", name, perm)
		}
	}

	dirInfo, err := os.Stat(restoreResult.PreRestoreDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("pre-restore directory mode = %o, want 700 -- it holds a raw copy of the OAuth credentials, next to a often cloud-synced archive", perm)
	}

	// No staging litter left in StateDir.
	entries, err := os.ReadDir(statedb.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover staging file after a successful restore: %s", e.Name())
		}
	}
}

// snapshotDirContents records every file's exact bytes, for proving a
// failed operation changed nothing at all (not merely that it errored).
func snapshotDirContents(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func writeZipWith(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// A backup archive is attacker-controlled input: a zip crafted elsewhere can
// carry entry names like "../../.ssh/authorized_keys", and a naive
// filepath.Join would write outside ~/.gpsync ("zip slip"). Restore must
// refuse the archive instead, leaving the state directory untouched.
func TestRestore_RefusesArchiveEntriesThatEscapeTheStateDir(t *testing.T) {
	for _, name := range []string{
		"../escaped.txt",
		"../../escaped.txt",
		"sub/nested.txt",
		`..\escaped.txt`,
		"/etc/passwd",
	} {
		if _, err := safeStatePath(name); err == nil {
			t.Errorf("safeStatePath(%q) allowed the entry; it must be refused", name)
		}
	}
	// A plain file name, which is all gpsync ever archives, still works.
	got, err := safeStatePath("state.sqlite")
	if err != nil {
		t.Fatalf("safeStatePath(\"state.sqlite\") = %v, want it accepted", err)
	}
	if want := filepath.Join(statedb.StateDir, "state.sqlite"); got != want {
		t.Errorf("safeStatePath = %q, want %q", got, want)
	}
}
