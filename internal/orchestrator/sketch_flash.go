package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/arduino/arduino-cli/commands"
	rpc "github.com/arduino/arduino-cli/rpc/cc/arduino/cli/commands/v1"
	"github.com/arduino/go-paths-helper"
	"github.com/sirupsen/logrus"
	semver "go.bug.st/relaxed-semver"

	"github.com/arduino/arduino-app-cli/internal/micro"
)

func uploadSketchInRam(ctx context.Context,
	w io.Writer,
	srv rpc.ArduinoCoreServiceServer,
	inst *rpc.Instance,
	sketchPath string,
	buildPath string,
) error {
	upload := func() error {
		stream, _ := commands.UploadToServerStreams(ctx, w, w)
		if err := srv.Upload(&rpc.UploadRequest{
			Instance:   inst,
			Fqbn:       "arduino:zephyr:unoq:flash_mode=ram",
			SketchPath: sketchPath,
			ImportDir:  buildPath,
		}, stream); err != nil {
			return err
		}
		return nil
	}
	if err := upload(); err != nil {
		slog.Warn("failed to upload in ram mode, trying to configure the board in ram mode, and retry", slog.String("error", err.Error()))
		if err := configureMicroInRamMode(ctx, w, srv, inst); err != nil {
			return err
		}
	}
	return upload()
}

func uploadSketchWaitForApp(ctx context.Context,
	w io.Writer,
	srv rpc.ArduinoCoreServiceServer,
	inst *rpc.Instance,
	sketchPath string,
	buildPath string,
) error {
	stream, _ := commands.UploadToServerStreams(ctx, w, w)
	if err := srv.Upload(&rpc.UploadRequest{
		Instance:   inst,
		Fqbn:       "arduino:zephyr:unoq",
		SketchPath: sketchPath,
		ImportDir:  buildPath,
	}, stream); err != nil {
		return err
	}

	// After the sketch is uploaded, we signal the microcontroller to start.
	go func() {
		time.Sleep(500 * time.Millisecond) // wait a bit.

		if err := micro.SignalAppStart(); err != nil {
			slog.Warn("failed to signal app start to microcontroller", slog.String("error", err.Error()))
		}
	}()

	return nil
}

// configureMicroInRamMode uploads an empty binary overing any sketch previously uploaded in flash.
// This is required to be able to upload sketches in ram mode after if there is already a sketch in flash.
func configureMicroInRamMode(
	ctx context.Context,
	w io.Writer,
	srv rpc.ArduinoCoreServiceServer,
	inst *rpc.Instance,
) error {
	emptyBinDir := paths.New("/tmp/empty")
	_ = emptyBinDir.MkdirAll()
	defer func() { _ = emptyBinDir.RemoveAll() }()

	zeros, err := os.Open("/dev/zero")
	if err != nil {
		return err
	}
	defer zeros.Close()

	empty, err := emptyBinDir.Join("empty.ino.elf-zsk.bin").Create()
	if err != nil {
		return err
	}
	defer empty.Close()
	if _, err := io.CopyN(empty, zeros, 50); err != nil {
		return err
	}

	stream, _ := commands.UploadToServerStreams(ctx, w, w)
	return srv.Upload(&rpc.UploadRequest{
		Instance:  inst,
		Fqbn:      "arduino:zephyr:unoq:flash_mode=flash",
		ImportDir: emptyBinDir.String(),
	}, stream)
}

func hasWaitForApp(ctx context.Context) bool {
	check, err := func() (bool, error) {
		logrus.SetLevel(logrus.ErrorLevel) // Reduce the log level of arduino-cli
		srv := commands.NewArduinoCoreServer()
		if _, err := srv.SettingsSetValue(ctx, &rpc.SettingsSetValueRequest{
			Key:          "network.connection_timeout",
			EncodedValue: "600s",
			ValueFormat:  "cli",
		}); err != nil {
			return false, err
		}

		var inst *rpc.Instance
		if resp, err := srv.Create(ctx, &rpc.CreateRequest{}); err != nil {
			return false, err
		} else {
			inst = resp.GetInstance()
		}
		defer func() {
			_, _ = srv.Destroy(ctx, &rpc.DestroyRequest{Instance: inst})
		}()

		if err := srv.Init(
			&rpc.InitRequest{Instance: inst},
			commands.InitStreamResponseToCallbackFunction(ctx, func(r *rpc.InitResponse) error {
				slog.Debug("Arduino init instance", slog.String("instance", r.String()))
				return nil
			}),
		); err != nil {
			return false, err
		}

		platforms, err := srv.PlatformSearch(ctx, &rpc.PlatformSearchRequest{
			Instance:          inst,
			ManuallyInstalled: true,
			SearchArgs:        "arduino:zephyr",
		})
		if err != nil {
			return false, err
		}

		for _, p := range platforms.GetSearchOutput() {
			if p.GetMetadata().Id == "arduino:zephyr" {
				v, err := semver.Parse(p.GetInstalledVersion())
				if err != nil {
					return false, fmt.Errorf("failed to parse platform version: %w", err)
				}
				if v.GreaterThan(semver.MustParse("0.53.1")) {
					return true, nil
				}
				return false, nil
			}
		}

		return false, fmt.Errorf("platform arduino:zephyr not installed")
	}()
	if err != nil {
		slog.Warn("failed to check if wait for app upload is supported, use flash to ram mode", "error", err)
		return false
	}
	return check
}
