package httpapi

import (
	"net/http"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/auth"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
)

func (s *Server) handleTeamSubmission(w http.ResponseWriter, r *http.Request, u auth.User) {
	teamID := pathID(w, r, "teamID")
	if teamID == "" {
		return
	}
	out, err := s.Submissions.ForTeam(r.Context(), u.ID, teamID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateVersion starts a version. The Idempotency-Key header is
// required: the browser makes one per "Submit" click and reuses it on retry.
func (s *Server) handleCreateVersion(w http.ResponseWriter, r *http.Request, u auth.User) {
	teamID := pathID(w, r, "teamID")
	if teamID == "" {
		return
	}
	var body struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Links       []string `json:"links"`
		File        struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"file"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	res, err := s.Submissions.Create(r.Context(), u.ID, teamID, submissions.CreateInput{
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Title:          body.Title,
		Description:    body.Description,
		Links:          body.Links,
		FileName:       body.File.Name,
		FileSize:       body.File.Size,
		FileSHA256:     body.File.SHA256,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

func (s *Server) handleUploadedParts(w http.ResponseWriter, r *http.Request, u auth.User) {
	id := pathID(w, r, "versionID")
	if id == "" {
		return
	}
	out, err := s.Submissions.Uploaded(r.Context(), u.ID, id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSignParts(w http.ResponseWriter, r *http.Request, u auth.User) {
	id := pathID(w, r, "versionID")
	if id == "" {
		return
	}
	var body struct {
		Parts []int `json:"parts"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := s.Submissions.SignParts(r.Context(), u.ID, id, body.Parts)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDownload returns a 60-second signed link to a member's own file.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, u auth.User) {
	id := pathID(w, r, "versionID")
	if id == "" {
		return
	}
	link, err := s.Submissions.DownloadURL(r.Context(), u.ID, id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, link)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request, u auth.User) {
	id := pathID(w, r, "versionID")
	if id == "" {
		return
	}
	p, err := s.Submissions.Preview(r.Context(), u.ID, id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request, u auth.User) {
	id := pathID(w, r, "versionID")
	if id == "" {
		return
	}
	var body struct{}
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	v, err := s.Submissions.Complete(r.Context(), u.ID, id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": v})
}
