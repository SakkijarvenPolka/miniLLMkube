set -eu
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
