// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package handlers

import (
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/cli/cli/command"

	"github.com/arduino/arduino-app-cli/internal/api/models"
	"github.com/arduino/arduino-app-cli/internal/orchestrator"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/render"
)

type ResponseLogs struct {
	// ID is "main" for the app's python container or the brick ID for brick containers.
	ID string `json:"id"`
	// ContainerID is the underlying container/compose service reference.
	ContainerID string `json:"container_id"`
	// Message is the log message.
	Message string `json:"message"`
}

func HandleAppLogs(
	dockerClient command.Cli,
	idProvider *app.IDProvider,
	bricksIndex *bricksindex.BricksIndex,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := idProvider.IDFromBase64(r.PathValue("appID"))
		if err != nil {
			render.EncodeResponse(w, http.StatusPreconditionFailed, models.ErrorResponse{Details: "invalid id"})
			return
		}

		app, err := app.Load(id.ToPath())
		if err != nil {
			slog.Error("Unable to parse the app.yaml", slog.String("error", err.Error()), slog.String("path", id.String()))
			render.EncodeResponse(w, http.StatusInternalServerError, models.ErrorResponse{Details: "unable to find the app"})
			return
		}

		queryParams := r.URL.Query()

		showAppLogs, showServicesLogs := true, false
		if filter := queryParams.Get("filter"); filter != "" {
			filters := strings.Split(strings.TrimSpace(filter), ",")
			showServicesLogs = slices.Contains(filters, "bricks")
			showAppLogs = slices.Contains(filters, "main")
		}

		var tail *uint64
		if tailStr := queryParams.Get("tail"); tailStr != "" {
			tailParsed, err := strconv.ParseUint(tailStr, 10, 64)
			if err != nil {
				slog.Error("Unable to parse tail", slog.String("error", err.Error()), slog.String("tail", tailStr))
				render.EncodeResponse(w, http.StatusBadRequest, models.ErrorResponse{Details: "invalid tail value"})
				return
			}
			tail = &tailParsed
		}

		// If the follow query param is set, the default is true
		follow := !queryParams.Has("nofollow")

		appLogsRequest := orchestrator.AppLogsRequest{
			ShowAppLogs:      showAppLogs,
			ShowServicesLogs: showServicesLogs,
			Tail:             tail,
			Follow:           follow,
		}

		sseStream, err := render.NewSSEStream(r.Context(), w)
		if err != nil {
			slog.Error("Unable to create SSE stream", slog.String("error", err.Error()))
			render.EncodeResponse(w, http.StatusInternalServerError, models.ErrorResponse{Details: "unable to create SSE stream"})
			return
		}
		defer sseStream.Close()

		messagesIter, err := orchestrator.AppLogs(r.Context(), app, appLogsRequest, dockerClient, bricksIndex)
		if err != nil {
			sseStream.SendError(render.SSEErrorData{
				Code:    render.InternalServiceErr,
				Message: "failed to start the app",
			})
			return
		}
		for item := range messagesIter {
			id := item.BrickName
			if item.Name == "main" {
				id = "main"
			}
			sseStream.Send(render.SSEEvent{Type: "message", Data: ResponseLogs{
				ID:          id,
				ContainerID: item.Name,
				Message:     item.Content,
			}})
		}
	}
}
