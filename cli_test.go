package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCLIRejectsUnknownOptionsAndCommands(t *testing.T) {
	if got := runCLI([]string{"--definitely-unknown"}); got != 2 {
		t.Fatalf("unknown option exit=%d want 2", got)
	}
	empty, err := os.CreateTemp(t.TempDir(), "empty-*.env")
	if err != nil {
		t.Fatal(err)
	}
	_ = empty.Close()
	if got := runCLI([]string{"this-command-does-not-exist", "--env-file=" + empty.Name()}); got != 2 {
		t.Fatalf("unknown command exit=%d want 2", got)
	}
}

func TestCLIHelpAndVersionDoNotRequireDotenv(t *testing.T) {
	if got := runCLI([]string{"--help"}); got != 0 {
		t.Fatalf("help exit=%d", got)
	}
	old := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	code := runCLI([]string{"--version"})
	_ = write.Close()
	os.Stdout = old
	out, _ := io.ReadAll(read)
	_ = read.Close()
	if code != 0 || !bytes.Contains(out, []byte("ModelMesh")) || !bytes.Contains(out, []byte("commit:")) {
		t.Fatalf("version exit=%d output=%q", code, string(out))
	}
}

func TestCLIEnvFileOptionValidation(t *testing.T) {
	if got := runCLI([]string{"--env-file"}); got != 2 {
		t.Fatalf("missing env path exit=%d", got)
	}
	if got := runCLI([]string{"--env-file", "/no/such/file", "check-config", "agent"}); got != 1 {
		t.Fatalf("missing env file exit=%d", got)
	}
}

func TestCLICheckConfigWithTemporaryEnvFile(t *testing.T) {
	t.Setenv("AGENT_TOKEN", "9a8B7c6D5e4F3g2H1i0J9k8L7m6N5o4P")
	t.Setenv("AGENT_TOKENS", "")
	t.Setenv("ADMIN_TOKEN", "1p2Q3r4S5t6U7v8W9x0Y1z2A3b4C5d6E")
	t.Setenv("ALLOWED_NODE_NETWORKS", "10.0.0.0/8")
	t.Setenv("WEB_DIR", "./web")
	t.Setenv("CONTROLLER_HOST", "127.0.0.1")
	t.Setenv("ALLOW_INSECURE_HTTP", "false")
	t.Setenv("TLS_CERT_FILE", "")
	t.Setenv("TLS_KEY_FILE", "")
	file, err := os.CreateTemp(t.TempDir(), "mesh-*.env")
	if err != nil {
		t.Fatal(err)
	}
	contents := strings.Join([]string{
		"AGENT_TOKEN=9a8B7c6D5e4F3g2H1i0J9k8L7m6N5o4P",
		"ADMIN_TOKEN=1p2Q3r4S5t6U7v8W9x0Y1z2A3b4C5d6E",
		"ALLOWED_NODE_NETWORKS=10.0.0.0/8",
		"WEB_DIR=./web",
	}, "\n")
	if _, err := file.WriteString(contents); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// Do not run the controller; check-config only validates the configuration.
	if got := runCLI([]string{"--env-file", file.Name(), "check-config", "controller"}); got != 0 {
		t.Fatalf("check-config exit=%d", got)
	}
}

func TestCLIRejectsWrongPositionalArguments(t *testing.T) {
	if got := runCLI([]string{"controller", "extra"}); got != 2 {
		t.Fatalf("exit=%d want 2", got)
	}
	if got := runCLI([]string{"check-config"}); got != 2 {
		t.Fatalf("exit=%d want 2", got)
	}
}

func TestControllerListenAddressValidation(t *testing.T) {
	for _, tt := range []struct {
		name, host, port, want string
		bad                    bool
	}{
		{"loopback default", "127.0.0.1", "8000", "127.0.0.1:8000", false},
		{"bracketed IPv6", "[::1]", "9000", "[::1]:9000", false},
		{"IPv6", "::1", "8000", "[::1]:8000", false},
		{"bad hostname syntax", "bad/host", "8000", "", true},
		{"bad colon hostname", "host:name", "8000", "", true},
		{"zero port", "127.0.0.1", "0", "", true},
		{"too large port", "127.0.0.1", "65536", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONTROLLER_HOST", tt.host)
			t.Setenv("CONTROLLER_PORT", tt.port)
			got, err := controllerListenAddress()
			if (err != nil) != tt.bad {
				t.Fatalf("address=%q error=%v bad=%v", got, err, tt.bad)
			}
			if !tt.bad && got != tt.want {
				t.Fatalf("address=%q want %q", got, tt.want)
			}
		})
	}
}
