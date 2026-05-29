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
	StatusPaused  JobStatus = "paused" // worker is blocked waiting for /resume or /finalize (manual /pause or auto-pause from pause_every > 0)
	StatusDone    JobStatus = "done"
	StatusError   JobStatus = "error"
)

// errJobNotFound is returned by Pause / Resume / Finalize when the given id
// has no entry in the store. The HTTP layer differentiates it from state-guard
// errors so unknown ids surface as 404 (not 409).
var errJobNotFound = errors.New("job not found")

// errJobFinalized is returned from runInner when the worker exits cleanly via
// a user finalize signal (running or paused). run() translates it to
// StatusDone.
var errJobFinalized = errors.New("job finalized by user")

// Output describes one voice × language bundle within a job.
type Output struct {
	Voice       string   `json:"voice"`
	Lang        string   `json:"lang"`
	FullURL     string   `json:"full"`         // /files/... link to stitched WAV
	Paragraphs  []string `json:"paragraphs"`   // /files/... links, in order
	FullBytes   int64    `json:"full_bytes"`   // size of full.wav on disk
	DurationSec float64  `json:"duration_sec"` // total stitched audio length
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
// paragraph synthesis steps (counted globally across all voice×lang bundles);
// 0 disables auto-pause.
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
// job has no active control entry. The returned struct's channel field
// (resume) may be used without holding s.mu — channel operations are their
// own synchronisation. The boolean and int fields (pauseEvery,
// pauseRequested, finalizeRequested) must be read or written only while
// holding s.mu — re-acquire it after this returns, since the brief read-lock
// taken here is released on return.
func (s *JobStore) getControl(id string) *jobControl {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.controls[id]
}

// Get returns a snapshot of the current job state, or false if the id is
// unknown. The snapshot is taken under s.mu and deep-copies Outputs so
// callers can read it freely without further synchronisation, even while
// the worker continues to update fields in place (updateBundleFromStitch
// overwrites FullURL/FullBytes/DurationSec on the live Output, so a
// shallow copy that shared the backing array would race).
func (s *JobStore) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	cp := *j
	cp.Outputs = append([]Output(nil), j.Outputs...)
	return cp, true
}

// Pause requests a pause; the worker will pause at the next paragraph boundary.
// Errors if the job is not currently running, or if a pause is already pending
// (rejecting a redundant Pause keeps the contract sharp and closes the race
// window where a concurrent Pause's intent would otherwise be silently wiped
// at the worker's resume reset).
func (s *JobStore) Pause(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return errJobNotFound
	}
	if j.Status != StatusRunning {
		return fmt.Errorf("job is %s, cannot pause", j.Status)
	}
	ctl, ok := s.controls[id]
	if !ok {
		return fmt.Errorf("job control missing")
	}
	if ctl.pauseRequested {
		return fmt.Errorf("pause already pending")
	}
	ctl.pauseRequested = true
	return nil
}

// Resume continues a paused job. If all is true, auto-pause is disabled for
// the remainder of the job. Errors if the job is not paused.
func (s *JobStore) Resume(id string, all bool) error {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return errJobNotFound
	}
	if j.Status != StatusPaused {
		st := j.Status
		s.mu.Unlock()
		return fmt.Errorf("job is %s, cannot resume", st)
	}
	ctl := s.controls[id]
	s.mu.Unlock()
	select {
	case ctl.resume <- resumeReq{all: all}:
		return nil
	default:
		return fmt.Errorf("resume already pending")
	}
}

// Finalize ends a job — from paused, signal exit cleanly with the partial
// stitched; from running, set a flag the worker catches at the next paragraph
// boundary. Errors if the job is done/error.
func (s *JobStore) Finalize(id string) error {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return errJobNotFound
	}
	if j.Status != StatusRunning && j.Status != StatusPaused {
		st := j.Status
		s.mu.Unlock()
		return fmt.Errorf("job is %s, cannot finalize", st)
	}
	ctl := s.controls[id]
	if j.Status == StatusPaused {
		s.mu.Unlock()
		select {
		case ctl.resume <- resumeReq{finalize: true}:
			return nil
		default:
			return fmt.Errorf("signal already pending (resume or finalize)")
		}
	}
	ctl.finalizeRequested = true
	s.mu.Unlock()
	return nil
}

type ctlAction int

const (
	ctlContinue ctlAction = iota
	ctlPause
	ctlFinalize
)

// controlGate reads the control flags and decides what to do before the next
// paragraph. Auto-pause fires when stepsSinceResume reaches pauseEvery.
// stepsSinceResume is the global paragraph counter from runInner; it is not
// per-bundle.
func (s *JobStore) controlGate(id string, stepsSinceResume int) ctlAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl, ok := s.controls[id]
	if !ok {
		return ctlContinue
	}
	if ctl.finalizeRequested {
		return ctlFinalize
	}
	if ctl.pauseRequested {
		return ctlPause
	}
	if ctl.pauseEvery > 0 && stepsSinceResume >= ctl.pauseEvery {
		return ctlPause
	}
	return ctlContinue
}

// appendLiveOutput adds a new Output for the in-progress bundle and returns
// its index in job.Outputs. Paragraphs/stitch fields are filled in live.
func (s *JobStore) appendLiveOutput(id, voice, lang string) int {
	var idx int
	s.update(id, func(j *Job) {
		j.Outputs = append(j.Outputs, Output{Voice: voice, Lang: lang})
		idx = len(j.Outputs) - 1
	})
	return idx
}

// updateBundleFromStitch (re)stitches full.wav from the paragraphs rendered so
// far and updates the live Output's stitch fields. Idempotent. No-op if there
// are no paragraphs yet.
func (s *JobStore) updateBundleFromStitch(id string, outputIdx int, bundleDir string, paraPaths []string) {
	if len(paraPaths) == 0 {
		return
	}
	fullPath := filepath.Join(bundleDir, "full.wav")
	if err := audio.Concatenate(paraPaths, fullPath, s.paraGapMs); err != nil {
		return // non-fatal for partial stitch
	}
	var size int64
	if info, err := os.Stat(fullPath); err == nil {
		size = info.Size()
	}
	dur, _ := audio.Duration(fullPath)
	s.update(id, func(j *Job) {
		if outputIdx < len(j.Outputs) {
			j.Outputs[outputIdx].FullURL = fileURL(s.outboxDir, fullPath)
			j.Outputs[outputIdx].FullBytes = size
			j.Outputs[outputIdx].DurationSec = dur
		}
	})
}

// run is the actual worker. One goroutine per job.
func (s *JobStore) run(job *Job, docxPath string) {
	defer s.deleteControls(job.ID)
	s.update(job.ID, func(j *Job) {
		j.Status = StatusRunning
		j.StartedAt = time.Now()
	})

	err := s.runInner(job, docxPath)
	finished := time.Now()

	switch {
	case errors.Is(err, errJobFinalized):
		s.update(job.ID, func(j *Job) {
			j.Status = StatusDone
			j.FinishedAt = finished
		})
	case err != nil:
		s.update(job.ID, func(j *Job) {
			j.Status = StatusError
			j.Error = err.Error()
			j.FinishedAt = finished
		})
	default:
		s.update(job.ID, func(j *Job) {
			j.Status = StatusDone
			j.FinishedAt = finished
		})
	}
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

	// stepsSinceResume counts paragraph syntheses across all (voice, lang)
	// bundles — not per-bundle. Per the spec, pause_every is a global cadence:
	// if a job has multiple bundles, an auto-pause can land mid-bundle on the
	// second/third bundle without resetting between bundles. The counter
	// resets only when the worker resumes from a pause.
	stepsSinceResume := 0

	for _, voice := range job.Voices {
		for _, lang := range job.Langs {
			bundleDir := filepath.Join(jobDir, voice+"_"+lang)
			if err := os.MkdirAll(bundleDir, 0o755); err != nil {
				return err
			}

			outputIdx := s.appendLiveOutput(job.ID, voice, lang)
			var paraPaths []string

			for _, p := range paragraphs {
				switch s.controlGate(job.ID, stepsSinceResume) {
				case ctlFinalize:
					s.updateBundleFromStitch(job.ID, outputIdx, bundleDir, paraPaths)
					return errJobFinalized
				case ctlPause:
					s.updateBundleFromStitch(job.ID, outputIdx, bundleDir, paraPaths)
					s.update(job.ID, func(j *Job) { j.Status = StatusPaused })
					ctl := s.getControl(job.ID)
					req := <-ctl.resume // BLOCK until /resume or /finalize
					if req.finalize {
						return errJobFinalized
					}
					s.mu.Lock()
					ctl.pauseRequested = false
					if req.all {
						ctl.pauseEvery = 0
					}
					s.mu.Unlock()
					stepsSinceResume = 0
					s.update(job.ID, func(j *Job) { j.Status = StatusRunning })
				}

				wavBytes, err := s.engine.Synthesize(p.Text, voice, lang)
				if err != nil {
					return fmt.Errorf("voice=%s lang=%s paragraph %d: %w", voice, lang, p.Index, err)
				}
				name := fmt.Sprintf("para_%03d.wav", p.Index)
				path := filepath.Join(bundleDir, name)
				if err := os.WriteFile(path, wavBytes, 0o644); err != nil {
					return err
				}
				paraPaths = append(paraPaths, path)
				s.update(job.ID, func(j *Job) {
					if outputIdx < len(j.Outputs) {
						j.Outputs[outputIdx].Paragraphs = append(j.Outputs[outputIdx].Paragraphs, fileURL(s.outboxDir, path))
					}
					j.Progress.Done++
				})
				stepsSinceResume++
			}

			// Final stitch for this completed bundle.
			s.updateBundleFromStitch(job.ID, outputIdx, bundleDir, paraPaths)
			// Per-bundle manifest (non-fatal).
			_ = writeBundleManifest(bundleDir, paragraphs, paraPaths)
		}
	}
	return nil
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
	// Guard against short paths: with pause/finalize the caller may pass fewer
	// rendered WAVs than total paragraphs. Iterate over the shorter slice.
	n := len(paragraphs)
	if len(paths) < n {
		n = len(paths)
	}
	for i := 0; i < n; i++ {
		p := paragraphs[i]
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
