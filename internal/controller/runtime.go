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
	"fmt"
	"net/url"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	// runtimeAuto means "not specified": the backend is inferred from the
	// weights format and from the source, and the decision is only reported in
	// status.resolvedRuntime (E29).
	runtimeAuto     = "auto"
	runtimeLlamaCpp = "llamacpp"
	runtimeVLLM     = "vllm"

	formatAuto        = "auto"
	formatGGUF        = "gguf"
	formatSafetensors = "safetensors"

	// inferenceBindAddress is the listen address handed to both engines. The
	// IPv6 unspecified address also accepts IPv4 connections; binding 0.0.0.0
	// breaks the probe path on a dual-stack cluster (E18).
	inferenceBindAddress = "::"

	// healthPath is the probe path of both engines.
	healthPath = "/health"

	hfSourcePrefix = "hf://"
	ggufSuffix     = ".gguf"
)

// Backend renders the parts of the Deployment that depend on the inference
// engine. Everything below is a pure function of the Model and of the
// operator's environment, so the Reconcile loop only orchestrates.
type Backend interface {
	// Name is the value reported in status.resolvedRuntime.
	Name() string
	// DefaultImage is the engine image used when spec.image is empty.
	DefaultImage(gpu bool) string
	// Args renders the engine command line, including spec.extraArgs.
	Args(model *llmv1alpha1.Model, modelPath string) []string
	// Env renders engine-specific environment variables.
	Env(model *llmv1alpha1.Model) []corev1.EnvVar
	HealthPath() string
	StartupFailureThreshold() int32
	// ModelPath is what the engine loads: a file for llama.cpp, the cache
	// directory itself for vLLM.
	ModelPath(model *llmv1alpha1.Model) string
	Downloader(model *llmv1alpha1.Model) corev1.Container
	// PodMutations applies the remaining engine-specific Pod fields.
	PodMutations(spec *corev1.PodSpec, model *llmv1alpha1.Model)
}

// hfSource is a parsed hf://<org>/<repo>[@<revision>] reference.
type hfSource struct {
	repo     string
	revision string
}

// hfNameRe bounds the repository segments the downloader is asked to fetch.
var hfNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

// resolveBackend picks the inference engine and the weights format. An explicit
// runtime wins; otherwise the format decides, and finally the source. The
// returned format is the resolved one, which is what status reports.
func resolveBackend(model *llmv1alpha1.Model) (Backend, string, error) {
	switch model.Spec.Runtime {
	case runtimeLlamaCpp:
		switch model.Spec.Format {
		case formatAuto, formatGGUF, "":
			return llamacppBackend{}, formatGGUF, nil
		}
		return nil, "", fmt.Errorf("llamacpp requires %s weights, got format %q", formatGGUF, model.Spec.Format)
	case runtimeVLLM:
		switch model.Spec.Format {
		case formatAuto, formatSafetensors, "":
			return vllmBackend{}, formatSafetensors, nil
		}
		return nil, "", fmt.Errorf("vllm does not support %s in this operator", model.Spec.Format)
	case runtimeAuto, "":
		return resolveInferredBackend(model)
	default:
		return nil, "", fmt.Errorf("unknown runtime %q", model.Spec.Runtime)
	}
}

// resolveInferredBackend applies the auto rules: an explicit format decides,
// otherwise an hf:// source means safetensors/vLLM and an https .gguf URL means
// llama.cpp.
func resolveInferredBackend(model *llmv1alpha1.Model) (Backend, string, error) {
	switch model.Spec.Format {
	case formatGGUF:
		return llamacppBackend{}, formatGGUF, nil
	case formatSafetensors:
		return vllmBackend{}, formatSafetensors, nil
	case formatAuto, "":
		if strings.HasPrefix(model.Spec.Source, hfSourcePrefix) {
			return vllmBackend{}, formatSafetensors, nil
		}
		if ggufSourceURL(model.Spec.Source) {
			return llamacppBackend{}, formatGGUF, nil
		}
		return nil, "", fmt.Errorf(
			"cannot infer the runtime from source %q: use an https %s URL or set runtime to %s, or an %s<org>/<repo> reference for %s",
			model.Spec.Source, ggufSuffix, runtimeLlamaCpp, hfSourcePrefix, runtimeVLLM,
		)
	}
	return nil, "", fmt.Errorf("unknown format %q", model.Spec.Format)
}

// ggufSourceURL reports whether source is the https URL of a GGUF file.
func ggufSourceURL(source string) bool {
	parsed, err := url.Parse(source)
	if err != nil {
		return false
	}
	return parsed.Scheme == "https" && strings.HasSuffix(parsed.Path, ggufSuffix)
}

// validateSource checks spec.source against the resolved format: llama.cpp
// needs an https URL ending in .gguf, vLLM needs an hf:// repository reference.
func validateSource(model *llmv1alpha1.Model, format string) error {
	if format == formatSafetensors {
		_, err := parseHFSource(model.Spec.Source)
		return err
	}

	sourceURL, err := url.Parse(model.Spec.Source)
	if err != nil {
		return fmt.Errorf("source must be a valid URL: %w", err)
	}
	if sourceURL.Scheme != "https" {
		return fmt.Errorf("source must use https scheme")
	}
	if !strings.HasSuffix(sourceURL.Path, ggufSuffix) {
		return fmt.Errorf("source path must end with %s", ggufSuffix)
	}
	return nil
}

// parseHFSource splits hf://<org>/<repo>[@<revision>]. The revision is optional
// and defaults to the repository's default branch.
func parseHFSource(source string) (hfSource, error) {
	reference, found := strings.CutPrefix(source, hfSourcePrefix)
	if !found {
		return hfSource{}, fmt.Errorf("source must look like %s<org>/<repo>[@<revision>] for %s weights", hfSourcePrefix, formatSafetensors)
	}

	name, revision, hasRevision := strings.Cut(reference, "@")
	segments := strings.Split(name, "/")
	if len(segments) != 2 || !validHFName(segments[0]) || !validHFName(segments[1]) {
		return hfSource{}, fmt.Errorf("source must look like %s<org>/<repo>[@<revision>]", hfSourcePrefix)
	}
	if hasRevision {
		if revision == "" {
			return hfSource{}, fmt.Errorf("source revision after @ must not be empty")
		}
		if strings.ContainsAny(revision, " \t\n") {
			return hfSource{}, fmt.Errorf("source revision must not contain whitespace")
		}
	}
	return hfSource{repo: name, revision: revision}, nil
}

// validHFName bounds one repository segment and rejects the path-traversal
// spellings a repository name must never contain.
func validHFName(name string) bool {
	if name == "." || name == ".." {
		return false
	}
	return hfNameRe.MatchString(name)
}
