package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestArchiveVerification(t *testing.T) {
	root := repositoryRoot(t)
	read := func(name string) []byte {
		t.Helper()
		//nolint:gosec // Read a fixed file beneath the repository test root.
		contents, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return contents
	}
	companions := map[string][]byte{
		"CITATION.cff":         read("CITATION.cff"),
		"LICENSE":              read("LICENSE"),
		"THIRD_PARTY_LICENSES": read("THIRD_PARTY_LICENSES"),
	}
	directory := t.TempDir()
	tarFiles := cloneTestFiles(companions)
	tarFiles["tturl"] = nil
	zipFiles := cloneTestFiles(companions)
	zipFiles["tturl.exe"] = nil
	tarPath := filepath.Join(directory, "good.tar.gz")
	zipPath := filepath.Join(directory, "good.zip")
	writeTestTar(t, tarPath, tarFiles, nil)
	writeTestZip(t, zipPath, zipFiles)

	t.Chdir(root)
	for _, archive := range []string{tarPath, zipPath} {
		name := filepath.Base(archive)
		if err := verifyArchiveContents(directory, []string{name}); err != nil {
			t.Errorf("verify %s contents: %v", name, err)
		}
		if err := verifyArchiveCompanionFiles(directory, []string{name}); err != nil {
			t.Errorf("verify %s companions: %v", name, err)
		}
	}

	wrong := cloneTestFiles(zipFiles)
	wrong["THIRD_PARTY_LICENSES"] = append(wrong["THIRD_PARTY_LICENSES"], 'x')
	writeTestZip(t, filepath.Join(directory, "wrong.zip"), wrong)
	err := verifyArchiveCompanionFiles(directory, []string{"wrong.zip"})
	if err == nil || !strings.Contains(err.Error(), "wrong THIRD_PARTY_LICENSES") {
		t.Fatalf("wrong companion error = %v", err)
	}

	wrong = cloneTestFiles(zipFiles)
	wrong["LICENSE"] = append(wrong["LICENSE"], 'x')
	writeTestZip(t, filepath.Join(directory, "wrong-licence.zip"), wrong)
	err = verifyArchiveCompanionFiles(directory, []string{"wrong-licence.zip"})
	if err == nil || !strings.Contains(err.Error(), "wrong LICENSE") {
		t.Fatalf("wrong licence error = %v", err)
	}

	wrong = cloneTestFiles(zipFiles)
	wrong["CITATION.cff"] = append(wrong["CITATION.cff"], 'x')
	writeTestZip(t, filepath.Join(directory, "wrong-citation.zip"), wrong)
	err = verifyArchiveCompanionFiles(directory, []string{"wrong-citation.zip"})
	if err == nil || !strings.Contains(err.Error(), "wrong CITATION.cff") {
		t.Fatalf("wrong citation error = %v", err)
	}

	missing := cloneTestFiles(zipFiles)
	delete(missing, "CITATION.cff")
	writeTestZip(t, filepath.Join(directory, "missing-citation.zip"), missing)
	err = verifyArchiveContents(directory, []string{"missing-citation.zip"})
	if err == nil || !strings.Contains(err.Error(), "contents =") {
		t.Fatalf("missing citation error = %v", err)
	}

	writeTestTar(t, filepath.Join(directory, "not-executable.tar.gz"), tarFiles,
		map[string]int64{"tturl": 0o644})
	err = verifyArchiveContents(directory, []string{"not-executable.tar.gz"})
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("binary mode error = %v", err)
	}

	writeTestTar(t, filepath.Join(directory, "wrong-citation-mode.tar.gz"), tarFiles,
		map[string]int64{"CITATION.cff": 0o600})
	err = verifyArchiveContents(directory, []string{"wrong-citation-mode.tar.gz"})
	if err == nil || !strings.Contains(err.Error(), "CITATION.cff mode") {
		t.Fatalf("citation mode error = %v", err)
	}

	unexpected := cloneTestFiles(zipFiles)
	unexpected["EXTRA"] = []byte("unexpected archive entry\n")
	writeTestZip(t, filepath.Join(directory, "unexpected.zip"), unexpected)
	err = verifyArchiveContents(directory, []string{"unexpected.zip"})
	if err == nil || !strings.Contains(err.Error(), "contents =") {
		t.Fatalf("unexpected archive entry error = %v", err)
	}

	duplicatePath := filepath.Join(directory, "duplicate-citation.zip")
	writeTestZipWithExtra(
		t, duplicatePath, zipFiles, "CITATION.cff", companions["CITATION.cff"],
	)
	err = verifyArchiveContents(directory, []string{"duplicate-citation.zip"})
	if err == nil || !strings.Contains(err.Error(), "contents =") {
		t.Fatalf("duplicate citation error = %v", err)
	}
}

func cloneTestFiles(files map[string][]byte) map[string][]byte {
	clone := make(map[string][]byte, len(files))
	for name, contents := range files {
		clone[name] = bytes.Clone(contents)
	}
	return clone
}

func writeTestTar(
	t *testing.T,
	path string,
	files map[string][]byte,
	modes map[string]int64,
) {
	t.Helper()
	//nolint:gosec // The caller supplies a path beneath t.TempDir.
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, contents := range files {
		mode := int64(0o644)
		if name == "tturl" {
			mode = 0o755
		}
		if override, ok := modes[name]; ok {
			mode = override
		}
		header := &tar.Header{
			Name: name, Mode: mode, Size: int64(len(contents)),
			Typeflag: tar.TypeReg,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTestZip(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	writeTestZipWithExtra(t, path, files, "", nil)
}

func writeTestZipWithExtra(
	t *testing.T,
	path string,
	files map[string][]byte,
	extraName string,
	extraContents []byte,
) {
	t.Helper()
	//nolint:gosec // The caller supplies a path beneath t.TempDir.
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, contents := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if name == "tturl.exe" {
			header.SetMode(0o755)
		} else {
			header.SetMode(0o644)
		}
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if extraName != "" {
		header := &zip.FileHeader{Name: extraName, Method: zip.Deflate}
		header.SetMode(0o644)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(extraContents); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime did not report the test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}
