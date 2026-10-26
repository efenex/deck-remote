package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	maxPhotos       = 4
	maxPhotoBytes   = 8 << 20
	maxPhotoRequest = maxPhotos*maxPhotoBytes + (128 << 10)
)

func photoHarness(tool string) bool { return tool == "claude" || tool == "gemini" }

// askPhotos shares /ask's authTyped gate. The CLI copies files before returning
// its durable receipt, so all request-owned temporary files can be deleted here.
func (s *server) askPhotos(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotoRequest)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooBig *http.MaxBytesError
		code := http.StatusBadRequest
		if errors.As(err, &tooBig) {
			code = http.StatusRequestEntityTooLarge
		}
		httpError(w, code, "invalid photo upload (maximum 4 photos, 8 MiB each)")
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["photos"]
	if len(files) == 0 || len(files) > maxPhotos || len(r.MultipartForm.File) != 1 {
		httpError(w, http.StatusBadRequest, "attach 1 to 4 photos")
		return
	}
	id := strings.TrimSpace(r.FormValue("sessionId"))
	prof := strings.TrimSpace(r.FormValue("profile"))
	text := strings.TrimSpace(r.FormValue("text"))
	if id == "" || len(id) > 200 || len(prof) > 200 || len(text) > 64<<10 {
		httpError(w, http.StatusBadRequest, "invalid session, profile or caption")
		return
	}
	if strings.HasPrefix(text, "/") {
		httpError(w, http.StatusBadRequest, "send photos with a message, not a slash command")
		return
	}
	if text == "" {
		text = "Please look at the attached photos."
	}
	ctx, cancel := context.WithTimeout(withProfile(r.Context(), prof), 30*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if !photoHarness(se.Tool) || se.SSHHost != "" {
		httpError(w, http.StatusBadRequest, "photos are supported for local Claude and Gemini sessions only")
		return
	}
	dir, err := os.MkdirTemp("", "deck-remote-photos-")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "cannot stage photos")
		return
	}
	defer os.RemoveAll(dir)
	paths := make([]string, 0, len(files))
	for _, header := range files {
		if header.Size > maxPhotoBytes {
			httpError(w, http.StatusRequestEntityTooLarge, "photo exceeds 8 MiB")
			return
		}
		f, err := header.Open()
		if err != nil {
			httpError(w, http.StatusBadRequest, "cannot read photo")
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, maxPhotoBytes+1))
		f.Close()
		if err != nil {
			httpError(w, http.StatusBadRequest, "cannot read photo")
			return
		}
		if len(data) > maxPhotoBytes {
			httpError(w, http.StatusRequestEntityTooLarge, "photo exceeds 8 MiB")
			return
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || (format != "jpeg" && format != "png") || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
			httpError(w, http.StatusBadRequest, "photo must be a JPEG or PNG of at most 16 megapixels")
			return
		}
		ext := ".jpg"
		if format == "png" {
			ext = ".png"
		}
		out, err := os.CreateTemp(dir, "photo-*"+ext)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "cannot stage photo")
			return
		}
		_, writeErr := out.Write(data)
		closeErr := out.Close()
		if writeErr != nil || closeErr != nil {
			httpError(w, http.StatusInternalServerError, "cannot stage photo")
			return
		}
		paths = append(paths, out.Name())
	}
	rec, err := s.enqueueSend(ctx, se.ID, text, paths...)
	if err != nil {
		httpError(w, http.StatusBadGateway, fmt.Sprintf("photo send was not confirmed: %v; check the session before retrying", err))
		return
	}
	reqID := newRequestID()
	s.hub.publish(map[string]any{"type": "ask-state", "state": "sent", "requestId": reqID, "sessionId": se.ID, "text": text, "images": len(paths), "ts": time.Now().Unix()})
	s.trackSend(&trackedSend{RequestID: reqID, SessionID: se.ID, SendID: rec.SendID, Profile: prof, Images: len(paths)}, rec)
	writeJSON(w, http.StatusAccepted, map[string]any{"requestId": reqID, "sessionId": se.ID, "sendId": rec.SendID, "status": mapSendState(rec).State, "images": len(paths)})
}
