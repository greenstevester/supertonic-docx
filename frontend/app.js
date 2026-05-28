/* supertonic-docx — frontend logic.
   No build step, no framework. The whole app is small enough that adding
   one would cost more than it saves. */

const state = {
  file: null,
  voices: [],
  langs: [],
  selectedVoices: new Set(),
  selectedLangs: new Set(),
  jobId: null,
  pollTimer: null,
};

const $ = (id) => document.getElementById(id);

/* ---------- bootstrap ---------- */

(async function init() {
  setupDropper();
  setupSubmit();
  try {
    const cat = await fetch('/api/catalogue').then(r => r.json());
    state.voices = cat.voices || [];
    state.langs = cat.languages || [];
    renderVoiceChips();
    renderLangChips();
    $('meta-voices').textContent = `${state.voices.length} voices`;
    $('meta-langs').textContent  = `${state.langs.length} languages`;
  } catch (err) {
    $('meta-voices').textContent = 'catalogue unavailable';
    $('voice-chips').innerHTML = '<span class="loading-pulse">backend not reachable</span>';
    $('lang-chips').innerHTML  = '<span class="loading-pulse">backend not reachable</span>';
  }
  refreshTally();
})();

/* ---------- file drop ---------- */

function setupDropper() {
  const drop = $('dropper');
  const input = $('file-input');

  drop.addEventListener('click', () => input.click());
  input.addEventListener('change', () => {
    if (input.files[0]) setFile(input.files[0]);
  });

  ['dragenter', 'dragover'].forEach(ev =>
    drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add('is-drag'); })
  );
  ['dragleave', 'drop'].forEach(ev =>
    drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.remove('is-drag'); })
  );
  drop.addEventListener('drop', (e) => {
    const f = e.dataTransfer.files[0];
    if (f) setFile(f);
  });
}

function setFile(f) {
  if (!f.name.toLowerCase().endsWith('.docx')) {
    $('action-note').textContent = 'Only .docx files are supported.';
    return;
  }
  state.file = f;
  $('dropper').classList.add('has-file');
  $('dropper-file').textContent = `${f.name} · ${(f.size / 1024).toFixed(1)} KB`;
  refreshTally();
}

/* ---------- chips ---------- */

function renderVoiceChips() {
  const el = $('voice-chips');
  el.removeAttribute('data-loading');
  el.innerHTML = '';
  for (const v of state.voices) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'chip';
    b.setAttribute('aria-pressed', 'false');
    b.dataset.voice = v;
    b.textContent = v;
    b.addEventListener('click', () => toggleVoice(v, b));
    el.appendChild(b);
  }
  // Sensible default — pick the first voice so the user can see what a
  // ready-to-go state looks like.
  if (state.voices.length > 0) {
    const first = el.querySelector('.chip');
    toggleVoice(state.voices[0], first);
  }
}

function renderLangChips() {
  const el = $('lang-chips');
  el.removeAttribute('data-loading');
  el.innerHTML = '';
  for (const l of state.langs) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'chip';
    b.setAttribute('aria-pressed', 'false');
    b.dataset.lang = l.code;
    b.innerHTML = `${l.name}<span class="chip-code">${l.code}</span>`;
    b.addEventListener('click', () => toggleLang(l.code, b));
    el.appendChild(b);
  }
  // Default to English.
  const enChip = el.querySelector('.chip[data-lang="en"]') || el.querySelector('.chip');
  if (enChip) toggleLang(enChip.dataset.lang, enChip);
}

function toggleVoice(v, btn) {
  if (state.selectedVoices.has(v)) {
    state.selectedVoices.delete(v);
    btn.setAttribute('aria-pressed', 'false');
  } else {
    state.selectedVoices.add(v);
    btn.setAttribute('aria-pressed', 'true');
  }
  refreshTally();
}

function toggleLang(l, btn) {
  if (state.selectedLangs.has(l)) {
    state.selectedLangs.delete(l);
    btn.setAttribute('aria-pressed', 'false');
  } else {
    state.selectedLangs.add(l);
    btn.setAttribute('aria-pressed', 'true');
  }
  refreshTally();
}

function refreshTally() {
  const v = state.selectedVoices.size;
  const l = state.selectedLangs.size;
  $('submit-tally').textContent = `${v} voice${v === 1 ? '' : 's'} · ${l} lang${l === 1 ? '' : 's'}`;
  const ready = state.file && v > 0 && l > 0;
  $('submit').disabled = !ready;
  if (!state.file) {
    $('action-note').textContent = 'Pick a file, at least one voice, and at least one language.';
  } else if (v === 0 || l === 0) {
    $('action-note').textContent = 'Pick at least one voice and one language.';
  } else {
    $('action-note').textContent = `Ready — ${v * l} bundle${v * l === 1 ? '' : 's'} will be produced.`;
  }
}

/* ---------- submission ---------- */

function setupSubmit() {
  $('submit').addEventListener('click', submit);

  $('job-controls').addEventListener('click', async (e) => {
    const action = e.target.dataset.action;
    if (!action || !state.jobId) return;
    const id = state.jobId;
    try {
      if (action === 'pause') {
        await fetch(`/api/jobs/${id}/pause`, { method: 'POST' });
      } else if (action === 'resume') {
        await fetch(`/api/jobs/${id}/resume`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: '{}',
        });
      } else if (action === 'resume-all') {
        await fetch(`/api/jobs/${id}/resume`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ all: true }),
        });
      } else if (action === 'finalize') {
        await fetch(`/api/jobs/${id}/finalize`, { method: 'POST' });
      }
    } catch (err) {
      // The next poll will surface state; nothing else to do here.
    }
  });
}

async function submit() {
  $('submit').disabled = true;
  $('action-note').textContent = 'Uploading…';

  const form = new FormData();
  form.append('file', state.file);
  form.append('voices', [...state.selectedVoices].join(','));
  form.append('langs',  [...state.selectedLangs].join(','));
  const pe = $('pause-every').value.trim();
  if (pe !== '' && Number(pe) > 0) {
    form.append('pause_every', pe);
  }

  let res;
  try {
    res = await fetch('/api/synthesize', { method: 'POST', body: form });
  } catch (e) {
    $('action-note').textContent = 'Network error contacting backend.';
    $('submit').disabled = false;
    return;
  }
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: 'submission failed' }));
    $('action-note').textContent = err.error || 'submission failed';
    $('submit').disabled = false;
    return;
  }
  const { job_id } = await res.json();
  // Upload is done; the job panel below owns status from here. Clear the
  // action-note so "Uploading…" doesn't linger over a running job.
  $('action-note').textContent = '';
  state.jobId = job_id;
  $('job-id').textContent = job_id.slice(0, 8);
  $('job-panel').hidden = false;
  $('job-panel').scrollIntoView({ behavior: 'smooth', block: 'start' });
  startPolling();
}

function startPolling() {
  clearInterval(state.pollTimer);
  state.pollTimer = setInterval(pollJob, 800);
  pollJob();
}

async function pollJob() {
  if (!state.jobId) return;
  let job;
  try {
    job = await fetch(`/api/jobs/${state.jobId}`).then(r => r.json());
  } catch (e) {
    return;
  }
  renderJob(job);
  if (job.status === 'done' || job.status === 'error') {
    clearInterval(state.pollTimer);
    $('submit').disabled = false;
  }
}

function renderJob(job) {
  $('job-status').textContent = job.status;
  $('job-status').dataset.status = job.status;

  const total = job.progress.total || 1;
  const pct = Math.min(100, (job.progress.done / total) * 100);
  $('job-progress-bar').style.right = `${100 - pct}%`;
  $('job-progress-text').textContent =
    `${job.progress.done} / ${job.progress.total} paragraphs synthesised`;

  renderControls(job);

  const outs = $('job-outputs');
  outs.innerHTML = '';
  for (const out of (job.outputs || [])) {
    outs.appendChild(renderBundle(out));
  }

  if (job.status === 'error' && job.error) {
    $('job-error').hidden = false;
    $('job-error').textContent = job.error;
  } else {
    $('job-error').hidden = true;
  }
}

function renderBundle(out) {
  const wrap = document.createElement('div');
  wrap.className = 'bundle';

  const header = document.createElement('div');
  header.className = 'bundle-header';
  const title = document.createElement('span');
  title.className = 'bundle-title';
  title.textContent = `${out.voice} · ${out.lang}`;
  const meta = document.createElement('span');
  meta.className = 'bundle-meta';
  const bytes = out.full_bytes || 0;
  const dur = out.duration_sec || 0;
  meta.textContent =
    `${out.paragraphs.length}¶ · ${formatBytes(bytes)} · ${formatDuration(dur)}`;
  header.append(title, meta);
  wrap.appendChild(header);

  const audio = document.createElement('audio');
  audio.controls = true;
  audio.preload = 'none';
  audio.src = out.full;
  wrap.appendChild(audio);

  const paras = document.createElement('div');
  paras.className = 'bundle-paragraphs';
  out.paragraphs.forEach((p, i) => {
    const a = document.createElement('a');
    a.href = p;
    a.target = '_blank';
    a.rel = 'noopener';
    a.className = 'para-link';
    a.textContent = `¶${String(i + 1).padStart(3, '0')}`;
    paras.appendChild(a);
  });
  wrap.appendChild(paras);

  return wrap;
}

function renderControls(job) {
  const c = $('job-controls');
  const running = job.status === 'running';
  const paused  = job.status === 'paused';
  c.hidden = !(running || paused);
  c.querySelector('[data-action="pause"]').hidden       = !running;
  c.querySelector('[data-action="resume"]').hidden      = !paused;
  c.querySelector('[data-action="resume-all"]').hidden  = !paused;
  c.querySelector('[data-action="finalize"]').hidden    = !(running || paused);
}

function formatBytes(b) {
  if (!b) return '—';
  if (b >= 1024 * 1024) return (b / 1024 / 1024).toFixed(1) + ' MB';
  if (b >= 1024) return (b / 1024).toFixed(0) + ' KB';
  return b + ' B';
}

function formatDuration(s) {
  if (!s) return '—';
  const m = Math.floor(s / 60);
  const sec = Math.floor(s % 60);
  return m > 0 ? `${m}m ${String(sec).padStart(2, '0')}s` : `${sec}s`;
}
