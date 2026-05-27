package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/yourorg/supertonic-docx/internal/tts"
)

// ServerOpts is what the cmd/server wires together.
type ServerOpts struct {
	Jobs        *JobStore
	OutboxDir   string
	FrontendDir string // static files served at /
}

// NewServer returns an http.Handler with:
//   - GET  /              static frontend
//   - GET  /api/catalogue voices + langs available
//   - POST /api/synthesize  multipart upload → job
//   - GET  /api/jobs/:id  job status
//   - GET  /files/*       static serving of outbox
func NewServer(opts ServerOpts) http.Handler {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	// CORS open by default — this tool is meant to run on localhost or behind
	// the user's own auth proxy. If you deploy publicly, tighten this.
	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"GET", "POST", "OPTIONS"},
		AllowHeaders: []string{"*"},
	}))

	h := &handlers{opts: opts}

	r.GET("/api/catalogue", h.catalogue)
	r.POST("/api/synthesize", h.synthesize)
	r.GET("/api/jobs/:id", h.getJob)

	// Static file serving for generated audio. We use a raw http.FileServer
	// because Gin's StaticFS doesn't give us the path-walking we want here,
	// and we want to be paranoid about path traversal.
	r.GET("/files/*path", h.serveFile)

	// Frontend at root. Single-page; everything else falls through.
	if opts.FrontendDir != "" {
		r.StaticFile("/", filepath.Join(opts.FrontendDir, "index.html"))
		r.Static("/static", opts.FrontendDir)
	}

	return r
}

type handlers struct {
	opts ServerOpts
}

func (h *handlers) catalogue(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"voices":    h.opts.Jobs.engine.Voices(),
		"languages": catalogueLangs(h.opts.Jobs.engine.Languages()),
	})
}

type langEntry struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func catalogueLangs(codes []string) []langEntry {
	out := make([]langEntry, 0, len(codes))
	for _, c := range codes {
		name, ok := tts.LanguageName[c]
		if !ok || name == "" {
			name = c
		}
		out = append(out, langEntry{Code: c, Name: name})
	}
	return out
}

func (h *handlers) synthesize(c *gin.Context) {
	file, hdr, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing 'file' upload"})
		return
	}
	defer file.Close()

	if !strings.EqualFold(filepath.Ext(hdr.Filename), ".docx") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .docx files supported"})
		return
	}

	voices := splitField(c.Request.FormValue("voices"))
	langs := splitField(c.Request.FormValue("langs"))
	if len(voices) == 0 || len(langs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "voices and langs are required"})
		return
	}

	// Save upload to a temp file inside outbox so we don't need a separate
	// upload area. Cleanup is the worker's responsibility (or just leave
	// it: it's evidence of what the user submitted).
	tmpDir := filepath.Join(h.opts.OutboxDir, "_uploads")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp(tmpDir, "upload-*.docx")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tmp.Close()

	job, err := h.opts.Jobs.Submit(tmp.Name(), hdr.Filename, voices, langs)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"job_id":     job.ID,
		"status_url": "/api/jobs/" + job.ID,
	})
}

func (h *handlers) getJob(c *gin.Context) {
	id := c.Param("id")
	job, ok := h.opts.Jobs.Get(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	c.JSON(http.StatusOK, job)
}

// serveFile streams a file from outboxDir, guarding against path traversal.
func (h *handlers) serveFile(c *gin.Context) {
	requested := strings.TrimPrefix(c.Param("path"), "/")
	clean := filepath.Clean(requested)
	if strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
		c.Status(http.StatusBadRequest)
		return
	}
	abs := filepath.Join(h.opts.OutboxDir, clean)

	// Belt-and-braces: confirm the resolved path is still inside outbox.
	absOutbox, _ := filepath.Abs(h.opts.OutboxDir)
	absFile, _ := filepath.Abs(abs)
	if !strings.HasPrefix(absFile, absOutbox+string(os.PathSeparator)) && absFile != absOutbox {
		c.Status(http.StatusBadRequest)
		return
	}

	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		c.Status(http.StatusNotFound)
		return
	}

	// Use http.ServeFile to get Range support (for in-browser audio scrubbing).
	c.Header("Content-Type", contentTypeFor(abs))
	c.Header("Content-Disposition", fmt.Sprintf(`inline; filename=%q`, filepath.Base(abs)))
	http.ServeFile(c.Writer, c.Request, abs)
}

func contentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

func splitField(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
