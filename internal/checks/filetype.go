package checks

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

// allowedTypes maps each accepted content type to the extensions it may use.
var allowedTypes = map[string][]string{
	"application/pdf": {".pdf"},
	"application/zip": {".zip"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {".pptx"},
	"image/png":  {".png"},
	"image/jpeg": {".jpg", ".jpeg"},
	"video/mp4":  {".mp4"},
}

// fileKind is what the content turned out to be.
type fileKind struct {
	mime  string
	isZip bool // zip or a zip-based format such as .pptx
	isPDF bool
	image bool
}

// checkFileType detects the type from the file's bytes. The extension only
// has to agree with the content; it never decides the type.
func checkFileType(path, fileName string) (fileKind, Result) {
	mt, err := mimetype.DetectFile(path)
	if err != nil {
		return fileKind{}, Result{Name: CheckFileType, Passed: false, Detail: map[string]any{"reason": "could not read the file"}}
	}
	kind := fileKind{mime: mt.String()}
	for m := mt; m != nil; m = m.Parent() {
		if m.Is("application/zip") {
			kind.isZip = true
		}
	}
	kind.isPDF = mt.Is("application/pdf")
	kind.image = mt.Is("image/png") || mt.Is("image/jpeg")

	ext := strings.ToLower(filepath.Ext(fileName))
	detail := map[string]any{"detected": baseMime(mt.String()), "extension": ext}

	var allowedExts []string
	for t, exts := range allowedTypes {
		if mt.Is(t) {
			allowedExts = exts
			detail["detected"] = t
		}
	}
	switch {
	case allowedExts == nil:
		detail["reason"] = "this file type is not accepted; upload a PDF, ZIP, PPTX, PNG, JPEG or MP4"
		return kind, Result{Name: CheckFileType, Passed: false, Detail: detail}
	case !slices.Contains(allowedExts, ext):
		detail["reason"] = "the file name's extension does not match its content"
		return kind, Result{Name: CheckFileType, Passed: false, Detail: detail}
	}
	return kind, Result{Name: CheckFileType, Passed: true, Detail: detail}
}

func baseMime(m string) string {
	base, _, _ := strings.Cut(m, ";")
	return base
}
