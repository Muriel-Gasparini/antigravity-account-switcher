package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallDesktop(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	fakeBin := filepath.Join(tmpHome, "fake-antigravity")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeIcon := filepath.Join(tmpHome, "icon.png")
	if err := os.WriteFile(fakeIcon, []byte("fake-png-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := DesktopOptions{
		SwitcherBin:    "/usr/bin/antigravity-account-switcher",
		AntigravityBin: fakeBin,
		IconPath:       fakeIcon,
		Name:           "Antigravity 2.0 Test",
		Comment:        "Test Comment",
	}

	res, err := InstallDesktop(opts)
	if err != nil {
		t.Fatalf("InstallDesktop failed: %v", err)
	}

	if _, err := os.Stat(res.DesktopFilePath); err != nil {
		t.Errorf("expected desktop file at %s, got error %v", res.DesktopFilePath, err)
	}

	content, err := os.ReadFile(res.DesktopFilePath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(content), "Name=Antigravity 2.0 Test") {
		t.Errorf("expected Name in content, got:\n%s", string(content))
	}
	if !strings.Contains(string(content), "Exec=/usr/bin/antigravity-account-switcher launch %F") {
		t.Errorf("expected Exec launch in content, got:\n%s", string(content))
	}

	// Test Uninstall
	if err := UninstallDesktop(); err != nil {
		t.Fatalf("UninstallDesktop failed: %v", err)
	}
	if _, err := os.Stat(res.DesktopFilePath); !os.IsNotExist(err) {
		t.Errorf("expected desktop file to be removed, but still exists")
	}
}

// closeFailWriter accepts writes but fails on Close, the way a full disk or an
// NFS mount can only report a failed flush when the file is closed.
type closeFailWriter struct {
	buf      strings.Builder
	writeErr error
	closeErr error
	closed   bool
}

func (w *closeFailWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.buf.Write(p)
}

func (w *closeFailWriter) Close() error {
	w.closed = true
	return w.closeErr
}

func TestCopyAndClose_ReportsCloseError(t *testing.T) {
	closeErr := errors.New("close: no space left on device")
	w := &closeFailWriter{closeErr: closeErr}

	err := copyAndClose(w, strings.NewReader("icon-bytes"))
	if !errors.Is(err, closeErr) {
		t.Fatalf("copyAndClose error = %v, want the Close error", err)
	}
	if got := w.buf.String(); got != "icon-bytes" {
		t.Errorf("copied %q, want %q", got, "icon-bytes")
	}
}

func TestCopyAndClose_WriteErrorWinsAndStillCloses(t *testing.T) {
	writeErr := errors.New("write failed")
	w := &closeFailWriter{writeErr: writeErr, closeErr: errors.New("close failed")}

	err := copyAndClose(w, strings.NewReader("icon-bytes"))
	if !errors.Is(err, writeErr) {
		t.Fatalf("copyAndClose error = %v, want the write error", err)
	}
	if !w.closed {
		t.Error("writer was not closed after a failed write")
	}
}

func TestCopyFile_CopiesContents(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.png")
	dst := filepath.Join(dir, "dst.png")
	if err := os.WriteFile(src, []byte("fake-png-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fake-png-data" {
		t.Errorf("dst = %q, want %q", got, "fake-png-data")
	}
}
