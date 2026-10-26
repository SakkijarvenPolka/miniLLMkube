/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// downloadScriptGGUF is the init container script that materializes the model
// file in the per-source cache directory. It is kept as a single constant and
// pinned by a golden file (testdata/download_gguf.sh) so that the script that
// ships is exactly the script that was tested.
//
// The script only uses commands that exist in curlimages/curl (BusyBox ash,
// uid 100): flock, sha256sum, head, find, mv, wc, sed, tail, mkdir, rm.
//
// Contract with the failure diagnostics: a fatal error writes
// "REASON: message" to /dev/termination-log, so the controller can classify
// the failure by the message prefix.
const downloadScriptGGUF = `set -eu
fail() { echo "$1: $2" > /dev/termination-log; echo "$1: $2" >&2; exit 1; }
mkdir -p "$MODEL_DIR" || fail DOWNLOAD_FAILED "mkdir $MODEL_DIR"
exec 9>"$MODEL_DIR/.lock"
flock 9
F="$MODEL_DIR/$MODEL_FILE"
if [ -s "$F" ]; then echo "Cache hit $F"; exit 0; fi
find "$MODEL_DIR" -maxdepth 1 -name '*.tmp' ! -name "$MODEL_FILE.tmp" -exec rm -f {} +
T="$F.tmp"
if [ -f "$T" ]; then
  R=$(curl -fsIL "$MODEL_URL" | sed -n 's/^[Cc]ontent-[Ll]ength: *\([0-9]*\).*/\1/p' | tail -1)
  L=$(wc -c < "$T")
  if [ -z "$R" ] || [ "$L" -ge "$R" ]; then rm -f "$T"; fi
fi
curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
     --speed-limit 1024 --speed-time 120 -C - -o "$T" "$MODEL_URL" \
  || fail DOWNLOAD_FAILED "curl exit $?"
if [ "$EXPECT_FORMAT" = gguf ] && [ "$(head -c 4 "$T")" != GGUF ]; then
  rm -f "$T"; fail FORMAT_MISMATCH "expected gguf, magic mismatch"
fi
mv "$T" "$F"
`

// downloadScriptSafetensors materializes a Hugging Face repository in the
// per-source cache directory for the vLLM backend. Like the GGUF script it is
// pinned by a golden file (testdata/download_safetensors.sh).
//
// It runs inside the vLLM image, which ships the Hugging Face CLI. The cache is
// considered complete only when both config.json and at least one *.safetensors
// shard are present: a half-downloaded repository must not be served.
//
// Contract with the failure diagnostics: a fatal error writes
// "REASON: message" to /dev/termination-log, so the controller can classify the
// failure by the message prefix.
const downloadScriptSafetensors = `set -eu
fail() { echo "$1: $2" > /dev/termination-log; echo "$1: $2" >&2; exit 1; }
mkdir -p "$MODEL_DIR" || fail DOWNLOAD_FAILED "mkdir $MODEL_DIR"
exec 9>"$MODEL_DIR/.lock"
flock 9
ready() { [ -f "$MODEL_DIR/config.json" ] && ls "$MODEL_DIR"/*.safetensors >/dev/null 2>&1; }
if ready; then echo "Cache hit $MODEL_DIR"; exit 0; fi
find "$MODEL_DIR" -maxdepth 1 -name '*.incomplete' -exec rm -rf {} + 2>/dev/null || true
if command -v hf >/dev/null 2>&1; then
  CLI=hf
elif command -v huggingface-cli >/dev/null 2>&1; then
  CLI=huggingface-cli
else
  fail DOWNLOAD_FAILED "no Hugging Face CLI in the downloader image"
fi
if [ -n "${MODEL_REVISION:-}" ]; then
  "$CLI" download "$MODEL_REPO" --revision "$MODEL_REVISION" --local-dir "$MODEL_DIR" || fail DOWNLOAD_FAILED "$CLI exit $?"
else
  "$CLI" download "$MODEL_REPO" --local-dir "$MODEL_DIR" || fail DOWNLOAD_FAILED "$CLI exit $?"
fi
ready || fail FORMAT_MISMATCH "expected config.json and *.safetensors in $MODEL_DIR"
`
