// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

// Command editor-server is a minimal standalone host for internal/editor,
// used by container-based benchmarks that need the editor service reachable
// over the network on the same footing as adb/ssh. It is not a shipping
// binary.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/arduino/arduino-app-cli/internal/editor"
)

func main() {
	addr := flag.String("addr", ":9999", "listen address")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil)) //nolint:forbidigo // bench-only binary, no feedback pkg
	h, err := editor.New(editor.Config{Logger: log})
	if err != nil {
		log.Error("editor init failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/edit", h)
	log.Info("editor-server listening", slog.String("addr", *addr))
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("serve failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}
