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
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path"
	"regexp"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	// modelMountPath is the directory the model volume is mounted at inside
	// every container of the inference Pod.
	modelMountPath = "/models"
	// defaultModelFileName is used when the source URL does not carry a safe
	// file name in its path.
	defaultModelFileName = "model.gguf"
	// cacheKeyLength is the number of hexadecimal characters kept from the
	// source digest: 12 hex characters are 48 bits, enough to keep distinct
	// sources apart while staying readable on the node.
	cacheKeyLength = 12
)

// modelFileNameRe accepts the file names this operator is willing to write to
// the model cache. Anything else falls back to defaultModelFileName so that a
// hostile or unusual URL cannot escape the per-source cache directory.
var modelFileNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

// cacheKeyFor returns the first 12 hexadecimal characters of sha256(source).
// The full source, including its query string, is part of the key: the same
// path served with and without "?download=true" is treated as two sources and
// therefore gets two cache directories (see the cache documentation).
func cacheKeyFor(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])[:cacheKeyLength]
}

// modelFileName returns the basename of the source URL path. It returns
// defaultModelFileName when the path has no usable basename after decoding, or
// when the basename is not safe to use as a plain file name.
func modelFileName(source string) string {
	parsed, err := url.Parse(source)
	if err != nil {
		return defaultModelFileName
	}
	// EscapedPath keeps percent escapes intact, so "%2F" does not turn into a
	// path separator and "../../x" cannot be smuggled in as a file name.
	name := path.Base(parsed.EscapedPath())
	if name == "." || name == ".." || !modelFileNameRe.MatchString(name) {
		return defaultModelFileName
	}
	return name
}

// modelDirFor returns the per-source cache directory of the model.
func modelDirFor(model *llmv1alpha1.Model) string {
	return modelMountPath + "/" + cacheKeyFor(model.Spec.Source)
}

// modelFilePathFor returns the absolute path of the model file inside the
// inference container.
func modelFilePathFor(model *llmv1alpha1.Model) string {
	return modelDirFor(model) + "/" + modelFileName(model.Spec.Source)
}
