package checks

import (
	"archive/zip"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

// ArchiveLimits bound what an uploaded archive may unpack to.
type ArchiveLimits struct {
	MaxEntries    int
	MaxTotalBytes int64   // unpacked, across all entries
	MaxRatio      float64 // unpacked / packed, for entries over 1 MiB
}

// DefaultArchiveLimits are the limits in the design doc.
var DefaultArchiveLimits = ArchiveLimits{MaxEntries: 10_000, MaxTotalBytes: 1 << 30, MaxRatio: 100}

// Readme is README text found in an archive, used as its preview.
type Readme struct {
	Path string
	Text string
}

const readmeMax = 64 << 10

var nestedArchiveExts = []string{".zip", ".7z", ".rar", ".tar", ".gz", ".tgz", ".bz2", ".xz"}

// inspectZip refuses zip bombs and unsafe archives. It reads the central
// directory first (entry count, declared sizes, ratios, names), then really
// unpacks every entry under a hard byte cap, because headers can lie.
func inspectZip(file string, lim ArchiveLimits) (Result, *Readme) {
	if lim.MaxEntries == 0 {
		lim = DefaultArchiveLimits
	}
	fail := func(reason string, extra map[string]any) (Result, *Readme) {
		d := map[string]any{"reason": reason}
		for k, v := range extra {
			d[k] = v
		}
		return Result{Name: CheckArchive, Passed: false, Detail: d}, nil
	}

	r, err := zip.OpenReader(file)
	if err != nil {
		return fail("the archive could not be opened", nil)
	}
	defer r.Close()

	if len(r.File) > lim.MaxEntries {
		return fail("the archive has too many entries", map[string]any{"entries": len(r.File), "max_entries": lim.MaxEntries})
	}
	var declared uint64
	for _, f := range r.File {
		if msg := unsafeName(f.Name); msg != "" {
			return fail(msg, map[string]any{"entry": safeLabel(f.Name)})
		}
		lower := strings.ToLower(f.Name)
		for _, ext := range nestedArchiveExts {
			if strings.HasSuffix(lower, ext) {
				return fail("archives inside the archive are not accepted", map[string]any{"entry": safeLabel(f.Name)})
			}
		}
		declared += f.UncompressedSize64
		if declared > uint64(lim.MaxTotalBytes) {
			return fail("the archive unpacks to more than 1 GiB", map[string]any{"max_bytes": lim.MaxTotalBytes})
		}
		if f.UncompressedSize64 > 1<<20 {
			ratio := float64(f.UncompressedSize64) / float64(max(f.CompressedSize64, 1))
			if ratio > lim.MaxRatio {
				return fail("an entry expands far more than real files do (possible zip bomb)", map[string]any{"entry": safeLabel(f.Name), "ratio": int(ratio)})
			}
		}
	}

	var readme *Readme
	readmeDepth := 1 << 30
	var unpacked int64
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fail("an entry could not be read", map[string]any{"entry": safeLabel(f.Name)})
		}
		budget := lim.MaxTotalBytes - unpacked
		var n int64
		depth := strings.Count(strings.Trim(f.Name, "/"), "/")
		if isReadme(f.Name) && depth < readmeDepth {
			head, err := io.ReadAll(io.LimitReader(rc, readmeMax))
			n = int64(len(head))
			if err == nil && utf8.Valid(head) {
				readme, readmeDepth = &Readme{Path: f.Name, Text: string(head)}, depth
			}
		}
		rest, err := io.Copy(io.Discard, io.LimitReader(rc, budget-n+1))
		rc.Close()
		n += rest
		if err != nil {
			return fail("an entry is corrupt or does not match its header", map[string]any{"entry": safeLabel(f.Name)})
		}
		unpacked += n
		if unpacked > lim.MaxTotalBytes {
			return fail("the archive unpacks to more than 1 GiB", map[string]any{"max_bytes": lim.MaxTotalBytes})
		}
	}
	return Result{Name: CheckArchive, Passed: true, Detail: map[string]any{"entries": len(r.File), "unpacked_bytes": unpacked}}, readme
}

func unsafeName(name string) string {
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) || strings.Contains(name, ":") {
		return "an entry has an absolute path"
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "an entry tries to escape the archive (..)"
		}
	}
	return ""
}

func isReadme(name string) bool {
	switch strings.ToLower(path.Base(name)) {
	case "readme", "readme.md", "readme.txt", "readme.markdown", "readme.rst":
		return true
	}
	return false
}

// safeLabel trims an entry name for display.
func safeLabel(name string) string {
	if !utf8.ValidString(name) {
		return "(unreadable name)"
	}
	if len(name) > 200 {
		return name[:200] + "..."
	}
	return name
}
