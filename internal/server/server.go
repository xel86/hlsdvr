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

	"github.com/xel86/hlsdvr/internal/platform"
)

const (
	SocketFileName = "hlsdvr.sock"
)

// Json request that will be sent to this server from clients.
// Command is the name of the platform.Command to be performed.
// Value is the necessary or optional information needed to perform said command.
// No value may be required for certain commands, such as "status".
type RpcRequest struct {
	Command string         `json:"command"`
	Value   map[string]any `json:"value,omitempty"`
}

// The json response we send back to the requesting client.
// Success is true or false based on if we had an error
// If Success is false, there still may be a partial response to be used in Data
// An error message is populated in Error if Success if false.
// Data will be filled with various different structs based on the command requested
// such as RpcCmdStatusData
type RpcResponse struct {
	Success bool            `json:"success"`
	Error   *string         `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type RpcCmdStatsParams struct {
	IncludePastDigests bool `json:"include_past_digests"`
}

// "twitch" -> data, "youtube" -> data
type RpcCmdStatsData map[string]platform.CmdStatsReturn

func doRpcCommandStats(pcs *platform.CommandSender, cmdValue map[string]any) (RpcCmdStatsData, error) {
	numPlatforms := pcs.GetNumPlatforms()
	ch := make(chan any, numPlatforms)
	data := RpcCmdStatsData{}

	// Default params
	params := platform.CmdStatsParams{
		IncludePastDigests: false,
	}

	if cmdValue != nil {
		paramsBytes, err := json.Marshal(cmdValue)
		if err != nil {
			return data, fmt.Errorf("(server) couldn't marshal client's sent message params: %v", err)
		}

		var decodedParams RpcCmdStatsParams
		if err := json.Unmarshal(paramsBytes, &decodedParams); err != nil {
			return data, fmt.Errorf("(server) invalid paramaters sent to stats command: %v", err)
		}
		params.IncludePastDigests = decodedParams.IncludePastDigests
	}

	pcs.Broadcast(platform.CommandMsg{Type: platform.CmdStats, Value: params, ReturnChan: ch})
	timeout := time.After(5 * time.Second)

	for range numPlatforms {
		select {
		case status := <-ch:
			v, ok := status.(platform.CmdStatsReturn)
			if !ok {
				slog.Error(fmt.Sprintf(
					"(server) received incorrect response from platform for stats command: %v", status))
			}

			data[v.PlatformName] = v
		case <-timeout:
			return data, fmt.Errorf("timed out waiting for platform(s) to return stats information")
		}
	}

	return data, nil
}

func handleRpcClient(conn net.Conn, pcs *platform.CommandSender) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		req := RpcRequest{}
		resp := RpcResponse{Success: true}
		var respData any

		if err := decoder.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Warn(fmt.Sprintf("(server) couldn't decode client's sent message: %v", err))
			}
			return
		}

		slog.Debug(fmt.Sprintf("(server) received RPC server request: %v", req))
		switch req.Command {
		case "stats":
			{
				data, err := doRpcCommandStats(pcs, req.Value)
				if err != nil {
					errStr := err.Error()
					slog.Warn(fmt.Sprintf("(server) %s", errStr))
					resp.Success = false
					resp.Error = &errStr
				}
				respData = data
			}
		default:
			slog.Warn(fmt.Sprintf("(server) received unknown RPC command: %s", req.Command))
			return
		}

		rawData, err := json.Marshal(respData)
		if err != nil {
			slog.Error(fmt.Sprintf("(server) failed to marshal command response data: %v", err))
			return
		}

		resp.Data = rawData

		if err := encoder.Encode(resp); err != nil {
			slog.Error(fmt.Sprintf("(server) failed to respond to RPC client: %v", err))
			return
		}
	}
}

func RpcServer(ctx context.Context, pcs *platform.CommandSender,
	socketPath string, deleteOldSocket bool) error {
	if _, err := os.Stat(socketPath); err == nil {
		if deleteOldSocket {
			if err := os.Remove(socketPath); err != nil {
				return fmt.Errorf("Failed to remove old existing socket file: %v", err)
			}
		} else {
			return fmt.Errorf("socket %s already exists. "+
				"This probably means another instance of hlsdvr is running. "+
				"If you are sure this is not the case, please remove the file and restart the daemon"+
				"or provide a custom -socket flag for a second hlsdvr instance.", socketPath)
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

	slog.Info(fmt.Sprintf("Started RPC server, created and listening on unix socket %s", socketPath))

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
			handleRpcClient(conn, pcs)
		}()
	}
	wg.Wait()
	slog.Info("(server) RPC server shutdown.")
	return listenerError
}
