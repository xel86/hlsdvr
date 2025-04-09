package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/xel86/hlsdvr/hls"
	"github.com/xel86/hlsdvr/platform"
)

// Json request that will be sent to this server from clients.
// Command is the name of the platform.Command to be performed.
// Value is the necessary or optional information needed to perform said command.
// No value may be required for certain commands, such as "status".
type IpcRequest struct {
	Command string         `json:"command"`
	Value   map[string]any `json:"value,omitempty"`
}

// The json response we send back to the requesting client.
// Success is true or false based on if we had an error
// If Success is false, there still may be a partial response to be used in Data
// An error message is populated in Error if Success if false.
// Data will be filled with various different structs based on the command requested
// such as IpcCmdStatusData
type IpcResponse struct {
	Success bool            `json:"success"`
	Error   *string         `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// "twitch" -> [stream1, stream2, stream3], "youtube" -> [stream1]
type IpcCmdStatusData map[string][]IpcStreamInfoResponse

type IpcStreamInfoResponse struct {
	Identifier string
	ViewCount  int
	Title      string
	Category   string
	Digest     hls.RecordingDigest
}

func doIpcCommandStatus(pcs *platform.CommandSender) (IpcCmdStatusData, error) {
	numPlatforms := pcs.GetNumPlatforms()
	ch := make(chan any, numPlatforms)

	pcs.Broadcast(platform.CommandMsg{Type: platform.CmdStatus, Value: nil, ReturnChan: ch})
	timeout := time.After(5 * time.Second)

	data := IpcCmdStatusData{}
	for range numPlatforms {
		select {
		case status := <-ch:
			v, ok := status.(platform.CmdStatusReturn)
			if !ok {
				slog.Error(fmt.Sprintf(
					"(server) received incorrect response from platform for status command: %v", status))
			}

			for _, digest := range v.Digests {
				data[v.PlatformName] = append(data[v.PlatformName],
					IpcStreamInfoResponse{
						Identifier: digest.Identifier,
						Digest:     digest,
					})
			}
		case <-timeout:
			return data, fmt.Errorf("timed out waiting for platform(s) to return status information")
		}
	}

	return data, nil
}

func handleIpcClient(conn net.Conn, pcs *platform.CommandSender) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		req := IpcRequest{}
		resp := IpcResponse{Success: true}
		var respData any

		if err := decoder.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Warn(fmt.Sprintf("(server) couldn't decode client's sent message: %v", err))
			}
			return
		}

		slog.Debug(fmt.Sprintf("(server) received ipc server request: %v", req))
		switch req.Command {
		case "status":
			{
				data, err := doIpcCommandStatus(pcs)
				if err != nil {
					errStr := err.Error()
					slog.Warn(fmt.Sprintf("(server) %s", errStr))
					resp.Success = false
					resp.Error = &errStr
				}
				respData = data
			}
		default:
			slog.Warn(fmt.Sprintf("(server) received unknown ipc command: %s", req.Command))
			return
		}

		rawData, err := json.Marshal(respData)
		if err != nil {
			slog.Error(fmt.Sprintf("(server) failed to marshal command response data: %v", err))
			return
		}

		resp.Data = rawData

		if err := encoder.Encode(resp); err != nil {
			slog.Error(fmt.Sprintf("(server) failed to respond to ipc client: %v", err))
			return
		}
	}
}

func IpcServer(ctx context.Context, pcs *platform.CommandSender, socketPath string) error {
	if _, err := os.Stat(socketPath); err == nil {
		if err := os.Remove(socketPath); err != nil {
			return fmt.Errorf("Failed to remove old existing socket file: %v", err)
		}
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("Failed to create and listen to unix socket: %v", err)
	}
	defer listener.Close()

	if err := os.Chmod(socketPath, 0666); err != nil {
		return fmt.Errorf("Failed to set socket permissions 0666: %v", err)
	}

	slog.Info(fmt.Sprintf("Started IPC server, created and listening on unix socket %s", socketPath))

	// This function will wait for any signals to shutdown and trigger the listener
	// to close so it doesn't block forever.
	go func() {
		<-ctx.Done()
		slog.Info("(server) got shutdown signal, shutting down.")
		listener.Close()
		return
	}()

	var wg sync.WaitGroup
	var listenerError error
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			} else {
				listenerError = fmt.Errorf("(server) error trying to accept client(s): %v", err)
				break
			}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			handleIpcClient(conn, pcs)
		}()
	}
	wg.Wait()

	return listenerError
}
