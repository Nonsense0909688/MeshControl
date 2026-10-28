// ModelMesh: one binary, two roles.
//
//	meshcontrol controller    central control plane + web UI + Ollama proxy
//	meshcontrol agent         node agent that runs next to an Ollama server
//
// Configuration is read from environment variables (see usage()).
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var log = slog.New(slog.NewTextHandler(os.Stderr, nil))

// Set these at build time with -ldflags when producing a release.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func setupLogging() {
	var lvl slog.LevelVar
	_ = lvl.UnmarshalText([]byte(envOr("LOG_LEVEL", "INFO")))
	log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &lvl}))
}

func usage() {
	fmt.Fprint(os.Stderr, `ModelMesh: control plane for a LAN cluster of Ollama instances.

Usage:
  meshcontrol [--env-file PATH] <controller|agent>
  meshcontrol [--env-file PATH] check-config <controller|agent>
  meshcontrol [--env-file PATH] doctor <controller|agent>
  meshcontrol version

Commands:
  controller       Start the control plane, web UI and streaming Ollama proxy.
  agent            Start a node agent beside an Ollama server.
  check-config     Validate required settings without starting a service.
  doctor           Validate settings and probe the relevant local/remote services.
  version          Print version/build metadata.

Global options:
  --env-file PATH  Load a specific dotenv file (missing file is an error).
  --help, -h       Show this help.
  --version        Print version/build metadata.

Important settings:
  AGENT_TOKEN or AGENT_TOKENS             Shared or per-node agent authentication secrets.
  ADMIN_TOKEN (controller, 32+ chars) Required API/UI bearer token.
  CONTROLLER_HOST (127.0.0.1)       Listen address; choose a LAN IP/0.0.0.0 to share.
  CONTROLLER_PORT (8000)             Listen port.
  TLS_CERT_FILE / TLS_KEY_FILE       Optional native HTTPS certificate/key pair.
  ALLOW_INSECURE_HTTP (false)        Explicitly allow non-loopback plain HTTP.
  WEB_DIR (./web)                    Folder containing HTML pages and static/.
  ALLOWED_NODE_PORTS (11434)      Ollama ports reachable by the controller.
  MAX_BODY_MB (8)                    Max API request body, 1–64 MiB.
  MAX_CONCURRENT_REQUESTS (256)      Bounded HTTP concurrency.
  MAX_CONCURRENT_PULLS (4)           Global active model-pull limit.
  MAX_CONCURRENT_PULLS_PER_NODE (1)  Per-node active model-pull limit.

Agent settings:
  CONTROLLER_URL (required)           Controller base URL.
  NODE_ID (hostname)                  Node name (letters, digits, '.', '_' and '-').
  OLLAMA_LOCAL_URL (127.0.0.1:11434) Local Ollama base URL.
  OLLAMA_ADVERTISE_URL (auto)         URL the controller can reach.
  OLLAMA_PORT (11434)                 Advertised Ollama port.
  HEARTBEAT_INTERVAL (5)              Seconds between heartbeats (1–3600).

Generate unique secrets with: openssl rand -hex 32
`)
}

func printVersion() {
	fmt.Printf("ModelMesh %s\ncommit: %s\nbuilt: %s\n", version, commit, buildDate)
}

// isLoopbackListenHost recognizes only explicit loopback addresses/names; arbitrary
// hostnames are not resolved here to avoid making the startup security decision
// depend on DNS configuration.
func isLoopbackListenHost(host string) bool {
	host = strings.TrimSpace(host)
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

// controllerTLSFromEnv validates TLS settings and prevents accidentally exposing
// bearer tokens over plaintext HTTP on a non-loopback interface. Setting
// ALLOW_INSECURE_HTTP=true is an explicit escape hatch for trusted lab networks.
func controllerTLSFromEnv() (certFile, keyFile string, err error) {
	certFile = strings.TrimSpace(os.Getenv("TLS_CERT_FILE"))
	keyFile = strings.TrimSpace(os.Getenv("TLS_KEY_FILE"))
	if (certFile == "") != (keyFile == "") {
		return "", "", fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	}
	if certFile != "" {
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return "", "", fmt.Errorf("TLS certificate/key pair is invalid: %w", err)
		}
		return certFile, keyFile, nil
	}
	host := envOr("CONTROLLER_HOST", "127.0.0.1")
	loopback := isLoopbackListenHost(host)
	allowPlainHTTP, err := envBoolChecked("ALLOW_INSECURE_HTTP", false)
	if err != nil {
		return "", "", err
	}
	if !loopback && !allowPlainHTTP {
		return "", "", fmt.Errorf("non-loopback HTTP exposes bearer tokens; configure TLS_CERT_FILE and TLS_KEY_FILE, or explicitly set ALLOW_INSECURE_HTTP=true for a trusted lab network")
	}
	return "", "", nil
}

func runCheckConfig(role string) int {
	switch role {
	case "controller":
		cfg, err := controllerConfigFromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "controller configuration invalid:", err)
			return 1
		}
		web := envOr("WEB_DIR", "./web")
		info, err := os.Stat(web)
		if err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "WEB_DIR %q is missing or not a directory\n", web)
			return 1
		}
		if _, err := controllerListenAddress(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cert, _, err := controllerTLSFromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		transport := "HTTP (loopback only)"
		if cert != "" {
			transport = "HTTPS"
		} else if !isLoopbackListenHost(envOr("CONTROLLER_HOST", "127.0.0.1")) {
			transport = "HTTP (explicitly allowed)"
		}
		fmt.Printf("controller configuration OK (transport=%s, web=%s, max_body=%d MiB, request_limit=%d, pull_limit=%d)\n", transport, filepath.Clean(web), cfg.MaxBodyBytes/(1024*1024), cfg.MaxConcurrentRequests, cfg.MaxConcurrentPulls)
	case "agent":
		cfg, err := agentConfigFromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent configuration invalid:", err)
			return 1
		}
		fmt.Printf("agent configuration OK (node=%s, controller=%s, heartbeat=%s)\n", cfg.NodeID, cfg.ControllerURL, cfg.HeartbeatInterval)
	default:
		fmt.Fprintf(os.Stderr, "check-config expects controller or agent, got %q\n", role)
		return 2
	}
	return 0
}

func runDoctor(role string) int {
	if code := runCheckConfig(role); code != 0 {
		return code
	}
	switch role {
	case "controller":
		fmt.Println("doctor: configuration and web directory checks passed; network bind availability is checked when the controller starts")
		return 0
	case "agent":
		cfg, err := agentConfigFromEnv()
		if err != nil {
			return 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ControllerURL+"/healthz", nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "doctor: invalid health URL:", err)
			return 1
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "doctor: controller not reachable:", err)
			return 1
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			fmt.Fprintf(os.Stderr, "doctor: controller health endpoint returned HTTP %d\n", resp.StatusCode)
			return 1
		}
		models, err := NewAgent(cfg).modelsAt(ctx, cfg.OllamaLocalURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "doctor: local Ollama API not reachable:", err)
			return 1
		}
		fmt.Printf("doctor: controller reachable; local Ollama reachable (%d model(s))\n", len(models))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "doctor expects controller or agent, got %q\n", role)
		return 2
	}
}

func runCLI(args []string) int {
	if len(args) == 0 { /* configuration may set the mode */
	}
	envFile, explicit := os.Getenv("ENV_FILE"), strings.TrimSpace(os.Getenv("ENV_FILE")) != ""
	if !explicit {
		envFile = ".env"
	}
	positional := make([]string, 0, 2)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--help" || a == "-h" || a == "help":
			usage()
			return 0
		case a == "--version" || a == "-v" || a == "version":
			if len(positional) > 0 {
				positional = append(positional, a)
				continue
			}
			printVersion()
			return 0
		case a == "--env-file" || a == "-env-file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--env-file needs a path")
				return 2
			}
			i++
			envFile, explicit = args[i], true
		case strings.HasPrefix(a, "--env-file="):
			envFile, explicit = strings.TrimPrefix(a, "--env-file="), true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "unknown option %q\n", a)
			return 2
		default:
			positional = append(positional, a)
		}
	}
	loaded, err := loadDotEnv(envFile, explicit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot load env file:", err)
		return 1
	}
	if loaded > 0 {
		fmt.Fprintf(os.Stderr, "loaded %d setting(s) from %s\n", loaded, envFile)
	}
	mode := ""
	if len(positional) > 0 {
		mode = strings.ToLower(strings.TrimSpace(positional[0]))
	} else {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("MESHCONTROL_MODE")))
	}
	setupLogging()
	switch mode {
	case "controller":
		if len(positional) != 1 {
			fmt.Fprintln(os.Stderr, "controller does not accept positional arguments")
			return 2
		}
		return runController()
	case "agent":
		if len(positional) != 1 {
			fmt.Fprintln(os.Stderr, "agent does not accept positional arguments")
			return 2
		}
		return runAgent()
	case "check-config":
		if len(positional) != 2 {
			fmt.Fprintln(os.Stderr, "usage: meshcontrol [--env-file PATH] check-config <controller|agent>")
			return 2
		}
		return runCheckConfig(strings.ToLower(positional[1]))
	case "doctor":
		if len(positional) != 2 {
			fmt.Fprintln(os.Stderr, "usage: meshcontrol [--env-file PATH] doctor <controller|agent>")
			return 2
		}
		return runDoctor(strings.ToLower(positional[1]))
	case "":
		usage()
		return 2
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", mode)
		usage()
		return 2
	}
}

func main() { os.Exit(runCLI(os.Args[1:])) }
