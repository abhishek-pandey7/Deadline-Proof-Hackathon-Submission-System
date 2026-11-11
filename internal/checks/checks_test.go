package checks

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"crypto/x509"
	"hash/crc32"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// zipOf builds a zip from name -> content.
func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

const minimalPDF = "%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n"

func TestFileTypeComesFromContentNotTheExtension(t *testing.T) {
	cases := []struct {
		name   string
		body   []byte
		passed bool
	}{
		{"deck.pdf", []byte(minimalPDF), true},
		{"logo.png", pngBytes(t), true},
		{"deck.pdf", pngBytes(t), false},                       // a PNG renamed to .pdf
		{"notes.pdf", []byte("just some text, no pdf"), false}, // text is not an accepted type
		{"code.zip", zipOf(t, map[string][]byte{"main.go": []byte("package main")}), true},
	}
	for _, c := range cases {
		_, res := checkFileType(writeTemp(t, c.name, c.body), c.name)
		if res.Passed != c.passed {
			t.Errorf("%s with %q...: passed=%v, want %v (%v)", c.name, c.body[:min(8, len(c.body))], res.Passed, c.passed, res.Detail)
		}
	}
}

func TestZipBombIsRefusedByRatio(t *testing.T) {
	bomb := zipOf(t, map[string][]byte{"zeros.bin": make([]byte, 50<<20)}) // 50 MiB of zeros packs to ~50 KiB
	res, _ := inspectZip(writeTemp(t, "bomb.zip", bomb), DefaultArchiveLimits)
	if res.Passed || !strings.Contains(res.Detail.(map[string]any)["reason"].(string), "zip bomb") {
		t.Fatalf("bomb passed or wrong reason: %v", res.Detail)
	}
}

func TestZipWithLyingHeaderIsRefused(t *testing.T) {
	real := bytes.Repeat([]byte("x"), 2<<20)
	var packed bytes.Buffer
	fw, _ := flate.NewWriter(&packed, flate.BestCompression)
	fw.Write(real)
	fw.Close()

	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, err := w.CreateRaw(&zip.FileHeader{
		Name: "small.txt", Method: zip.Deflate,
		CompressedSize64: uint64(packed.Len()), UncompressedSize64: 10, // claims 10 bytes
		CRC32: crc32.ChecksumIEEE(real),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Write(packed.Bytes())
	w.Close()

	res, _ := inspectZip(writeTemp(t, "liar.zip", b.Bytes()), DefaultArchiveLimits)
	if res.Passed {
		t.Fatalf("a zip whose header understates its size passed: %v", res.Detail)
	}
}

func TestUnsafeArchivesAreRefused(t *testing.T) {
	cases := map[string]map[string][]byte{
		"path traversal": {"../../etc/cron.d/evil": []byte("x")},
		"absolute path":  {"/etc/passwd": []byte("x")},
		"nested archive": {"inner.zip": []byte("PK")},
		"too many entries": func() map[string][]byte {
			m := map[string][]byte{}
			for i := range 30 {
				m[strings.Repeat("d", i+1)] = nil
			}
			return m
		}(),
	}
	limits := DefaultArchiveLimits
	limits.MaxEntries = 20
	for name, entries := range cases {
		res, _ := inspectZip(writeTemp(t, "a.zip", zipOf(t, entries)), limits)
		if res.Passed {
			t.Errorf("%s: passed, want refused", name)
		}
	}
}

func TestNormalZipPassesAndFindsTheShallowestReadme(t *testing.T) {
	z := zipOf(t, map[string][]byte{
		"rocket-main/README.md":       []byte("# Rocket\nShips things."),
		"rocket-main/docs/readme.txt": []byte("deeper readme"),
		"rocket-main/src/main.go":     []byte("package main"),
		"rocket-main/assets/logo.svg": []byte("<svg/>"),
	})
	res, readme := inspectZip(writeTemp(t, "rocket.zip", z), DefaultArchiveLimits)
	if !res.Passed {
		t.Fatalf("normal zip refused: %v", res.Detail)
	}
	if readme == nil || readme.Path != "rocket-main/README.md" || !strings.HasPrefix(readme.Text, "# Rocket") {
		t.Fatalf("readme = %+v, want the top-level README.md", readme)
	}
}

// A one-page PDF that Poppler can render (it rebuilds the missing xref table).
const onePagePDF = "%PDF-1.4\n" +
	"1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\n" +
	"2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj\n" +
	"3 0 obj << /Type /Page /Parent 2 0 R /MediaBox [0 0 200 120] >> endobj\n" +
	"trailer << /Root 1 0 R >>\n%%EOF\n"

func TestPDFPreviewIsTheFirstPageAsPNG(t *testing.T) {
	renderer := FindPoppler()
	if renderer == nil {
		if os.Getenv("CI") != "" {
			t.Fatal("pdftoppm must be installed in CI")
		}
		t.Skip("pdftoppm (poppler-utils) not installed")
	}
	out, err := renderer.RenderFirstPage(context.Background(), writeTemp(t, "deck.pdf", []byte(onePagePDF)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("preview is not a PNG: %v", err)
	}
}

func TestPrivateAddressesAreBlocked(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "192.168.29.1", "172.16.0.9", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1"} {
		if !blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s is not blocked", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "104.16.0.1", "2606:4700:4700::1111"} {
		if blocked(netip.MustParseAddr(a)) {
			t.Errorf("public %s is blocked", a)
		}
	}
}

func TestLinkCheckerRefusesToReachTheLocalNetwork(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	res := NewLinkChecker(LinkOptions{}).Check(context.Background(), srv.URL)
	if res.OK || res.Error != errBlockedAddress.Error() {
		t.Fatalf("link to %s: %+v, want blocked", srv.URL, res)
	}
}

func TestLinkCheckerReportsEachLink(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/no-head", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	lc := NewLinkChecker(LinkOptions{AllowPrivate: true, RootCAs: roots})

	all := lc.CheckAll(context.Background(), []string{srv.URL + "/ok", srv.URL + "/moved", srv.URL + "/no-head"})
	if !all.Passed {
		t.Fatalf("working links failed: %+v", all.Detail)
	}
	bad := lc.CheckAll(context.Background(), []string{srv.URL + "/ok", srv.URL + "/gone"})
	links := bad.Detail.(map[string]any)["links"].([]LinkResult)
	if bad.Passed || links[1].Status != http.StatusNotFound || !links[0].OK {
		t.Fatalf("one dead link: passed=%v %+v", bad.Passed, links)
	}
}
