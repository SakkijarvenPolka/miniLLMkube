set -eu
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
