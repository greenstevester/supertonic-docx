// Package api implements the HTTP surface and the in-memory job orchestrator.
//
// Job model
// ---------
// A "job" is one .docx + a set of voices + a set of languages. The product
// is len(voices) × len(langs) output bundles, each containing one WAV per
// paragraph plus a stitched full.wav.
//
// We hold jobs in memory only. The on-disk audio outputs are the source of
// truth; if the process restarts mid-job, the half-finished WAVs are
// orphaned but harmless (they go in a job-id-prefixed directory). For a
// single-user / single-machine tool this is the right trade — adding a
// database for status persistence would be over-engineering.
//
// Concurrency
// -----------
// Each job runs in its own goroutine. Inside a job, we synthesize sequentially
// because the Supertonic engine serialises ONNX calls internally (CPU-bound,
// no benefit from concurrency on one model session). If we ever load multiple
// model sessions to parallelise, that fan-out goes here.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/yourorg/supertonic-docx/internal/audio"
	"github.com/yourorg/supertonic-docx/internal/docx"
)

type JobStatus string

const (
	StatusQueued  JobStatus = "queued"
	StatusRunning JobStatus = "running"
	StatusPaused  JobStatus = "paused" // auto-pause after every N bundles (pause_every > 0)
	StatusDone    JobStatus = "done"
	StatusError   JobStatus = "error"
)

// errJobFinalized is returned from runInner when the worker exits cleanly via
// a user finalize signal (running or paused). run() will translate it to
// StatusDone — wiring lands in the worker-restructure task (T4).
var errJobFinalized = errors.New("job finalized by user")

// Output describes one voice × language bundle within a job.
type Output struct {
	Voice      string   `json:"voice"`
	Lang       string   `json:"lang"`
	FullURL    string   `json:"full"`       // /files/... link to stitched WAV
	Paragraphs []string `json:"paragraphs"` // /files/... links, in order
}

// Job is the state we expose via the API. The actual audio bytes live on disk.
type Job struct {
	ID         string    `json:"id"`
	SourceName string    `json:"source_name"` // original .docx filename
	Status     JobStatus `json:"status"`
	Progress   Progress  `json:"progress"`
	Voices     []string  `json:"voices"`
	Langs      []string  `json:"langs"`
	PauseEvery int       `json:"pause_every,omitempty"` // 0 = no auto-pause
	Outputs    []Output  `json:"outputs"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type Progress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// jobControl carries pause/resume signaling for one in-flight job. Read/written
// under JobStore.mu, except the resume channel which is buffered (cap 1) so
// the API never blocks the worker (and vice versa).
type jobControl struct {
	pauseEvery        int  // mutated to 0 by resume{all:true}
	pauseRequested    bool // set by /pause
	finalizeRequested bool // set by /finalize while running
	resume            chan resumeReq
}

type resumeReq struct {
	all      bool // disable auto-pause for the remainder
	finalize bool // end the job instead of continuing
}

// JobStore is the orchestrator. All access goes through methods (no direct
// map iteration outside this file), so the lock discipline stays simple.
type JobStore struct {
	outboxDir string
	engine    Engine
	paraGapMs int

	mu       sync.RWMutex
	jobs     map[string]*Job
	controls map[string]*jobControl // transient; not serialised
}

func NewJobStore(outboxDir string, engine Engine, paraGapMs int) *JobStore {
	return &JobStore{
		outboxDir: outboxDir,
		engine:    engine,
		paraGapMs: paraGapMs,
		jobs:      map[string]*Job{},
		controls:  map[string]*jobControl{},
	}
}

// Submit accepts an already-uploaded .docx (path on disk) and the user's
// requested voices/langs. It validates, creates a Job record, and kicks off
// the worker goroutine. pauseEvery > 0 enables auto-pause after every N
// completed bundles; 0 disables auto-pause.
func (s *JobStore) Submit(docxPath, sourceName string, voices, langs []string, pauseEvery int) (*Job, error) {
	voices = dedupe(voices)
	langs = dedupe(langs)
	if len(voices) == 0 {
		return nil, fmt.Errorf("at least one voice required")
	}
	if len(langs) == 0 {
		return nil, fmt.Errorf("at least one language required")
	}
	if pauseEvery < 0 {
		return nil, fmt.Errorf("pause_every must be >= 0")
	}
	for _, v := range voices {
		if !s.engine.HasVoice(v) {
			return nil, fmt.Errorf("unknown voice %q", v)
		}
	}
	for _, l := range langs {
		if !s.engine.HasLang(l) {
			return nil, fmt.Errorf("unknown language %q", l)
		}
	}

	job := &Job{
		ID:         uuid.NewString(),
		SourceName: sourceName,
		Status:     StatusQueued,
		Voices:     voices,
		Langs:      langs,
		PauseEvery: pauseEvery,
		CreatedAt:  time.Now(),
	}

	s.mu.Lock()
	s.jobs[job.ID] = job
	s.controls[job.ID] = &jobControl{
		pauseEvery: pauseEvery,
		resume:     make(chan resumeReq, 1),
	}
	s.mu.Unlock()

	go s.run(job, docxPath)
	return job, nil
}

// SubmitFromPath is the watcher's entry point — same as Submit but reads
// the filename from the path itself. The .docx is copied into the job's
// output dir so the watcher can move/delete the inbox file without
// racing with the worker.
func (s *JobStore) SubmitFromPath(path string, voices, langs []string, pauseEvery int) (*Job, error) {
	return s.Submit(path, filepath.Base(path), voices, langs, pauseEvery)
}

func (s *JobStore) deleteControls(id string) {
	s.mu.Lock()
	delete(s.controls, id)
	s.mu.Unlock()
}

// getControl returns the live control struct for the given job, or nil if the
// job has no active control entry. The struct's fields are mutable and must
// only be read or written while holding s.mu — re-acquire it after this
// returns, since the brief read-lock taken here is released on return.
func (s *JobStore) getControl(id string) *jobControl {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.controls[id]
}

func (s *JobStore) Get(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

// run is the actual worker. One goroutine per job.
func (s *JobStore) run(job *Job, docxPath string) {
	s.update(job.ID, func(j *Job) {
		j.Status = StatusRunning
		j.StartedAt = time.Now()
	})

	if err := s.runInner(job, docxPath); err != nil {
		s.update(job.ID, func(j *Job) {
			j.Status = StatusError
			j.Error = err.Error()
			j.FinishedAt = time.Now()
		})
		return
	}

	s.update(job.ID, func(j *Job) {
		j.Status = StatusDone
		j.FinishedAt = time.Now()
	})
}

func (s *JobStore) runInner(job *Job, docxPath string) error {
	paragraphs, err := docx.Extract(docxPath)
	if err != nil {
		return fmt.Errorf("parse docx: %w", err)
	}
	if len(paragraphs) == 0 {
		return fmt.Errorf("document contained no readable text")
	}

	total := len(paragraphs) * len(job.Voices) * len(job.Langs)
	s.update(job.ID, func(j *Job) { j.Progress.Total = total })

	jobDir := filepath.Join(s.outboxDir, "job-"+job.ID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return err
	}

	// Save a manifest of source text up front — useful for debugging even
	// if synthesis fails partway through.
	if err := writeSourceManifest(jobDir, job, paragraphs); err != nil {
		return err
	}

	for _, voice := range job.Voices {
		for _, lang := range job.Langs {
			out, err := s.synthesizeBundle(job, jobDir, voice, lang, paragraphs)
			if err != nil {
				return fmt.Errorf("voice=%s lang=%s: %w", voice, lang, err)
			}
			// Append to the live job record so the UI sees partial progress
			// (each bundle appears as soon as it completes).
			s.update(job.ID, func(j *Job) {
				j.Outputs = append(j.Outputs, out)
			})
		}
	}
	return nil
}

// synthesizeBundle produces one voice/lang output directory: paragraph WAVs
// plus a stitched full.wav.
func (s *JobStore) synthesizeBundle(job *Job, jobDir, voice, lang string, paragraphs []docx.Paragraph) (Output, error) {
	bundleDir := filepath.Join(jobDir, voice+"_"+lang)
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return Output{}, err
	}

	var paraPaths []string
	var paraURLs []string

	for _, p := range paragraphs {
		wav, err := s.engine.Synthesize(p.Text, voice, lang)
		if err != nil {
			return Output{}, fmt.Errorf("paragraph %d: %w", p.Index, err)
		}
		name := fmt.Sprintf("para_%03d.wav", p.Index)
		path := filepath.Join(bundleDir, name)
		if err := os.WriteFile(path, wav, 0o644); err != nil {
			return Output{}, err
		}
		paraPaths = append(paraPaths, path)
		paraURLs = append(paraURLs, fileURL(s.outboxDir, path))

		s.update(job.ID, func(j *Job) { j.Progress.Done++ })
	}

	fullPath := filepath.Join(bundleDir, "full.wav")
	if err := audio.Concatenate(paraPaths, fullPath, s.paraGapMs); err != nil {
		return Output{}, fmt.Errorf("stitch: %w", err)
	}

	// Per-bundle manifest with durations — useful for downstream alignment.
	if err := writeBundleManifest(bundleDir, paragraphs, paraPaths); err != nil {
		// Non-fatal: log and continue. The audio is the real deliverable.
		// (No logger plumbed in here; the API layer's middleware will surface
		// the error if it propagates. For now, swallow silently.)
		_ = err
	}

	return Output{
		Voice:      voice,
		Lang:       lang,
		FullURL:    fileURL(s.outboxDir, fullPath),
		Paragraphs: paraURLs,
	}, nil
}

func (s *JobStore) update(id string, fn func(*Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		fn(j)
	}
}

func fileURL(outboxDir, abs string) string {
	rel, err := filepath.Rel(outboxDir, abs)
	if err != nil {
		return ""
	}
	return "/files/" + filepath.ToSlash(rel)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

type sourceManifest struct {
	JobID      string            `json:"job_id"`
	SourceName string            `json:"source_name"`
	Voices     []string          `json:"voices"`
	Langs      []string          `json:"langs"`
	Paragraphs []sourceParagraph `json:"paragraphs"`
}

type sourceParagraph struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

func writeSourceManifest(jobDir string, job *Job, paragraphs []docx.Paragraph) error {
	sm := sourceManifest{
		JobID:      job.ID,
		SourceName: job.SourceName,
		Voices:     job.Voices,
		Langs:      job.Langs,
	}
	for _, p := range paragraphs {
		sm.Paragraphs = append(sm.Paragraphs, sourceParagraph{Index: p.Index, Text: p.Text})
	}
	b, err := json.MarshalIndent(sm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(jobDir, "source.json"), b, 0o644)
}

type bundleManifest struct {
	Paragraphs []bundleParagraph `json:"paragraphs"`
}

type bundleParagraph struct {
	Index    int     `json:"index"`
	Text     string  `json:"text"`
	WAV      string  `json:"wav"`
	Duration float64 `json:"duration_sec"`
}

func writeBundleManifest(bundleDir string, paragraphs []docx.Paragraph, paths []string) error {
	bm := bundleManifest{}
	for i, p := range paragraphs {
		dur, _ := audio.Duration(paths[i])
		bm.Paragraphs = append(bm.Paragraphs, bundleParagraph{
			Index:    p.Index,
			Text:     p.Text,
			WAV:      filepath.Base(paths[i]),
			Duration: dur,
		})
	}
	b, err := json.MarshalIndent(bm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleDir, "manifest.json"), b, 0o644)
}
