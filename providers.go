package main

import (
	"fmt"
	"net/url"
	"strings"
)

// ProviderAdapter maps a provider-neutral endpoint name to a fixed upstream
// URL. Adapters must never accept upstream hosts or paths from request data.
type ProviderAdapter interface {
	Name() string
	Endpoint(endpoint string) (string, error)
}

// OllamaAdapter describes Ollama's native /api endpoints. Existing clustered
// Ollama routing remains node-aware in server.go; this adapter is also useful
// to other callers that need a validated endpoint mapping.
type OllamaAdapter struct{ BaseURL string }

func (a OllamaAdapter) Name() string { return "ollama" }
func (a OllamaAdapter) Endpoint(endpoint string) (string, error) {
	switch endpoint {
	case "chat", "generate", "embeddings", "embed", "tags":
	default:
		return "", fmt.Errorf("unsupported Ollama endpoint")
	}
	return joinProviderURL(a.BaseURL, "/api/"+endpoint)
}

// vLLMAdapter targets vLLM's OpenAI-compatible API.
type vLLMAdapter struct{ BaseURL string }

func (a vLLMAdapter) Name() string { return "vllm" }
func (a vLLMAdapter) Endpoint(endpoint string) (string, error) {
	switch endpoint {
	case "chat/completions", "completions", "embeddings":
	case "models":
	default:
		return "", fmt.Errorf("unsupported vLLM endpoint")
	}
	return joinProviderURL(a.BaseURL, "/v1/"+endpoint)
}

func joinProviderURL(base, path string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("invalid provider base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}
