package main

import "testing"

func TestVLLMAdapterEndpoints(t *testing.T) {
	a := vLLMAdapter{BaseURL: "http://127.0.0.1:8000"}
	cases := map[string]string{
		"chat/completions": "http://127.0.0.1:8000/v1/chat/completions",
		"completions":      "http://127.0.0.1:8000/v1/completions",
		"embeddings":       "http://127.0.0.1:8000/v1/embeddings",
		"models":           "http://127.0.0.1:8000/v1/models",
	}
	for endpoint, want := range cases {
		got, err := a.Endpoint(endpoint)
		if err != nil || got != want {
			t.Errorf("Endpoint(%q) = %q, %v; want %q", endpoint, got, err, want)
		}
	}
}

func TestProviderAdaptersRejectUnsupportedEndpoints(t *testing.T) {
	for _, tc := range []struct {
		adapter  ProviderAdapter
		endpoint string
	}{
		{vLLMAdapter{BaseURL: "http://127.0.0.1:8000"}, "../../etc/passwd"},
		{OllamaAdapter{BaseURL: "http://127.0.0.1:11434"}, "../../etc/passwd"},
	} {
		if _, err := tc.adapter.Endpoint(tc.endpoint); err == nil {
			t.Errorf("%s accepted unsupported endpoint %q", tc.adapter.Name(), tc.endpoint)
		}
	}
}

func TestProviderBaseURLRejectsCredentialsAndPath(t *testing.T) {
	for _, base := range []string{"http://user:pass@127.0.0.1:8000", "http://127.0.0.1:8000/private", "file:///tmp/provider", "http://127.0.0.1:8000?x=y"} {
		if _, err := joinProviderURL(base, "/v1/models"); err == nil {
			t.Errorf("accepted invalid base URL %q", base)
		}
	}
}
