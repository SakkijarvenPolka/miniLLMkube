//go:build scriptlocal

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

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests execute the shipped script with a fake offline Hugging Face CLI
// and the host's real python3 validator. They require sh, flock and python3:
// go test -tags scriptlocal ./internal/controller -run TestDownloadSafetensors -v
// No cluster, network or /dev/termination-log is accessed.
type safetensorsRunner struct {
	t                                                *testing.T
	modelDir, binDir, calls, termLog, mode, revision string
}

func newSafetensorsRunner(t *testing.T) *safetensorsRunner {
	t.Helper()
	for _, command := range []string{"sh", "flock", "python3"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("local script test requires %s: %v", command, err)
		}
	}
	dir := t.TempDir()
	r := &safetensorsRunner{t: t, modelDir: filepath.Join(dir, "model"), binDir: filepath.Join(dir, "bin"), calls: filepath.Join(dir, "calls"), termLog: filepath.Join(dir, "termination.log"), mode: "noop"}
	for _, path := range []string{r.modelDir, r.binDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fakeHF := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$HF_CALLS"
[ "$1" = download ]
[ "$2" = example/model ]
shift 2
if [ -n "$EXPECTED_REVISION" ]; then
  [ "$1" = --revision ]
  [ "$2" = "$EXPECTED_REVISION" ]
  shift 2
fi
[ "$1" = --local-dir ]
[ "$2" = "$MODEL_DIR" ]
case "$HF_MODE" in
  resume)
    # The old config+one-shard cache predicate incorrectly bypassed this call.
    [ -s "$MODEL_DIR/model-00001-of-00002.safetensors" ]
    [ ! -e "$MODEL_DIR/.complete" ]
    printf 'second-shard' > "$MODEL_DIR/model-00002-of-00002.safetensors"
    ;;
  fail) exit 23 ;;
  noop) : ;;
  *) exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(r.binDir, "hf"), []byte(fakeHF), 0o700); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *safetensorsRunner) write(name, data string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.modelDir, name), []byte(data), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *safetensorsRunner) fixture(indexed bool, tokenizer string) {
	r.t.Helper()
	r.write("config.json", `{"model_type":"qwen2"}`)
	r.write("model-00001-of-00002.safetensors", "first-shard")
	if indexed {
		r.write("model.safetensors.index.json", `{"weight_map":{"a":"model-00001-of-00002.safetensors","b":"model-00002-of-00002.safetensors"}}`)
		r.write("model-00002-of-00002.safetensors", "second-shard")
	}
	if tokenizer != "" {
		r.write(tokenizer, "tokenizer-data")
	}
}

func (r *safetensorsRunner) run() (string, error) {
	r.t.Helper()
	// Quote the replacement path, so paths containing spaces remain safe.
	logPath := "'" + strings.ReplaceAll(r.termLog, "'", "'\"'\"'") + "'"
	script := strings.ReplaceAll(downloadScriptSafetensors, "/dev/termination-log", logPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"PATH="+r.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"MODEL_DIR="+r.modelDir, "MODEL_REPO=example/model", "MODEL_REVISION="+r.revision,
		"EXPECTED_REVISION="+r.revision, "HF_CALLS="+r.calls, "HF_MODE="+r.mode)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (r *safetensorsRunner) assertMarker(want bool) {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.modelDir, ".complete"))
	if want {
		if err != nil || string(data) != "complete\n" {
			r.t.Fatalf("completion marker = %q, err %v", data, err)
		}
		matches, err := filepath.Glob(filepath.Join(r.modelDir, ".complete.tmp.*"))
		if err != nil || len(matches) != 0 {
			r.t.Fatalf("temporary markers = %v, err %v", matches, err)
		}
	} else if !os.IsNotExist(err) {
		r.t.Fatalf("unexpected completion marker = %q, err %v", data, err)
	}
}

func (r *safetensorsRunner) assertCalls(want int) {
	r.t.Helper()
	data, err := os.ReadFile(r.calls)
	if err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
	got := strings.Count(string(data), "\n")
	if got != want {
		r.t.Fatalf("hf calls = %d, want %d: %s", got, want, data)
	}
}

func (r *safetensorsRunner) assertFailure(reason string) {
	r.t.Helper()
	output, err := r.run()
	if err == nil {
		r.t.Fatalf("expected %s, script succeeded: %s", reason, output)
	}
	data, err := os.ReadFile(r.termLog)
	if err != nil || !strings.HasPrefix(string(data), reason+":") {
		r.t.Fatalf("termination log = %q, err %v; output %s", data, err, output)
	}
	r.assertMarker(false)
	r.assertCalls(1)
}

func TestDownloadSafetensorsResumesPartialRepository(t *testing.T) {
	r := newSafetensorsRunner(t)
	r.fixture(true, "tokenizer.model")
	if err := os.Remove(filepath.Join(r.modelDir, "model-00002-of-00002.safetensors")); err != nil {
		t.Fatal(err)
	}
	r.mode = "resume"
	r.revision = "pinned-revision"
	output, err := r.run()
	if err != nil || strings.Contains(output, "Cache hit") {
		t.Fatalf("partial cache was not resumed: %v\n%s", err, output)
	}
	r.assertCalls(1)
	r.assertMarker(true)
	output, err = r.run()
	if err != nil || !strings.Contains(output, "Cache hit") {
		t.Fatalf("complete cache did not hit: %v\n%s", err, output)
	}
	r.assertCalls(1)
	r.assertMarker(true)
}

func TestDownloadSafetensorsCompleteMarkerRequired(t *testing.T) {
	r := newSafetensorsRunner(t)
	r.fixture(true, "vocab.json")
	// A complete-looking repository without evidence of successful hf download
	// still goes through hf before it is allowed to publish the marker.
	if output, err := r.run(); err != nil {
		t.Fatalf("download failed: %v\n%s", err, output)
	}
	r.assertCalls(1)
	r.assertMarker(true)
}

func TestDownloadSafetensorsMissingShardDoesNotComplete(t *testing.T) {
	for _, staleMarker := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale-marker-%t", staleMarker), func(t *testing.T) {
			r := newSafetensorsRunner(t)
			r.fixture(true, "tokenizer.json")
			if err := os.Remove(filepath.Join(r.modelDir, "model-00002-of-00002.safetensors")); err != nil {
				t.Fatal(err)
			}
			if staleMarker {
				r.write(".complete", "complete\n")
			}
			r.assertFailure("FORMAT_MISMATCH")
		})
	}
}

func TestDownloadSafetensorsCLIFailureDoesNotComplete(t *testing.T) {
	for _, revision := range []string{"", "pinned-revision"} {
		t.Run("revision-"+revision, func(t *testing.T) {
			r := newSafetensorsRunner(t)
			r.fixture(true, "tokenizer.model")
			r.mode, r.revision = "fail", revision
			r.assertFailure("DOWNLOAD_FAILED")
			data, err := os.ReadFile(r.termLog)
			if err != nil || !strings.Contains(string(data), "hf exit 23") {
				t.Fatalf("CLI status not preserved: %q, %v", data, err)
			}
		})
	}
}

func TestDownloadSafetensorsRejectsInvalidFiles(t *testing.T) {
	cases := []struct{ name, file, data string }{
		{"empty-config", "config.json", ""},
		{"invalid-config", "config.json", "not JSON"},
		{"empty-config-object", "config.json", "{}"},
		{"non-object-config", "config.json", `"not a config"`},
		{"empty-shard", "model-00002-of-00002.safetensors", ""},
		{"invalid-index", "model.safetensors.index.json", "not JSON"},
		{"empty-weight-map", "model.safetensors.index.json", `{"weight_map":{}}`},
		{"invalid-shard-name", "model.safetensors.index.json", `{"weight_map":{"a":[]}}`},
		{"empty-tokenizer", "tokenizer.json", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newSafetensorsRunner(t)
			r.fixture(true, "tokenizer.json")
			r.write(tc.file, tc.data)
			r.assertFailure("FORMAT_MISMATCH")
		})
	}
}

func TestDownloadSafetensorsTokenizerAlternatives(t *testing.T) {
	for _, tokenizer := range []string{"tokenizer.json", "tokenizer.model", "vocab.json", "vocab.txt", "spiece.model", "sentencepiece.bpe.model"} {
		t.Run(tokenizer, func(t *testing.T) {
			r := newSafetensorsRunner(t)
			r.fixture(false, tokenizer)
			if output, err := r.run(); err != nil {
				t.Fatalf("valid tokenizer alternative rejected: %v\n%s", err, output)
			}
			r.assertMarker(true)
			r.assertCalls(1)
		})
	}
	for _, tokenizer := range []string{"", "merges.txt"} {
		t.Run("missing-tokenizer-"+tokenizer, func(t *testing.T) {
			r := newSafetensorsRunner(t)
			r.fixture(false, tokenizer)
			r.assertFailure("FORMAT_MISMATCH")
		})
	}
}
