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

// This file does not run in `make test`: it needs the host's curl, flock and
// sh, while the script under test runs in curlimages/curl (BusyBox ash, uid
// 100). Run it by hand after changing the script:
//
//	go test -tags scriptlocal ./internal/controller -run TestDownloadScript -v

package controller

import (
	"bytes"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const scriptTestFile = "Qwen3-0.6B-Q4_K_M.gguf"

// scriptServer serves a GGUF payload and supports HTTP range requests so the
// resume branch of the script is exercised for real.
type scriptServer struct {
	*httptest.Server
	mu        sync.Mutex
	downloads int
}

func newScriptServer(t *testing.T, payload []byte) *scriptServer {
	t.Helper()
	srv := &scriptServer{}
	srv.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.mu.Lock()
		if r.Method != http.MethodHead {
			srv.downloads++
		}
		srv.mu.Unlock()
		http.ServeContent(w, r, scriptTestFile, time.Time{}, bytes.NewReader(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *scriptServer) downloadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.downloads
}

type scriptRunner struct {
	t        *testing.T
	modelDir string
	termLog  string
	certFile string
	url      string
}

func newScriptRunner(t *testing.T, url string, certFile string) *scriptRunner {
	t.Helper()
	dir := t.TempDir()
	return &scriptRunner{
		t:        t,
		modelDir: dir,
		termLog:  filepath.Join(dir, "termination.log"),
		certFile: certFile,
		url:      url,
	}
}

// run executes the script exactly as the init container does, except that the
// termination log points into the test directory.
func (r *scriptRunner) run() (string, error) {
	script := strings.ReplaceAll(downloadScriptGGUF, "/dev/termination-log", r.termLog)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"MODEL_URL="+r.url,
		"MODEL_DIR="+r.modelDir,
		"MODEL_FILE="+scriptTestFile,
		"EXPECT_FORMAT=gguf",
		"CURL_CA_BUNDLE="+r.certFile,
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (r *scriptRunner) modelPath() string {
	return filepath.Join(r.modelDir, scriptTestFile)
}

func (r *scriptRunner) readModel() []byte {
	r.t.Helper()
	data, err := os.ReadFile(r.modelPath())
	if err != nil {
		r.t.Fatalf("read model file: %v", err)
	}
	return data
}

func (r *scriptRunner) terminationLog() string {
	r.t.Helper()
	data, err := os.ReadFile(r.termLog)
	if err != nil {
		return ""
	}
	return string(data)
}

func writeCert(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: srv.Certificate().Raw,
	}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	return path
}

func ggufPayload(size int) []byte {
	payload := make([]byte, size+4)
	copy(payload, "GGUF")
	for i := 4; i < len(payload); i++ {
		payload[i] = byte('a' + i%26)
	}
	return payload
}

func TestDownloadScriptFreshAndCacheHit(t *testing.T) {
	payload := ggufPayload(1 << 20)
	srv := newScriptServer(t, payload)
	runner := newScriptRunner(t, srv.URL+"/"+scriptTestFile, writeCert(t, srv.Server))

	if output, err := runner.run(); err != nil {
		t.Fatalf("first run failed: %v\n%s", err, output)
	}
	if !bytes.Equal(runner.readModel(), payload) {
		t.Fatalf("downloaded file does not match the payload")
	}
	if got := srv.downloadCount(); got != 1 {
		t.Fatalf("download count = %d, want 1", got)
	}

	output, err := runner.run()
	if err != nil {
		t.Fatalf("second run failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "Cache hit") {
		t.Fatalf("second run did not report a cache hit: %s", output)
	}
	if got := srv.downloadCount(); got != 1 {
		t.Fatalf("download count after cache hit = %d, want 1", got)
	}
}

func TestDownloadScriptResumesPartialFile(t *testing.T) {
	payload := ggufPayload(1 << 20)
	srv := newScriptServer(t, payload)
	runner := newScriptRunner(t, srv.URL+"/"+scriptTestFile, writeCert(t, srv.Server))

	partial := payload[:len(payload)/2]
	if err := os.WriteFile(runner.modelPath()+".tmp", partial, 0o600); err != nil {
		t.Fatalf("write partial file: %v", err)
	}

	if output, err := runner.run(); err != nil {
		t.Fatalf("resume run failed: %v\n%s", err, output)
	}
	if !bytes.Equal(runner.readModel(), payload) {
		t.Fatalf("resumed file does not match the payload")
	}
	if got := srv.downloadCount(); got != 1 {
		t.Fatalf("download count = %d, want 1 (the partial file should be resumed)", got)
	}
}

func TestDownloadScriptRestartsWhenTemporaryFileIsComplete(t *testing.T) {
	payload := ggufPayload(1 << 20)
	srv := newScriptServer(t, payload)
	runner := newScriptRunner(t, srv.URL+"/"+scriptTestFile, writeCert(t, srv.Server))

	// A complete but never renamed temporary file: resuming would append
	// nothing and leave the mv to publish it, so the script must restart.
	if err := os.WriteFile(runner.modelPath()+".tmp", payload, 0o600); err != nil {
		t.Fatalf("write temporary file: %v", err)
	}

	if output, err := runner.run(); err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !bytes.Equal(runner.readModel(), payload) {
		t.Fatalf("restarted file does not match the payload")
	}
	if got := srv.downloadCount(); got != 1 {
		t.Fatalf("download count = %d, want 1 (a full restart)", got)
	}
}

func TestDownloadScriptRejectsWrongFormat(t *testing.T) {
	payload := []byte("<!DOCTYPE html>not a model")
	srv := newScriptServer(t, payload)
	runner := newScriptRunner(t, srv.URL+"/"+scriptTestFile, writeCert(t, srv.Server))

	output, err := runner.run()
	if err == nil {
		t.Fatalf("run succeeded on a non-GGUF payload: %s", output)
	}
	if got := runner.terminationLog(); !strings.Contains(got, "FORMAT_MISMATCH") {
		t.Fatalf("termination log = %q, want FORMAT_MISMATCH", got)
	}
	if _, statErr := os.Stat(runner.modelPath()); !os.IsNotExist(statErr) {
		t.Fatalf("a rejected download must not be published, stat error: %v", statErr)
	}
	if entries, readErr := os.ReadDir(runner.modelDir); readErr == nil {
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".tmp") {
				t.Fatalf("temporary file %q was left behind", entry.Name())
			}
		}
	}
}

func TestDownloadScriptFailsWithoutReachableServer(t *testing.T) {
	srv := newScriptServer(t, ggufPayload(1024))
	url := srv.URL + "/" + scriptTestFile
	cert := writeCert(t, srv.Server)
	srv.Close() // the model is unreachable from now on

	runner := newScriptRunner(t, url, cert)
	output, err := runner.run()
	if err == nil {
		t.Fatalf("run succeeded against a closed server: %s", output)
	}
	if got := runner.terminationLog(); !strings.Contains(got, "DOWNLOAD_FAILED") {
		t.Fatalf("termination log = %q, want DOWNLOAD_FAILED", got)
	}
}
