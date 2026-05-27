# Evals

Run after fetching real assets (`../scripts/fetch-model.sh`). All three tiers
need the model + ONNX Runtime, so they run on your machine, not in CI.

`SR` below = `AE.SampleRate` in `assets/onnx/tts.json`.

## Tier 0 — audio sanity (after generating any job)
    go run ./eval/audiocheck -dir ../outbox/job-<id> -sr <SR>
Also enforced at runtime: a silent/degenerate paragraph fails the job loudly.

## Tier 1 — wrapper parity (seed-pinned)
    SUPERTONIC_ASSETS=../assets go test -tags model_evals ./internal/tts -run Parity -v

## Tier 2 — intelligibility (WER/CER)
    cp eval/asr.sh.example eval/asr.sh && chmod +x eval/asr.sh   # adapt to your Whisper
    SUPERTONIC_ASSETS=../assets go run ./eval/wer \
      -corpus eval/wer/corpus.json -thresholds eval/wer/thresholds.json \
      -asr eval/asr.sh -out /tmp/wer-out

## Acceptance gate
"Done — verified" = Tier 0 green on a real run AND Tier 2 clears the `en` WER
threshold (≤ 0.15). Tier 1 is a wrapper-debugging aid; manual listening is a
backstop, not the gate.
