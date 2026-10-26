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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	testModelSource   = "https://huggingface.co/bartowski/Qwen_Qwen3-0.6B-GGUF/resolve/main/Qwen3-0.6B-Q4_K_M.gguf"
	testModelFileName = "Qwen3-0.6B-Q4_K_M.gguf"
	testNamespace     = "default"
)

func TestCacheKeyFor(t *testing.T) {
	t.Run("same source yields the same key", func(t *testing.T) {
		first := cacheKeyFor(testModelSource)
		second := cacheKeyFor(testModelSource)
		if first != second {
			t.Fatalf("cacheKeyFor is not stable: %q != %q", first, second)
		}
		if len(first) != cacheKeyLength {
			t.Fatalf("cacheKeyFor returned %d characters, want %d", len(first), cacheKeyLength)
		}
	})

	t.Run("query strings are part of the key", func(t *testing.T) {
		plain := cacheKeyFor(testModelSource)
		withQuery := cacheKeyFor(testModelSource + "?download=true")
		if plain == withQuery {
			t.Fatalf("cacheKeyFor ignored the query string: both keys are %q", plain)
		}
	})

	t.Run("different sources yield different keys", func(t *testing.T) {
		other := "https://huggingface.co/bartowski/Qwen_Qwen3-0.6B-GGUF/resolve/main/Qwen3-0.6B-Q8_0.gguf"
		if cacheKeyFor(testModelSource) == cacheKeyFor(other) {
			t.Fatalf("cacheKeyFor collided for different sources")
		}
	})
}

func TestModelFileName(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "plain URL",
			source: testModelSource,
			want:   testModelFileName,
		},
		{
			name:   "query string is ignored for the name",
			source: testModelSource + "?download=true",
			want:   testModelFileName,
		},
		{
			name:   "percent encoded slash falls back",
			source: "https://example.com/models%2Fbad.gguf",
			want:   defaultModelFileName,
		},
		{
			name:   "parent directory segment falls back",
			source: "https://example.com/models/..",
			want:   defaultModelFileName,
		},
		{
			name:   "current directory segment falls back",
			source: "https://example.com/models/.",
			want:   defaultModelFileName,
		},
		{
			name:   "empty path falls back",
			source: "https://example.com",
			want:   defaultModelFileName,
		},
		{
			name:   "root path falls back",
			source: "https://example.com/",
			want:   defaultModelFileName,
		},
		{
			name:   "too long basename falls back",
			source: "https://example.com/" + repeat('a', 201) + ".gguf",
			want:   defaultModelFileName,
		},
		{
			name:   "unparseable source falls back",
			source: "://not-a-url",
			want:   defaultModelFileName,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := modelFileName(test.source); got != test.want {
				t.Fatalf("modelFileName(%q) = %q, want %q", test.source, got, test.want)
			}
		})
	}
}

func TestModelPaths(t *testing.T) {
	model := &llmv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: testNamespace},
		Spec:       llmv1alpha1.ModelSpec{Source: testModelSource},
	}

	wantDir := modelMountPath + "/" + cacheKeyFor(testModelSource)
	if got := modelDirFor(model); got != wantDir {
		t.Fatalf("modelDirFor() = %q, want %q", got, wantDir)
	}
	wantFile := wantDir + "/Qwen3-0.6B-Q4_K_M.gguf"
	if got := modelFilePathFor(model); got != wantFile {
		t.Fatalf("modelFilePathFor() = %q, want %q", got, wantFile)
	}
}

func repeat(char rune, count int) string {
	runes := make([]rune, count)
	for i := range runes {
		runes[i] = char
	}
	return string(runes)
}
