package chrome

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func writeZip(t *testing.T, entries func(zw *zip.Writer)) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	entries(zw)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func addFile(t *testing.T, zw *zip.Writer, name string, mode os.FileMode, body string) {
	t.Helper()
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(mode)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(body))
}

func TestExtract(t *testing.T) {
	const pre = "chrome-headless-shell-linux64/"
	ok := writeZip(t, func(zw *zip.Writer) {
		addFile(t, zw, pre, os.ModeDir|0o755, "")
		addFile(t, zw, pre+"chrome-headless-shell", 0o755, "bin")
		addFile(t, zw, pre+"locales/en.pak", 0o644, "pak")
	})
	dst := filepath.Join(t.TempDir(), "out")
	if err := extract(ok, dst, pre); err != nil {
		t.Fatal(err)
	}
	if !installed(filepath.Join(dst, "chrome-headless-shell")) {
		t.Error("executable missing or not executable")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "locales", "en.pak")); string(b) != "pak" {
		t.Errorf("nested file = %q", b)
	}

	bad := map[string]func(zw *zip.Writer){
		"traversal":    func(zw *zip.Writer) { addFile(t, zw, pre+"../evil", 0o644, "x") },
		"deep escape":  func(zw *zip.Writer) { addFile(t, zw, pre+"a/../../evil", 0o644, "x") },
		"absolute":     func(zw *zip.Writer) { addFile(t, zw, pre+"/etc/evil", 0o644, "x") },
		"backslash":    func(zw *zip.Writer) { addFile(t, zw, pre+`..\evil`, 0o644, "x") },
		"wrong prefix": func(zw *zip.Writer) { addFile(t, zw, "other/evil", 0o644, "x") },
		"symlink":      func(zw *zip.Writer) { addFile(t, zw, pre+"link", os.ModeSymlink|0o777, "/etc") },
		"duplicate": func(zw *zip.Writer) {
			addFile(t, zw, pre+"f", 0o644, "1")
			addFile(t, zw, pre+"f", 0o644, "2")
		},
	}
	for name, entries := range bad {
		dst := filepath.Join(t.TempDir(), "out")
		if err := extract(writeZip(t, entries), dst, pre); err == nil {
			t.Errorf("%s: extract succeeded", name)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(dst), "evil")); err == nil {
			t.Errorf("%s: file escaped the destination", name)
		}
	}
}
