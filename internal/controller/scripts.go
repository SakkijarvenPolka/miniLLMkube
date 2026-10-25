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
