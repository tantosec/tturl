package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

func verifyDistribution(version, revision, directory string) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	if !isFullRevision(revision) {
		return fmt.Errorf("revision %q is not a full hexadecimal Git revision", revision)
	}
	wantArchives := archiveNames(version)
	if err := verifyArchiveSet(directory, wantArchives); err != nil {
		return err
	}
	if err := verifyArchiveContents(directory, wantArchives); err != nil {
		return err
	}
	if err := verifyChecksums(directory, wantArchives); err != nil {
		return err
	}
	if err := verifyArchiveCompanionFiles(directory, wantArchives); err != nil {
		return err
	}
	archive := filepath.Join(directory, nativeArchiveName(version))
	binary, cleanup, err := extractNativeBinary(archive)
	if err != nil {
		return err
	}
	defer cleanup()
	return verifyReleaseBinary(binary, version, revision)
}

type archiveEntry struct {
	name string
	mode os.FileMode
}

func verifyArchiveContents(directory string, archives []string) error {
	for _, name := range archives {
		archive := filepath.Join(directory, name)
		entries, err := listArchiveEntries(archive)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		binary := "tturl"
		if strings.HasSuffix(name, ".zip") {
			binary = "tturl.exe"
		}
		want := []string{"CITATION.cff", "LICENSE", "THIRD_PARTY_LICENSES", binary}
		got := make([]string, len(entries))
		for i, entry := range entries {
			got[i] = entry.name
		}
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			return fmt.Errorf("%s contents = %v, want %v", name, got, want)
		}
		for _, entry := range entries {
			wantMode := os.FileMode(0o644)
			if entry.name == binary {
				wantMode = 0o755
			}
			if entry.mode.Perm() != wantMode {
				return fmt.Errorf(
					"%s entry %s mode %v, want %v",
					name, entry.name, entry.mode.Perm(), wantMode,
				)
			}
		}
	}
	return nil
}

func listArchiveEntries(archive string) ([]archiveEntry, error) {
	if strings.HasSuffix(archive, ".zip") {
		reader, err := zip.OpenReader(archive)
		if err != nil {
			return nil, fmt.Errorf("open zip: %w", err)
		}
		defer func() { _ = reader.Close() }()
		entries := make([]archiveEntry, 0, len(reader.File))
		for _, file := range reader.File {
			if !file.Mode().IsRegular() {
				return nil, fmt.Errorf("unexpected non-file entry %s", file.Name)
			}
			entries = append(entries, archiveEntry{
				name: file.Name,
				mode: file.Mode(),
			})
		}
		return entries, nil
	}
	//nolint:gosec // archive is a release artefact under the supplied dist.
	file, err := os.Open(archive)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	var entries []archiveEntry
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		// A zero type flag is the legacy regular-file marker in the tar
		// format. archive/tar deprecates its TypeRegA name, not the format.
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			return nil, fmt.Errorf("unexpected non-file entry %s", header.Name)
		}
		entries = append(entries, archiveEntry{
			name: header.Name,
			mode: header.FileInfo().Mode(),
		})
	}
}

func verifyArchiveCompanionFiles(directory string, archives []string) error {
	files := []struct {
		source  string
		archive string
	}{
		{"CITATION.cff", "CITATION.cff"},
		{"LICENSE", "LICENSE"},
		{"THIRD_PARTY_LICENSES", "THIRD_PARTY_LICENSES"},
	}
	for _, file := range files {
		want, err := os.ReadFile(file.source)
		if err != nil {
			return fmt.Errorf("read archive companion %s: %w", file.source, err)
		}
		for _, name := range archives {
			archive := filepath.Join(directory, name)
			got, err := readArchiveFile(
				archive, file.archive, int64(len(want)+1),
			)
			if err != nil {
				return fmt.Errorf("read %s from %s: %w", file.archive, name, err)
			}
			if !bytes.Equal(got, want) {
				return fmt.Errorf("%s contains the wrong %s", name, file.archive)
			}
		}
	}
	return nil
}

func readArchiveFile(archive, name string, limit int64) ([]byte, error) {
	if strings.HasSuffix(archive, ".zip") {
		return readZipFile(archive, name, limit)
	}
	return readTarFile(archive, name, limit)
}

func readZipFile(archive, name string, limit int64) ([]byte, error) {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = reader.Close() }()
	for _, file := range reader.File {
		if file.Name != name || file.FileInfo().IsDir() {
			continue
		}
		contents, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s in zip: %w", name, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(contents, limit))
		closeErr := contents.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s in zip: %w", name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s in zip: %w", name, closeErr)
		}
		return data, nil
	}
	return nil, fmt.Errorf("zip has no %s", name)
}

func readTarFile(archive, name string, limit int64) ([]byte, error) {
	//nolint:gosec // archive is the release artefact under the supplied dist.
	file, err := os.Open(archive)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if header.Name == name && header.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tarReader, limit))
		}
	}
	return nil, fmt.Errorf("archive has no %s", name)
}

func isFullRevision(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	_, err := hex.DecodeString(revision)
	return err == nil
}

func archiveNames(version string) []string {
	var names []string
	for _, goos := range []string{"darwin", "linux", "windows"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			extension := ".tar.gz"
			if goos == "windows" {
				extension = ".zip"
			}
			names = append(names, fmt.Sprintf(
				"tturl_%s_%s_%s%s", version, goos, goarch, extension,
			))
		}
	}
	sort.Strings(names)
	return names
}

func nativeArchiveName(version string) string {
	extension := ".tar.gz"
	if runtime.GOOS == "windows" {
		extension = ".zip"
	}
	return fmt.Sprintf(
		"tturl_%s_%s_%s%s", version, runtime.GOOS, runtime.GOARCH, extension,
	)
}

func verifyArchiveSet(directory string, want []string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read distribution directory: %w", err)
	}
	var got []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "tturl_") &&
			(strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip")) {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		return fmt.Errorf("release archives = %v, want %v", got, want)
	}
	return nil
}

func verifyChecksums(directory string, archives []string) error {
	path := filepath.Join(directory, "checksums.txt")
	//nolint:gosec // The path is the fixed checksum name under the supplied dist.
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open checksums: %w", err)
	}
	defer func() { _ = file.Close() }()

	want := make(map[string]bool, len(archives))
	for _, archive := range archives {
		want[archive] = true
	}
	seen := make(map[string]bool, len(archives))
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || !want[fields[1]] || seen[fields[1]] {
			return fmt.Errorf("unexpected checksum line %q", scanner.Text())
		}
		// fields[1] was matched against the exact generated archive allowlist.
		//nolint:gosec // Only a canonical expected archive name can reach this read.
		contents, err := os.ReadFile(filepath.Join(directory, fields[1]))
		if err != nil {
			return fmt.Errorf("read archive %s: %w", fields[1], err)
		}
		digest := sha256.Sum256(contents)
		if !strings.EqualFold(fields[0], hex.EncodeToString(digest[:])) {
			return fmt.Errorf("checksum mismatch for %s", fields[1])
		}
		seen[fields[1]] = true
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}
	if len(seen) != len(want) {
		return fmt.Errorf("checksums cover %d archives, want %d", len(seen), len(want))
	}
	return nil
}

func extractNativeBinary(archive string) (string, func(), error) {
	directory, err := os.MkdirTemp("", "tturl-release-binary-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create binary directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	binaryName := "tturl"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(directory, binaryName)
	if strings.HasSuffix(archive, ".zip") {
		err = extractZipFile(archive, binaryName, binary)
	} else {
		err = extractTarFile(archive, binaryName, binary)
	}
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return binary, cleanup, nil
}

func extractZipFile(archive, name, destination string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open native zip: %w", err)
	}
	defer func() { _ = reader.Close() }()
	for _, file := range reader.File {
		if filepath.Base(file.Name) != name {
			continue
		}
		contents, err := file.Open()
		if err != nil {
			return fmt.Errorf("open binary in zip: %w", err)
		}
		err = writeExecutable(destination, contents)
		_ = contents.Close()
		return err
	}
	return fmt.Errorf("native zip has no %s", name)
}

func extractTarFile(archive, name, destination string) error {
	//nolint:gosec // archive is the canonical native name under the supplied dist.
	file, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open native archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open native gzip stream: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read native archive: %w", err)
		}
		if filepath.Base(header.Name) == name && header.Typeflag == tar.TypeReg {
			return writeExecutable(destination, tarReader)
		}
	}
	return fmt.Errorf("native archive has no %s", name)
}

func writeExecutable(path string, contents io.Reader) error {
	// The path is a fixed basename in a fresh private temporary directory. It
	// must be executable so this verifier can exercise the packaged program.
	//nolint:gosec // Verify execution from the constrained temporary path.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fmt.Errorf("create extracted binary: %w", err)
	}
	if _, err := io.Copy(file, contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("extract binary: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close extracted binary: %w", err)
	}
	return nil
}

func verifyReleaseBinary(binary, version, revision string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// binary is extracted by this process from the exact native release archive.
	//nolint:gosec // The verifier deliberately executes the artefact under test.
	output, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return fmt.Errorf("run release binary --version: %w", err)
	}
	wantVersion := fmt.Sprintf("tturl v%s (%.12s)\n", version, revision)
	if string(output) != wantVersion {
		return fmt.Errorf("version output = %q, want %q", output, wantVersion)
	}

	var stdout bytes.Buffer
	//nolint:gosec // The verifier deliberately executes the artefact under test.
	command := exec.CommandContext(
		ctx,
		binary, "race", "--report", "json", "--trials", "1",
		"https://127.0.0.1:0/",
	)
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if err := command.Run(); err == nil {
		return errors.New("release binary's port-zero request unexpectedly succeeded")
	}
	return verifyReleaseReport(stdout.Bytes(), version, revision)
}

func verifyReleaseReport(report []byte, version, revision string) error {
	decoder := json.NewDecoder(bytes.NewReader(report))
	foundRun := false
	foundRequest := false
	for {
		var record struct {
			Kind string `json:"kind"`
			Tool struct {
				Version  string `json:"version"`
				Revision string `json:"revision"`
				Modified *bool  `json:"modified"`
			} `json:"tool"`
			Headers map[string][]string `json:"headers"`
		}
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode release report: %w", err)
		}
		switch record.Kind {
		case "run":
			foundRun = record.Tool.Version == "v"+version &&
				record.Tool.Revision == revision &&
				record.Tool.Modified != nil && !*record.Tool.Modified
		case "request":
			foundRequest = slices.Equal(
				record.Headers["user-agent"], []string{"tturl/v" + version},
			)
		}
	}
	if !foundRun {
		return errors.New("release report has no matching clean release identity")
	}
	if !foundRequest {
		return errors.New("release report has no matching low-cardinality User-Agent")
	}
	return nil
}
