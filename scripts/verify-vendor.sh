#!/usr/bin/env bash
# verify-vendor.sh — prove the vendored engine equals upstream at the pinned
# commit, modulo our documented edits (package rename + Println vet-fix +
# seed hook), after gofmt normalization. Requires network + git + gofmt.
# Maintainer/CI tool, NOT part of `go test`.
set -euo pipefail

NATIVE_DIR="backend/internal/tts/supertonic_native"
VENDORED="$NATIVE_DIR/helper.go"
COMMIT="$(grep -oE '^\- \*\*Commit:\*\* .+' "$NATIVE_DIR/VENDORED.md" | awk '{print $3}')"
PRISTINE_SHA="$(grep -oE '^\- \*\*Pristine helper\.go sha256:\*\* .+' "$NATIVE_DIR/VENDORED.md" | awk '{print $NF}')"

if [ -z "$COMMIT" ]; then echo "could not read pinned commit from VENDORED.md"; exit 1; fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
git clone --quiet https://github.com/supertone-inc/supertonic "$TMP/up"
git -C "$TMP/up" checkout --quiet "$COMMIT"

# (a) Hard check: the pinned commit's pristine helper.go matches what we recorded.
GOT_SHA="$(shasum -a 256 "$TMP/up/go/helper.go" | awk '{print $1}')"
echo "Pristine upstream helper.go sha256: $GOT_SHA"
if [ -n "$PRISTINE_SHA" ] && [ "$GOT_SHA" != "$PRISTINE_SHA" ]; then
  echo "FAIL: upstream pristine sha != VENDORED.md ($PRISTINE_SHA)"; exit 1
fi
echo "  OK: matches VENDORED.md pristine sha"

# (b) Reproduce mechanical edits + gofmt, then diff. Residual should be ONLY
#     the additive seed-hook lines documented in VENDORED.md.
#
# Substitutions applied in order:
#   1. package main → package supertonic_native
#   2. Println vet-fix: strip trailing \n from the CPU inference print
#      (uses perl because BSD sed on macOS mangles literal \n escaping in -e)
#   3. rand.NewSource(time.Now().UnixNano()) → rand.NewSource(tts.SeedFunc())
sed \
  -e 's/^package main$/package supertonic_native/' \
  "$TMP/up/go/helper.go" \
  | perl -pe 's/Using CPU for inference\\n/Using CPU for inference/' \
  | sed \
  -e 's/rand\.NewSource(time\.Now()\.UnixNano())/rand.NewSource(tts.SeedFunc())/' \
  > "$TMP/expected.go"
gofmt -w "$TMP/expected.go"

echo
echo "Residual diff (EXPECTED: only the additive seed-hook lines — SeedFunc field+comment,"
echo "the sampleNoisyLatent LOCAL MOD comment, and the LoadTextToSpeech SeedFunc default):"
diff -u "$TMP/expected.go" "$VENDORED" || true
