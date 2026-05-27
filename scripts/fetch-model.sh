#!/usr/bin/env bash
# fetch-model.sh — pull Supertonic 3 ONNX assets from Hugging Face.
#
# We use the Hugging Face git LFS path because the official Python SDK
# resolves assets at runtime via its own cache, and we want the model to
# sit at a known location for the Docker bind mount.
#
# Requires: git, git-lfs.

set -euo pipefail

REPO_URL="https://huggingface.co/Supertone/supertonic-3"
DEST="${1:-./assets}"

if ! command -v git-lfs >/dev/null 2>&1; then
  echo "git-lfs not found. Install it first:"
  echo "  macOS:   brew install git-lfs && git lfs install"
  echo "  Debian:  sudo apt-get install git-lfs && git lfs install"
  exit 1
fi

if [ -d "$DEST/.git" ]; then
  echo "Updating existing assets in $DEST..."
  git -C "$DEST" pull --ff-only
else
  echo "Cloning Supertonic 3 into $DEST..."
  git lfs install
  git clone "$REPO_URL" "$DEST"
fi

echo
echo "Done. Assets live at: $DEST"
for p in onnx/tts.json onnx/unicode_indexer.json onnx/text_encoder.onnx voice_styles; do
  if [ -e "$DEST/$p" ]; then
    echo "  ✓ $p"
  else
    echo "  ✗ MISSING: $p  (asset layout may differ from expectations — check the HF repo tree)"
  fi
done
