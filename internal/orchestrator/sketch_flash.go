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

	"github.com/arduino/arduino-app-cli/internal/platform"
)

func uploadSketchInRam(ctx context.Context,
	w io.Writer,
	srv rpc.ArduinoCoreServiceServer,
	inst *rpc.Instance,
	platform platform.Platform,
	sketchPath string,
	buildPath string,
) error {
	upload := func() error {
		stream, _ := commands.UploadToServerStreams(ctx, w, w)
		if err := srv.Upload(&rpc.UploadRequest{
			Instance:   inst,
			Fqbn:       platform.FQBN + ":flash_mode=ram",
			SketchPath: sketchPath,
			ImportDir:  buildPath,
		}, stream); err != nil {
			return err
		}
		return nil
	}
	if err := upload(); err != nil {
		slog.Warn("failed to upload in ram mode, trying to configure the board in ram mode, and retry", slog.String("error", err.Error()))
		if err := configureMicroInRamMode(ctx, w, srv, inst, platform); err != nil {
			return err
		}
	}
	return upload()
}

func uploadSketchWaitForApp(ctx context.Context,
	w io.Writer,
	srv rpc.ArduinoCoreServiceServer,
	inst *rpc.Instance,
	platform platform.Platform,
	sketchPath string,
	buildPath string,
) error {
	stream, _ := commands.UploadToServerStreams(ctx, w, w)
	if err := srv.Upload(&rpc.UploadRequest{
		Instance:   inst,
		Fqbn:       platform.FQBN,
		SketchPath: sketchPath,
		ImportDir:  buildPath,
	}, stream); err != nil {
		return err
	}

	// After the sketch is uploaded, we signal the microcontroller to start.
	go func() {
		time.Sleep(500 * time.Millisecond) // wait a bit.

		if err := platform.GetMicro().SignalAppStart(); err != nil {
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
	platform platform.Platform,
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
		Fqbn:      platform.FQBN + ":flash_mode=flash",
		ImportDir: emptyBinDir.String(),
	}, stream)
}

func hasWaitForApp(ctx context.Context, platform platform.Platform) bool {
	const waitForLinuxMenu = "wait_linux_boot"
	const waitForAppValue = "app"

	if check, err := func() (bool, error) {
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

		info, err := srv.BoardDetails(ctx, &rpc.BoardDetailsRequest{
			Instance: inst,
			Fqbn:     platform.FQBN,
		})
		if err != nil {
			return false, err
		}

		for _, config := range info.GetConfigOptions() {
			// config option for zephyr platform defined here https://github.com/arduino/ArduinoCore-zephyr/blob/main/boards.txt#L641-L647
			if config.GetOption() == "wait_linux_boot" {
				for _, value := range config.GetValues() {
					slog.Debug("found config value for wait_linux_boot", "value", value.GetValue())
					if value.GetValue() == "app" {
						return true, nil
					}
				}
			}
		}

		return false, fmt.Errorf("platform %q not installed", platform.PlatformID)
	}(); err != nil {
		slog.Warn("failed to check if wait for app upload is supported, use flash to ram mode", "error", err)
		return false
	} else {
		return check
	}
}
