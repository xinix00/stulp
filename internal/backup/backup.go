// Package backup creates and restores portable Stulp archives. A backup owns
// one consistent copy of the document plus every non-native installed app bundle.
package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/store"
)

const (
	FormatVersion       = 1
	manifestPath        = "backup.json"
	documentArchivePath = "stulp.json"
	maxArchiveFiles     = 100_000
	maxArchiveBytes     = 2 << 30
	maxDocumentBytes    = 64 << 20
)

type Manifest struct {
	Format    int        `json:"format"`
	CreatedAt string     `json:"createdAt"`
	Apps      []AppEntry `json:"apps"`
}

type AppEntry struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	// Path is empty for an app that announced itself over the attach protocol.
	// Such an app has no bundle in Stulp's filesystem; its manifest (including
	// UI declarations) is already part of stulp.json.
	Path string `json:"path,omitempty"`
}

type RestoreResult struct {
	Document         string `json:"document"`
	AppsRoot         string `json:"appsRoot"`
	PreviousDocument string `json:"previousDocument,omitempty"`
	PreviousAppsRoot string `json:"previousAppsRoot,omitempty"`
}

func Write(ctx context.Context, database *store.Store, output io.Writer) error {
	if database == nil {
		return errors.New("backup needs a file-backed store")
	}
	snapshot, err := database.SnapshotBytes(ctx)
	if err != nil {
		return err
	}

	apps, err := database.Apps(ctx)
	if err != nil {
		return err
	}
	backupManifest := Manifest{Format: FormatVersion, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	type archivedApp struct {
		entry AppEntry
		root  string
	}
	archived := make([]archivedApp, 0, len(apps))
	for _, app := range apps {
		entry := AppEntry{ID: app.ID, Version: app.Version}
		if app.Root == "" {
			announced, manifestErr := manifest.FromRaw(app.Manifest)
			if manifestErr != nil {
				return fmt.Errorf("validate announced app %q before backup: %w", app.ID, manifestErr)
			}
			if announced.ID != app.ID || announced.Version != app.Version {
				return fmt.Errorf("validate announced app %q before backup: manifest identity mismatch", app.ID)
			}
			backupManifest.Apps = append(backupManifest.Apps, entry)
			continue
		}
		entry.Path = fmt.Sprintf("apps/%03d", len(archived))
		if _, _, loadErr := manifest.Load(app.Root); loadErr != nil {
			return fmt.Errorf("validate app %q before backup: %w", app.ID, loadErr)
		}
		backupManifest.Apps = append(backupManifest.Apps, entry)
		archived = append(archived, archivedApp{entry: entry, root: app.Root})
	}

	archive := zip.NewWriter(output)
	manifestJSON, err := json.MarshalIndent(backupManifest, "", "  ")
	if err == nil {
		err = addBytes(archive, manifestPath, manifestJSON, 0o600)
	}
	if err == nil {
		err = addBytes(archive, documentArchivePath, snapshot, 0o600)
	}
	for _, app := range archived {
		if err != nil {
			break
		}
		err = addTree(archive, app.entry.Path, app.root)
	}
	closeErr := archive.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func WriteFile(ctx context.Context, database *store.Store, destination string) error {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeErr := Write(ctx, database, file)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	return nil
}

func Restore(ctx context.Context, archivePath, documentPath string) (RestoreResult, error) {
	if archivePath == "" || documentPath == "" || documentPath == ":memory:" {
		return RestoreResult{}, errors.New("restore needs an archive and a file-backed document path")
	}
	documentPath, err := filepath.Abs(documentPath)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(documentPath), 0o755); err != nil {
		return RestoreResult{}, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(documentPath), ".stulp-restore-")
	if err != nil {
		return RestoreResult{}, err
	}
	defer os.RemoveAll(staging)
	if err := extract(archivePath, staging); err != nil {
		return RestoreResult{}, err
	}
	backupManifest, err := readManifest(filepath.Join(staging, manifestPath))
	if err != nil {
		return RestoreResult{}, err
	}
	stagedDocument := filepath.Join(staging, documentArchivePath)
	if _, err := os.Stat(stagedDocument); errors.Is(err, os.ErrNotExist) {
		// Naming the missing entry beats letting the document parser fail on
		// whatever else the archive happens to contain.
		return RestoreResult{}, fmt.Errorf("archive has no %s", documentArchivePath)
	}
	stagedApps := filepath.Join(staging, "apps")
	if err := os.MkdirAll(stagedApps, 0o755); err != nil {
		return RestoreResult{}, err
	}
	finalApps := documentPath + ".apps"
	if err := validateAndRelocate(ctx, stagedDocument, stagedApps, finalApps, backupManifest); err != nil {
		return RestoreResult{}, err
	}

	suffix := ".pre-restore-" + time.Now().UTC().Format("20060102T150405Z")
	result := RestoreResult{Document: documentPath, AppsRoot: finalApps}
	oldDocument, documentExists, err := moveAside(documentPath, suffix)
	if err != nil {
		return RestoreResult{}, err
	}
	result.PreviousDocument = oldDocument
	oldApps, appsExist, err := moveAside(finalApps, suffix)
	if err != nil {
		if documentExists {
			_ = os.Rename(oldDocument, documentPath)
		}
		return RestoreResult{}, err
	}
	result.PreviousAppsRoot = oldApps

	rollback := func() {
		_ = os.Rename(finalApps, stagedApps)
		_ = os.Rename(documentPath, stagedDocument)
		if appsExist {
			_ = os.Rename(oldApps, finalApps)
		}
		if documentExists {
			_ = os.Rename(oldDocument, documentPath)
		}
	}
	if err := os.Rename(stagedApps, finalApps); err != nil {
		rollback()
		return RestoreResult{}, fmt.Errorf("publish restored apps: %w", err)
	}
	if err := os.Rename(stagedDocument, documentPath); err != nil {
		rollback()
		return RestoreResult{}, fmt.Errorf("publish restored document: %w", err)
	}
	return result, nil
}

func validateAndRelocate(ctx context.Context, documentPath, stagedApps, finalApps string, backupManifest Manifest) error {
	if backupManifest.Format != FormatVersion {
		return fmt.Errorf("unsupported Stulp backup format %d", backupManifest.Format)
	}
	database, err := store.Open(documentPath)
	if err != nil {
		return fmt.Errorf("open backup database: %w", err)
	}
	defer database.Close()
	apps, err := database.Apps(ctx)
	if err != nil {
		return err
	}
	stored := make(map[string]store.App)
	for _, app := range apps {
		stored[app.ID] = app
	}
	if len(stored) != len(backupManifest.Apps) {
		return fmt.Errorf("backup app manifest has %d apps but the document has %d", len(backupManifest.Apps), len(stored))
	}
	seen := make(map[string]bool)
	for _, entry := range backupManifest.Apps {
		if entry.ID == "" || seen[entry.ID] {
			return fmt.Errorf("backup contains an empty or duplicate app id %q", entry.ID)
		}
		seen[entry.ID] = true
		storedApp, ok := stored[entry.ID]
		if !ok || storedApp.Version != entry.Version {
			return fmt.Errorf("app %q does not match the document", entry.ID)
		}
		if entry.Path == "" {
			if storedApp.Root != "" {
				return fmt.Errorf("bundled app %q has no bundle in the backup", entry.ID)
			}
			announced, loadErr := manifest.FromRaw(storedApp.Manifest)
			if loadErr != nil || announced.ID != entry.ID || announced.Version != entry.Version {
				if loadErr == nil {
					loadErr = errors.New("manifest identity mismatch")
				}
				return fmt.Errorf("validate restored announced app %q: %w", entry.ID, loadErr)
			}
			continue
		}
		if storedApp.Root == "" {
			return fmt.Errorf("announced app %q unexpectedly has a bundle in the backup", entry.ID)
		}
		clean := path.Clean(entry.Path)
		if clean != entry.Path || !strings.HasPrefix(clean, "apps/") || strings.Contains(clean, "\\") {
			return fmt.Errorf("app %q has unsafe archive path %q", entry.ID, entry.Path)
		}
		relative := strings.TrimPrefix(clean, "apps/")
		root := filepath.Join(stagedApps, filepath.FromSlash(relative))
		loaded, _, loadErr := manifest.Load(root)
		if loadErr != nil {
			return fmt.Errorf("validate restored app %q: %w", entry.ID, loadErr)
		}
		if loaded.ID != entry.ID || loaded.Version != entry.Version {
			return fmt.Errorf("validate restored app %q: manifest identity mismatch", entry.ID)
		}
		if err := database.SetAppRoot(ctx, entry.ID, filepath.Join(finalApps, filepath.FromSlash(relative))); err != nil {
			return err
		}
	}
	return nil
}

func restoreReader(ctx context.Context, input io.ReaderAt, size int64, database *store.Store, apply bool) (RestoreResult, error) {
	if database == nil || input == nil || size <= 0 {
		return RestoreResult{}, errors.New("restore needs a backup and a file-backed store")
	}
	archive, err := zip.NewReader(input, size)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("open Stulp backup: %w", err)
	}
	if err := validateArchiveEntries(archive.File); err != nil {
		return RestoreResult{}, err
	}

	manifestData, err := readArchiveFile(archive.File, manifestPath, 1<<20)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("backup manifest: %w", err)
	}
	backupManifest, err := decodeManifest(manifestData)
	if err != nil {
		return RestoreResult{}, err
	}
	if backupManifest.Format != FormatVersion {
		return RestoreResult{}, fmt.Errorf("unsupported Stulp backup format %d", backupManifest.Format)
	}
	documentData, err := readArchiveFile(archive.File, documentArchivePath, maxDocumentBytes)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("archive %s: %w", documentArchivePath, err)
	}
	snapshot, err := store.ParseSnapshot(documentData)
	if err != nil {
		return RestoreResult{}, err
	}

	stored := make(map[string]store.App)
	for _, app := range snapshot.Apps() {
		stored[app.ID] = app
	}
	if len(stored) != len(backupManifest.Apps) {
		return RestoreResult{}, fmt.Errorf("backup app manifest has %d apps but the document has %d", len(backupManifest.Apps), len(stored))
	}

	appsRoot, err := database.AppsRoot()
	if err != nil {
		return RestoreResult{}, err
	}
	seen := make(map[string]bool)
	rooted := make([]AppEntry, 0)
	rootPaths := make(map[string]bool)
	for _, entry := range backupManifest.Apps {
		if entry.ID == "" || seen[entry.ID] {
			return RestoreResult{}, fmt.Errorf("backup contains an empty or duplicate app id %q", entry.ID)
		}
		seen[entry.ID] = true
		app, ok := stored[entry.ID]
		if !ok || app.Version != entry.Version {
			return RestoreResult{}, fmt.Errorf("app %q does not match the document", entry.ID)
		}
		if entry.Path == "" {
			if app.Root != "" {
				return RestoreResult{}, fmt.Errorf("bundled app %q has no bundle in the backup", entry.ID)
			}
			announced, parseErr := manifest.FromRaw(app.Manifest)
			if parseErr != nil || announced.ID != app.ID || announced.Version != app.Version {
				if parseErr == nil {
					parseErr = errors.New("manifest identity mismatch")
				}
				return RestoreResult{}, fmt.Errorf("validate restored announced app %q: %w", entry.ID, parseErr)
			}
			continue
		}
		clean := path.Clean(entry.Path)
		if clean != entry.Path || !strings.HasPrefix(clean, "apps/") || strings.Contains(clean, "\\") {
			return RestoreResult{}, fmt.Errorf("app %q has unsafe archive path %q", entry.ID, entry.Path)
		}
		if app.Root == "" {
			return RestoreResult{}, fmt.Errorf("announced app %q unexpectedly has a bundle in the backup", entry.ID)
		}
		if rootPaths[clean] {
			return RestoreResult{}, fmt.Errorf("multiple apps use archive path %q", clean)
		}
		for existing := range rootPaths {
			if strings.HasPrefix(clean+"/", existing+"/") || strings.HasPrefix(existing+"/", clean+"/") {
				return RestoreResult{}, fmt.Errorf("app archive paths %q and %q overlap", existing, clean)
			}
		}
		rootPaths[clean] = true
		rooted = append(rooted, entry)
	}
	for _, file := range archive.File {
		if file.Name == "apps" || !strings.HasPrefix(file.Name, "apps/") {
			continue
		}
		owned := false
		for root := range rootPaths {
			if file.Name == root || strings.HasPrefix(file.Name, root+"/") {
				owned = true
				break
			}
		}
		if !owned {
			return RestoreResult{}, fmt.Errorf("backup contains app file outside a declared bundle: %q", file.Name)
		}
	}

	var staging, stagedApps string
	if len(rooted) > 0 {
		staging, err = os.MkdirTemp(filepath.Dir(appsRoot), ".stulp-live-restore-")
		if err != nil {
			return RestoreResult{}, fmt.Errorf("stage restored app bundles: %w", err)
		}
		defer os.RemoveAll(staging)
		stagedApps = filepath.Join(staging, "apps")
		if err := os.MkdirAll(stagedApps, 0o755); err != nil {
			return RestoreResult{}, err
		}
		if err := extractAppEntries(archive.File, stagedApps); err != nil {
			return RestoreResult{}, err
		}
		for _, entry := range rooted {
			relative := strings.TrimPrefix(entry.Path, "apps/")
			root := filepath.Join(stagedApps, filepath.FromSlash(relative))
			loaded, _, loadErr := manifest.Load(root)
			if loadErr != nil {
				return RestoreResult{}, fmt.Errorf("validate restored app %q: %w", entry.ID, loadErr)
			}
			if loaded.ID != entry.ID || loaded.Version != entry.Version {
				return RestoreResult{}, fmt.Errorf("validate restored app %q: manifest identity mismatch", entry.ID)
			}
			if err := snapshot.SetAppRoot(entry.ID, filepath.Join(appsRoot, filepath.FromSlash(relative))); err != nil {
				return RestoreResult{}, err
			}
		}
	}
	if !apply {
		return RestoreResult{Document: database.Path(), AppsRoot: appsRoot}, nil
	}

	suffix := ".pre-restore-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	result := RestoreResult{Document: database.Path(), AppsRoot: appsRoot}
	var oldApps string
	var appsExisted bool
	if len(rooted) > 0 {
		oldApps, appsExisted, err = moveAside(appsRoot, suffix)
		if err != nil {
			return RestoreResult{}, err
		}
		result.PreviousAppsRoot = oldApps
		if err := os.Rename(stagedApps, appsRoot); err != nil {
			if appsExisted {
				_ = os.Rename(oldApps, appsRoot)
			}
			return RestoreResult{}, fmt.Errorf("publish restored apps: %w", err)
		}
	}

	previousDocument, err := database.RestoreSnapshot(ctx, snapshot)
	if err != nil {
		if len(rooted) > 0 {
			_ = os.Rename(appsRoot, stagedApps)
			if appsExisted {
				_ = os.Rename(oldApps, appsRoot)
			}
		}
		return RestoreResult{}, err
	}
	result.PreviousDocument = previousDocument
	return result, nil
}

// RestoreReader validates and applies a backup to an already open store. The
// caller must stop app runtimes first; Store.RestoreSnapshot serializes the
// document replacement against every concurrent mutation.
func RestoreReader(ctx context.Context, input io.ReaderAt, size int64, database *store.Store) (RestoreResult, error) {
	return restoreReader(ctx, input, size, database, true)
}

// ValidateBytes performs every archive, document and app-manifest check without
// changing the store. Manage uses it before pausing apps, so a malformed upload
// cannot disrupt a running house even briefly.
func ValidateBytes(ctx context.Context, data []byte, database *store.Store) error {
	_, err := restoreReader(ctx, bytes.NewReader(data), int64(len(data)), database, false)
	return err
}

// RestoreBytes is the convenient form used by the web API after it has applied
// its compressed-upload limit.
func RestoreBytes(ctx context.Context, data []byte, database *store.Store) (RestoreResult, error) {
	return restoreReader(ctx, bytes.NewReader(data), int64(len(data)), database, true)
}

func validateArchiveEntries(entries []*zip.File) error {
	if len(entries) > maxArchiveFiles {
		return errors.New("backup contains too many files")
	}
	seen := make(map[string]bool, len(entries))
	var total uint64
	for _, entry := range entries {
		if entry.UncompressedSize64 > uint64(maxArchiveBytes)-total {
			return errors.New("backup expands beyond the safety limit")
		}
		total += entry.UncompressedSize64
		clean := path.Clean(entry.Name)
		if clean != entry.Name || clean == "." || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") || strings.Contains(clean, "\\") {
			return fmt.Errorf("backup contains unsafe path %q", entry.Name)
		}
		if clean != manifestPath && clean != documentArchivePath && clean != "apps" && !strings.HasPrefix(clean, "apps/") {
			return fmt.Errorf("backup contains unknown path %q", entry.Name)
		}
		if seen[clean] {
			return fmt.Errorf("backup contains duplicate path %q", entry.Name)
		}
		seen[clean] = true
		if entry.Mode()&os.ModeSymlink != 0 || (!entry.FileInfo().IsDir() && !entry.Mode().IsRegular()) {
			return fmt.Errorf("backup contains link or special file %q", entry.Name)
		}
	}
	return nil
}

func readArchiveFile(entries []*zip.File, name string, limit int64) ([]byte, error) {
	for _, entry := range entries {
		if entry.Name != name {
			continue
		}
		if entry.FileInfo().IsDir() {
			return nil, errors.New("entry is a directory")
		}
		if entry.UncompressedSize64 > uint64(limit) {
			return nil, errors.New("entry exceeds the safety limit")
		}
		reader, err := entry.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if int64(len(data)) > limit {
			return nil, errors.New("entry exceeds the safety limit")
		}
		return data, nil
	}
	return nil, errors.New("entry is missing")
}

func decodeManifest(data []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result Manifest
	if err := decoder.Decode(&result); err != nil {
		return Manifest{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Manifest{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	return result, nil
}

func extractAppEntries(entries []*zip.File, destination string) error {
	for _, entry := range entries {
		if entry.Name == "apps" || !strings.HasPrefix(entry.Name, "apps/") {
			continue
		}
		relative := strings.TrimPrefix(entry.Name, "apps/")
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if entry.Mode()&0o111 != 0 {
			mode = 0o700
		}
		output, createErr := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if createErr == nil {
			_, createErr = io.Copy(output, reader)
		}
		if output != nil {
			if closeErr := output.Close(); createErr == nil {
				createErr = closeErr
			}
		}
		if closeErr := reader.Close(); createErr == nil {
			createErr = closeErr
		}
		if createErr != nil {
			return createErr
		}
	}
	return nil
}

func extract(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	archive, err := zip.NewReader(file, info.Size())
	if err != nil {
		return fmt.Errorf("open Stulp backup: %w", err)
	}
	if err := validateArchiveEntries(archive.File); err != nil {
		return err
	}
	for _, entry := range archive.File {
		clean := path.Clean(entry.Name)
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if entry.Mode()&0o111 != 0 {
			mode = 0o700
		}
		output, createErr := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if createErr == nil {
			_, createErr = io.Copy(output, reader)
		}
		closeOutputErr := error(nil)
		if output != nil {
			closeOutputErr = output.Close()
		}
		closeReaderErr := reader.Close()
		if createErr != nil {
			return createErr
		}
		if closeOutputErr != nil {
			return closeOutputErr
		}
		if closeReaderErr != nil {
			return closeReaderErr
		}
	}
	return nil
}

func readManifest(filename string) (Manifest, error) {
	file, err := os.Open(filename)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup manifest: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return Manifest{}, err
	}
	if len(data) > 1<<20 {
		return Manifest{}, errors.New("backup manifest exceeds the safety limit")
	}
	return decodeManifest(data)
}

func moveAside(filename, suffix string) (string, bool, error) {
	if _, err := os.Stat(filename); errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	backup := filename + suffix
	if _, err := os.Stat(backup); err == nil {
		return "", false, fmt.Errorf("recovery path %q already exists", backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if err := os.Rename(filename, backup); err != nil {
		return "", false, err
	}
	return backup, true, nil
}

func addTree(archive *zip.Writer, prefix, root string) error {
	return filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("app bundle contains symbolic link %q", filename)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("app bundle contains non-regular file %q", filename)
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		return addDiskFile(archive, path.Join(prefix, filepath.ToSlash(relative)), filename)
	})
}

func addDiskFile(archive *zip.Writer, name, filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = name
	header.Method = zip.Deflate
	writer, err := archive.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(writer, file)
	return err
}

func addBytes(archive *zip.Writer, name string, value []byte, mode os.FileMode) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(mode)
	header.Modified = time.Now()
	writer, err := archive.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = writer.Write(value)
	return err
}
