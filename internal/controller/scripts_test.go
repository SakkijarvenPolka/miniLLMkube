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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDownloadScriptGolden pins the script that ships to the script that was
// reviewed: the constant and testdata/download_gguf.sh must stay byte for byte
// identical. Edit both, never one of them.
func TestDownloadScriptGolden(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "download_gguf.sh"))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	if string(golden) != downloadScriptGGUF {
		t.Fatalf("downloadScriptGGUF drifted from testdata/download_gguf.sh:\n--- constant ---\n%s\n--- golden ---\n%s",
			downloadScriptGGUF, golden)
	}
	if strings.Contains(string(golden), "\r") {
		t.Fatalf("golden file contains CRLF line endings; the init container runs BusyBox ash")
	}
}

// TestDownloadSafetensorsScriptGolden pins the vLLM downloader the same way.
func TestDownloadSafetensorsScriptGolden(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "download_safetensors.sh"))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	if string(golden) != downloadScriptSafetensors {
		t.Fatalf("downloadScriptSafetensors drifted from testdata/download_safetensors.sh:\n--- constant ---\n%s\n--- golden ---\n%s",
			downloadScriptSafetensors, golden)
	}
	if strings.Contains(string(golden), "\r") {
		t.Fatalf("golden file contains CRLF line endings; the init container runs a POSIX shell")
	}
}

// TestDownloadSafetensorsScriptContract keeps the vLLM failure contract: the
// cache is only complete with a config and weights, and a format problem is
// reported with the prefix the controller classifies on.
func TestDownloadSafetensorsScriptContract(t *testing.T) {
	for _, want := range []string{
		`> /dev/termination-log`,
		"DOWNLOAD_FAILED",
		"FORMAT_MISMATCH",
		`"$MODEL_REPO"`,
		"config.json",
		`*.safetensors`,
		"flock 9",
	} {
		if !strings.Contains(downloadScriptSafetensors, want) {
			t.Fatalf("downloadScriptSafetensors no longer contains %q", want)
		}
	}
}

// TestDownloadScriptContract keeps the failure contract the controller
// classifies on: every fatal path writes "REASON: message" to the termination
// log, and the download never bypasses the temporary file.
func TestDownloadScriptContract(t *testing.T) {
	for _, want := range []string{
		`> /dev/termination-log`,
		"DOWNLOAD_FAILED",
		"FORMAT_MISMATCH",
		`-C -`,
		`"$MODEL_DIR"`,
		`mv "$T" "$F"`,
	} {
		if !strings.Contains(downloadScriptGGUF, want) {
			t.Fatalf("downloadScriptGGUF no longer contains %q", want)
		}
	}
}
