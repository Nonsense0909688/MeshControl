// MeshControl: one binary, two roles.
//
//	meshcontrol controller   central control plane + web UI + Ollama proxy
//	meshcontrol agent        node agent that runs next to an Ollama server
//
// Configuration is read from environment variables (see README / usage()).
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

var log = slog.New(slog.NewTextHandler(os.Stderr, nil))

func setupLogging() {
	var lvl slog.LevelVar
	_ = lvl.UnmarshalText([]byte(envOr("LOG_LEVEL", "INFO")))
	log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &lvl}))
}

func usage() {
	fmt.Fprint(os.Stderr, `MeshControl: control plane for a LAN cluster of Ollama instances.

Usage:
  meshcontrol [--env-file PATH] <controller|agent>

  Settings are read from ./.env (or --env-file / ENV_FILE) and the environment;
  real environment variables win over the file. You can also put
  MESHCONTROL_MODE=controller|agent in the file and run with no arguments.

controller   AGENT_TOKEN (required), ADMIN_TOKEN, CONTROLLER_HOST (0.0.0.0),
             CONTROLLER_PORT (8000), WEB_DIR (.), OFFLINE_AFTER (20), PURGE_AFTER (86400),
             ALLOWED_NODE_NETWORKS, ALLOW_HOSTNAMES, VERIFY_SOURCE_IP,
             CONNECT_TIMEOUT (5), READ_TIMEOUT (600), PULL_REGISTRIES, LOG_LEVEL

agent        CONTROLLER_URL (required), AGENT_TOKEN (required), NODE_ID (hostname),
             OLLAMA_LOCAL_URL (http://127.0.0.1:11434), OLLAMA_ADVERTISE_URL (auto),
             OLLAMA_PORT (11434), HEARTBEAT_INTERVAL (5), LOG_LEVEL
`)
}

func main() {
	// Flags: [--env-file PATH] <controller|agent>. ENV_FILE works too.
	// Default: ./.env (silently skipped if it does not exist).
	envFile, explicit := os.Getenv("ENV_FILE"), os.Getenv("ENV_FILE") != ""
	if !explicit {
		envFile = ".env"
	}
	mode := ""
	for args := os.Args[1:]; len(args) > 0; args = args[1:] {
		switch a := args[0]; {
		case a == "--env-file" || a == "-env-file":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "--env-file needs a path")
				os.Exit(2)
			}
			envFile, explicit = args[1], true
			args = args[1:]
		case strings.HasPrefix(a, "--env-file="):
			envFile, explicit = strings.TrimPrefix(a, "--env-file="), true
		case mode == "":
			mode = strings.ToLower(a)
		}
	}

	loaded, err := loadDotEnv(envFile, explicit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot load env file:", err)
		os.Exit(1)
	}
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("MESHCONTROL_MODE")))
	}
	setupLogging()
	if loaded > 0 {
		log.Info("loaded settings from env file", "file", envFile, "variables", loaded)
	}

	switch mode {
	case "controller":
		os.Exit(runController())
	case "agent":
		os.Exit(runAgent())
	case "help", "-h", "--help":
		usage()
	default:
		if mode != "" {
			fmt.Fprintf(os.Stderr, "unknown mode %q\n\n", mode)
		}
		usage()
		os.Exit(2)
	}
}
