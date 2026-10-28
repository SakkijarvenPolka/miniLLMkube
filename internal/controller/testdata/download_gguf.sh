set -eu
fail() { echo "$1: $2" > /dev/termination-log; echo "$1: $2" >&2; exit 1; }
mkdir -p "$MODEL_DIR" || fail DOWNLOAD_FAILED "mkdir $MODEL_DIR"
exec 9>"$MODEL_DIR/.lock"
flock 9
F="$MODEL_DIR/$MODEL_FILE"
if [ -s "$F" ]; then echo "Cache hit $F"; exit 0; fi
find "$MODEL_DIR" -maxdepth 1 -name '*.tmp' ! -name "$MODEL_FILE.tmp" -exec rm -f {} +
T="$F.tmp"
DOWNLOADED=0
R=$(curl -fsL -r 0-0 -o /dev/null -D - "$MODEL_URL" 2>/dev/null | sed -n 's|^[Cc]ontent-[Rr]ange: *bytes [0-9]*-[0-9]*/\([0-9]*\).*/\1/p' | tail -1) || true
if [ -f "$T" ] && [ -n "$R" ]; then
  L=$(wc -c < "$T")
  if [ "$L" -gt "$R" ]; then rm -f "$T"; fi
  if [ "$L" -eq "$R" ]; then DOWNLOADED=1; fi
fi
if [ "$DOWNLOADED" -eq 0 ]; then
  i=1
  while :; do
    if curl -fL --connect-timeout 30 --speed-limit 1024 --speed-time 120 \
         -C - -o "$T" "$MODEL_URL"; then
      break
    else
      code=$?
      if [ "$code" -eq 33 ]; then rm -f "$T"; fi
      if [ "$i" -ge 30 ]; then fail DOWNLOAD_FAILED "download aborted after $i attempts (curl exit $code)"; fi
      i=$((i + 1))
      sleep 3
    fi
  done
fi
if [ -n "$R" ]; then
  L=$(wc -c < "$T")
  if [ "$L" -ne "$R" ]; then fail DOWNLOAD_FAILED "downloaded $L of $R bytes"; fi
fi
if [ "$EXPECT_FORMAT" = gguf ] && [ "$(head -c 4 "$T")" != GGUF ]; then
  rm -f "$T"; fail FORMAT_MISMATCH "expected gguf, magic mismatch"
fi
mv "$T" "$F"
