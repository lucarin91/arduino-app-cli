// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"log/slog"
	"net/http"

	"github.com/arduino/arduino-app-cli/internal/editor"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

func mountEditor(base http.Handler, cfg config.Configuration, allowedOrigins []string) http.Handler {
	edHandler, err := editor.New(editor.Config{
		Root:   cfg.AppsDir().String(),
		Logger: slog.Default(),
		// TODO: wire allowedOrigins through a CheckOrigin func once the shared
		// origin helper is factored out; phase 1 accepts any origin (the
		// daemon binds 127.0.0.1 and expects an SSH/ADB-tunneled client).
	})
	if err != nil {
		slog.Error("editor: failed to initialize", slog.String("error", err.Error()))
		return base
	}
	_ = allowedOrigins
	mux := http.NewServeMux()
	mux.Handle("/v1/edit", edHandler)
	mux.Handle("/", base)
	return mux
}
