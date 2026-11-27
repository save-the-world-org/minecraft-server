package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeapMB(t *testing.T) {
	for limit, want := range map[string]int{
		"":           1024,
		"junk":       1024,
		"3221225472": 2150, // 3 GiB
		"268435456":  256,  // 256 MiB: never below 256
	} {
		if got := heapMB(limit); got != want {
			t.Errorf("heapMB(%q) = %d, want %d", limit, got, want)
		}
	}
}

func TestFetchChecksTheChecksum(t *testing.T) {
	body := []byte("a server jar")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	dir := t.TempDir()

	good := filepath.Join(dir, "good.jar")
	if err := fetch(pinned{name: "good", url: srv.URL, sha256: hex.EncodeToString(sum[:])}, good); err != nil {
		t.Fatalf("a matching download: %v", err)
	}
	if got, _ := os.ReadFile(good); !bytes.Equal(got, body) {
		t.Fatalf("the file holds %q", got)
	}

	bad := filepath.Join(dir, "bad.jar")
	err := fetch(pinned{name: "bad", url: srv.URL, sha256: strings.Repeat("0", 64)}, bad)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a download with the wrong checksum: %v", err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("a download with the wrong checksum was kept")
	}
}

func tarGz(t *testing.T, entries ...tar.Header) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		content := []byte("x")
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(content))
		}
		h.Mode = 0o755
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(content)
		}
	}
	tw.Close()
	gz.Close()
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUntarAndFindJava(t *testing.T) {
	archive := tarGz(t,
		tar.Header{Name: "jdk/", Typeflag: tar.TypeDir},
		tar.Header{Name: "jdk/bin/java", Typeflag: tar.TypeReg},
	)
	dest := filepath.Join(t.TempDir(), "jre")
	if err := untar(archive, dest); err != nil {
		t.Fatal(err)
	}
	java, err := findJava(dest)
	if err != nil || !strings.HasSuffix(java, filepath.Join("jdk", "bin", "java")) {
		t.Fatalf("findJava = %q, %v", java, err)
	}
}

func TestUntarRefusesToLeaveItsFolder(t *testing.T) {
	for name, h := range map[string]tar.Header{
		"a file above":  {Name: "../escaped", Typeflag: tar.TypeReg},
		"a link above":  {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../etc"},
		"a rooted link": {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
	} {
		if err := untar(tarGz(t, h), filepath.Join(t.TempDir(), "out")); err == nil {
			t.Errorf("%s was unpacked", name)
		}
	}
}
