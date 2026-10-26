set -eu
fail() { echo "$1: $2" > /dev/termination-log; echo "$1: $2" >&2; exit 1; }
mkdir -p "$MODEL_DIR" || fail DOWNLOAD_FAILED "mkdir $MODEL_DIR"
exec 9>"$MODEL_DIR/.lock"
flock 9
if ! command -v python3 >/dev/null 2>&1; then
  fail DOWNLOAD_FAILED "no python3 in the downloader image"
fi
validate_repo() {
  python3 - "$MODEL_DIR" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
config = root / "config.json"
if not config.is_file() or config.stat().st_size == 0:
    raise SystemExit("config.json is missing or empty")
try:
    data = json.loads(config.read_text())
except (OSError, ValueError) as exc:
    raise SystemExit(f"config.json is not valid JSON: {exc}")
if not isinstance(data, dict) or not data:
    raise SystemExit("config.json is empty")

weights = [path for path in root.glob("*.safetensors")
           if path.is_file() and path.stat().st_size > 0]
indexes = list(root.glob("*.safetensors.index.json"))
if indexes:
    referenced = set()
    for index in indexes:
        try:
            payload = json.loads(index.read_text())
        except (OSError, ValueError) as exc:
            raise SystemExit(f"safetensors index is not valid JSON: {exc}")
        weight_map = payload.get("weight_map") if isinstance(payload, dict) else None
        if not isinstance(weight_map, dict) or not weight_map:
            raise SystemExit(f"safetensors index has no weight_map: {index.name}")
        for name in weight_map.values():
            if not isinstance(name, str):
                raise SystemExit("safetensors index contains a non-string shard name")
            referenced.add(name)
    for name in referenced:
        if not isinstance(name, str):
            raise SystemExit("safetensors index contains a non-string shard name")
        shard = root / name
        if not shard.is_file() or shard.stat().st_size == 0:
            raise SystemExit(f"safetensors shard is missing or empty: {name}")
if not weights:
    raise SystemExit("no non-empty *.safetensors shard")
# Accept alternative tokenizer layouts, but external-only tokenizers are not
# supported: the downloaded repository must contain at least one usable file.
tokenizers = ("tokenizer.json", "tokenizer.model", "vocab.json", "vocab.txt",
              "spiece.model", "sentencepiece.bpe.model")
if not any((root / name).is_file() and (root / name).stat().st_size > 0
           for name in tokenizers):
    raise SystemExit("no non-empty tokenizer artifact")
PY
}
COMPLETE="$MODEL_DIR/.complete"
if [ -f "$COMPLETE" ]; then
  if validate_repo; then
    echo "Cache hit $MODEL_DIR"
    exit 0
  fi
  rm -f "$COMPLETE"
fi
find "$MODEL_DIR" -maxdepth 1 -name '*.incomplete' -exec rm -rf {} + 2>/dev/null || true
if command -v hf >/dev/null 2>&1; then
  CLI=hf
elif command -v huggingface-cli >/dev/null 2>&1; then
  CLI=huggingface-cli
else
  fail DOWNLOAD_FAILED "no Hugging Face CLI in the downloader image"
fi
if [ -n "${MODEL_REVISION:-}" ]; then
  if "$CLI" download "$MODEL_REPO" --revision "$MODEL_REVISION" --local-dir "$MODEL_DIR"; then :; else
    STATUS=$?
    fail DOWNLOAD_FAILED "$CLI exit $STATUS"
  fi
else
  if "$CLI" download "$MODEL_REPO" --local-dir "$MODEL_DIR"; then :; else
    STATUS=$?
    fail DOWNLOAD_FAILED "$CLI exit $STATUS"
  fi
fi
if ! validate_repo; then
  rm -f "$COMPLETE"
  fail FORMAT_MISMATCH "expected valid config.json and non-empty safetensors shards in $MODEL_DIR"
fi
MARKER_TMP="$COMPLETE.tmp.$$"
printf 'complete\n' > "$MARKER_TMP" || fail DOWNLOAD_FAILED "write $MARKER_TMP"
mv -f "$MARKER_TMP" "$COMPLETE" || fail DOWNLOAD_FAILED "publish $COMPLETE"
