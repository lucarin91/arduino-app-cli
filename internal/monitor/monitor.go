// This file is part of arduino-app-cli.
//
// Copyright 2025 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU General Public License version 3,
// which covers the main part of arduino-app-cli.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/gpl-3.0.en.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package monitor

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

type MessageReaderWriter interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Close() error
}

func NewMonitorHandler(rw MessageReaderWriter) (func(), error) {
	// Connect to monitor
	mon, err := net.DialTimeout("tcp", "127.0.0.1:7500", time.Second)
	if err != nil {
		return nil, err
	}

	return func() {
		monitorStream(mon, rw)
	}, nil
}

func monitorStream(mon net.Conn, rw MessageReaderWriter) {
	logWebsocketError := func(msg string, err error) {
		// Do not log simple close or interruption errors
		if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure) {
			if e, ok := err.(*websocket.CloseError); ok {
				slog.Error(msg, slog.String("closecause", fmt.Sprintf("%d: %s", e.Code, err)))
			} else {
				slog.Error(msg, slog.String("error", err.Error()))
			}
		}
	}
	logSocketError := func(msg string, err error) {
		if !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			slog.Error(msg, slog.String("error", err.Error()))
		}
	}
	go func() {
		defer mon.Close()
		defer rw.Close()
		for {
			// Read from websocket and write to monitor
			_, msg, err := rw.ReadMessage()
			if err != nil {
				logWebsocketError("Error reading from websocket", err)
				return
			}
			if _, err := mon.Write(msg); err != nil {
				logSocketError("Error writing to monitor", err)
				return
			}
		}
	}()
	go func() {
		defer mon.Close()
		defer rw.Close()
		buff := [1024]byte{}
		for {
			// Read from monitor and write to websocket
			n, err := mon.Read(buff[:])
			if err != nil {
				logSocketError("Error reading from monitor", err)
				return
			}

			if err := rw.WriteMessage(websocket.BinaryMessage, buff[:n]); err != nil {
				logWebsocketError("Error writing to websocket", err)
				return
			}
		}
	}()
}
