package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// A download pinned by its SHA-256: nothing runs that does not match.
type pinned struct {
	name   string
	url    string
	sha256 string
}

// Paper, the Minecraft Java server this runs. To move to a newer build, change
// all three lines from https://fill.papermc.io/v3/projects/paper/versions/<version>/builds/latest.
var paperJar = pinned{
	name:   "paper-26.3-263.jar",
	url:    "https://fill-data.papermc.io/v1/objects/949a1b05063d5bfd9ca324509d1d4726f2051e5bc81bc5f6f5d96f56f3a6e392/paper-26.3-263.jar",
	sha256: "949a1b05063d5bfd9ca324509d1d4726f2051e5bc81bc5f6f5d96f56f3a6e392",
}

// Eclipse Temurin's Java 25 runtime, which Paper 26.3 needs, for each platform
// this runs on. From https://api.adoptium.net/v3/assets/latest/25/hotspot.
var javaRuntimes = map[string]pinned{
	"linux/amd64": {
		name:   "temurin-jre-25.0.5.1+2",
		url:    "https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.5.1%2B2/OpenJDK25U-jre_x64_linux_hotspot_25.0.5.1_2.tar.gz",
		sha256: "2edef9e4f81ec5ee426d023336d4cfbd7b4644e99884e7a73a6a867b70b17acf",
	},
	"linux/arm64": {
		name:   "temurin-jre-25.0.5.1+2",
		url:    "https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.5.1%2B2/OpenJDK25U-jre_aarch64_linux_hotspot_25.0.5.1_2.tar.gz",
		sha256: "c6161dcfb365024fbfbcda265343d562d6f821c78dd8e7aeaeaf294893f6ff8b",
	},
	"darwin/arm64": {
		name:   "temurin-jre-25.0.5.1+2",
		url:    "https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.5.1%2B2/OpenJDK25U-jre_aarch64_mac_hotspot_25.0.5.1_2.tar.gz",
		sha256: "70adbb3ce5cf924c8b60dd84272fdd5fba6d021014aa0c1f5cf66a198bd270c5",
	},
}

var downloader = &http.Client{Timeout: 15 * time.Minute}

// fetch downloads p to dest unless a file with the right checksum is there
// already. It writes beside dest and renames, so a stopped download is never
// taken for a whole one.
func fetch(p pinned, dest string) error {
	if sum, err := fileSHA256(dest); err == nil && sum == p.sha256 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	resp, err := downloader.Get(p.url)
	if err != nil {
		return fmt.Errorf("download %s: %w", p.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", p.name, resp.Status)
	}
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return fmt.Errorf("download %s: %w", p.name, err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != p.sha256 {
		os.Remove(part)
		return fmt.Errorf("download %s: checksum %s, want %s", p.name, sum, p.sha256)
	}
	return os.Rename(part, dest)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ensureJava returns the path of a java binary under dir, downloading and
// unpacking this platform's pinned runtime the first time.
func ensureJava(dir string) (string, error) {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	p, ok := javaRuntimes[platform]
	if !ok {
		return "", fmt.Errorf("no Java runtime is pinned for %s", platform)
	}
	home := filepath.Join(dir, "jre-"+p.sha256[:12])
	if java, err := findJava(home); err == nil {
		return java, nil
	}
	archive := filepath.Join(dir, "jre-"+p.sha256[:12]+".tar.gz")
	if err := fetch(p, archive); err != nil {
		return "", err
	}
	tmp := home + ".unpacking"
	if err := os.RemoveAll(tmp); err != nil {
		return "", err
	}
	if err := untar(archive, tmp); err != nil {
		return "", fmt.Errorf("unpack %s: %w", p.name, err)
	}
	if err := os.Rename(tmp, home); err != nil {
		return "", err
	}
	os.Remove(archive)
	return findJava(home)
}

// findJava looks for bin/java under home: at the top on Linux, under
// Contents/Home on a Mac.
func findJava(home string) (string, error) {
	var found string
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "java" && filepath.Base(filepath.Dir(path)) == "bin" {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New("no bin/java under " + home)
	}
	return found, nil
}

// untar unpacks a .tar.gz into dest and refuses an entry that would land
// outside it.
func untar(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	root := filepath.Clean(dest) + string(os.PathSeparator)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, hdr.Name)
		if !strings.HasPrefix(target+string(os.PathSeparator), root) {
			return fmt.Errorf("entry %q leaves the archive's folder", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode().Perm()|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) || !strings.HasPrefix(filepath.Join(filepath.Dir(target), hdr.Linkname)+string(os.PathSeparator), root) {
				return fmt.Errorf("link %q leaves the archive's folder", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}
