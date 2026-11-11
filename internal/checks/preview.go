package checks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
)

// PDFRenderer turns the first page of a PDF into a PNG.
type PDFRenderer interface {
	RenderFirstPage(ctx context.Context, pdfPath string) ([]byte, error)
}

// Poppler renders with pdftoppm from poppler-utils.
type Poppler struct{ Path string }

// FindPoppler returns a renderer if pdftoppm is installed, or nil.
func FindPoppler() PDFRenderer {
	p, err := exec.LookPath("pdftoppm")
	if err != nil {
		return nil
	}
	return Poppler{Path: p}
}

// RenderFirstPage runs pdftoppm with a time limit. The PDF is untrusted, so
// the renderer gets 20 seconds and only page 1.
func (p Poppler) RenderFirstPage(ctx context.Context, pdfPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "preview-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "page")
	cmd := exec.CommandContext(ctx, p.Path, "-png", "-f", "1", "-l", "1", "-singlefile", "-scale-to", "1200", pdfPath, out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pdftoppm: %w: %.200s", err, msg)
	}
	return os.ReadFile(out + ".png")
}

// PreviewKey is where a version's rendered preview is stored. It is fixed
// per version, so a retried job overwrites instead of adding files.
func PreviewKey(versionID string) string { return "previews/" + versionID + ".png" }

func (r *Runner) preview(ctx context.Context, t Target, file string, kind fileKind, readme *Readme) Result {
	d := submissions.PreviewDetail{Kind: submissions.PreviewNone}
	switch {
	case kind.image:
		d = submissions.PreviewDetail{Kind: submissions.PreviewOriginalImage, ContentType: baseMime(kind.mime)}
	case kind.isPDF && r.PDF == nil:
		d.Reason = "no PDF renderer is installed on this server"
	case kind.isPDF:
		png, err := r.PDF.RenderFirstPage(ctx, file)
		if err != nil {
			d.Reason = "the first page could not be rendered"
			break
		}
		if err := r.Store.Put(ctx, PreviewKey(t.VersionID), "image/png", png); err != nil {
			d.Reason = "the preview could not be stored"
			break
		}
		d = submissions.PreviewDetail{Kind: submissions.PreviewImage, Key: PreviewKey(t.VersionID), ContentType: "image/png"}
	case readme != nil:
		d = submissions.PreviewDetail{Kind: submissions.PreviewText, Path: readme.Path, Text: readme.Text}
	case kind.isZip:
		d.Reason = "the archive has no README"
	default:
		d.Reason = "no preview for this file type"
	}
	return Result{Name: CheckPreview, Passed: d.Kind != submissions.PreviewNone, Detail: d}
}
