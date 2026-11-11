package submissions

import (
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CreateInput is what a team sends to start a new version.
type CreateInput struct {
	IdempotencyKey string
	Title          string
	Description    string
	Links          []string
	FileName       string
	FileSize       int64
	FileSHA256     string // 64 hex characters
}

// Limits enforced on every new version. The database repeats them as checks.
const (
	MaxTitleLen       = 120
	MaxDescriptionLen = 5000
	MaxLinks          = 5
	MaxLinkLen        = 2048
	MaxFileNameLen    = 255
)

var idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// Normalize trims the text fields in place.
func (in *CreateInput) Normalize() {
	in.Title = strings.TrimSpace(in.Title)
	in.Description = strings.TrimSpace(in.Description)
	in.FileName = strings.TrimSpace(in.FileName)
	in.FileSHA256 = strings.ToLower(strings.TrimSpace(in.FileSHA256))
	for i, l := range in.Links {
		in.Links[i] = strings.TrimSpace(l)
	}
}

// Validate returns one message per bad field, or nil when the input is fine.
// The file size limit is per hackathon and is checked by the service.
func (in CreateInput) Validate() map[string]string {
	bad := map[string]string{}

	if !idempotencyKeyRE.MatchString(in.IdempotencyKey) {
		bad["idempotency_key"] = "must be 8-128 letters, digits, - or _"
	}
	switch n := utf8.RuneCountInString(in.Title); {
	case n == 0:
		bad["title"] = "is required"
	case n > MaxTitleLen:
		bad["title"] = "must be at most 120 characters"
	}
	if utf8.RuneCountInString(in.Description) > MaxDescriptionLen {
		bad["description"] = "must be at most 5000 characters"
	}
	if !utf8.ValidString(in.Title) || !utf8.ValidString(in.Description) {
		bad["title"] = "must be valid text"
	}

	if len(in.Links) > MaxLinks {
		bad["links"] = "at most 5 links"
	} else {
		seen := map[string]bool{}
		for _, l := range in.Links {
			if msg := checkLink(l); msg != "" {
				bad["links"] = msg
				break
			}
			if seen[l] {
				bad["links"] = "contains a duplicate"
				break
			}
			seen[l] = true
		}
	}

	if msg := checkFileName(in.FileName); msg != "" {
		bad["file.name"] = msg
	}
	if in.FileSize <= 0 {
		bad["file.size"] = "must be more than 0 bytes"
	}
	if b, err := hex.DecodeString(in.FileSHA256); err != nil || len(b) != 32 {
		bad["file.sha256"] = "must be 64 hex characters"
	}

	if len(bad) == 0 {
		return nil
	}
	return bad
}

func checkLink(l string) string {
	if l == "" || len(l) > MaxLinkLen {
		return "each link must be 1-2048 characters"
	}
	u, err := url.Parse(l)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "each link must be an https:// URL without a username or password"
	}
	return ""
}

func checkFileName(name string) string {
	if name == "" || utf8.RuneCountInString(name) > MaxFileNameLen || !utf8.ValidString(name) {
		return "must be 1-255 characters"
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "must be a plain file name"
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "must not contain control characters"
		}
	}
	return ""
}

// PartCount is how many parts a file of size splits into.
func PartCount(size, partSize int64) int {
	return int((size + partSize - 1) / partSize)
}

// PartLength is the exact size of part n (1-based). Every part is partSize
// except the last, which R2 requires.
func PartLength(size, partSize int64, n int) int64 {
	count := PartCount(size, partSize)
	if n < count {
		return partSize
	}
	return size - partSize*int64(count-1)
}
