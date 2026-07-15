package main

import (
	"archive/zip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	libraryPath := flag.String("library", "", "path to the dynamic library")
	archivePath := flag.String("archive", "", "output ZIP archive path")
	checksumPath := flag.String("checksum", "", "output SHA-256 checksum path")
	flag.Parse()
	if *libraryPath == "" || *archivePath == "" || *checksumPath == "" {
		fail("-library, -archive, and -checksum are required")
	}

	library, err := os.Open(*libraryPath)
	if err != nil {
		fail("open library: %v", err)
	}
	defer library.Close()
	info, err := library.Stat()
	if err != nil {
		fail("stat library: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(*archivePath), 0o755); err != nil {
		fail("create archive directory: %v", err)
	}
	archive, err := os.Create(*archivePath)
	if err != nil {
		fail("create archive: %v", err)
	}
	zipWriter := zip.NewWriter(archive)
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		fail("create ZIP header: %v", err)
	}
	header.Name = filepath.Base(*libraryPath)
	header.Method = zip.Deflate
	entry, err := zipWriter.CreateHeader(header)
	if err != nil {
		fail("create ZIP entry: %v", err)
	}
	if _, err := io.Copy(entry, library); err != nil {
		fail("write ZIP entry: %v", err)
	}
	if err := zipWriter.Close(); err != nil {
		fail("close ZIP writer: %v", err)
	}
	if err := archive.Close(); err != nil {
		fail("close archive: %v", err)
	}

	contents, err := os.ReadFile(*archivePath)
	if err != nil {
		fail("read archive for checksum: %v", err)
	}
	sum := sha256.Sum256(contents)
	if err := os.WriteFile(*checksumPath, []byte(fmt.Sprintf("%x  %s\n", sum, filepath.Base(*archivePath))), 0o644); err != nil {
		fail("write checksum: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
