package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path"
	"runtime/debug"
	"text/tabwriter"
	"time"

	"github.com/xel86/hlsdvr/internal/server"
	"github.com/xel86/hlsdvr/internal/util"
)

// Global flags
var (
	socketPath     string
	defaultTimeout = (5 * time.Second)
)

func main() {
	var showHelp bool
	var showVersion bool

	// top-level flags
	flag.StringVar(
		&socketPath,
		"socket",
		util.GetDefaultSocketPath(server.SocketFileName),
		"Path to the unix socket hlsdvr is currently using")
	flag.BoolVar(&showHelp, "help", false, "Show help message")
	flag.BoolVar(&showHelp, "h", false, "Show help message (shorthand)")
	flag.BoolVar(&showVersion, "version", false, "Show build version")
	flag.BoolVar(&showVersion, "v", false, "Show build version (shorthand)")

	// Parse only the global flags without consuming the command
	flag.CommandLine.Parse(os.Args[1:])

	if showHelp {
		printHelp()
		return
	}

	if showVersion {
		build, ok := debug.ReadBuildInfo()
		if ok {
			fmt.Printf("hlsctl: %v\n", build.Main.Version)
		} else {
			fmt.Printf("hlsctl: v1\n")
		}
		return
	}

	args := flag.CommandLine.Args()

	if len(args) < 1 {
		printHelp()
		os.Exit(1)
	}

	// Extract the command (first remaining argument)
	command := args[0]

	// Handle different commands
	switch command {
	case "status":
		handleStatus(args[1:])
	case "help":
		printHelp()
	default:
		fmt.Printf("Unknown command: %s\n", command)
		printHelp()
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println("Usage: hlsctl [global options] <command> [subcommand] [command options]")
	fmt.Println("\nGlobal options:")
	flag.PrintDefaults()
	fmt.Println("\nCommands:")
	fmt.Println("  status        Show currently recording streams and stats")
	fmt.Println("\nCommand options:")
	fmt.Println("  -verbose      Show detailed information")
	fmt.Println("  -h, --help    Show help for a specific command")
	fmt.Println("\nExamples:")
	fmt.Println("  hlsctl -socket /tmp/custom.sock status")
	fmt.Println("  hlsctl status -verbose")
}

func handleStatus(args []string) {
	// Create a new FlagSet for the status command
	statusCmd := flag.NewFlagSet("status", flag.ExitOnError)
	verbose := statusCmd.Bool("verbose", false, "Print all information gathered from status.")
	statusHelp := statusCmd.Bool("help", false, "Show help for status command")
	statusHelpS := statusCmd.Bool("h", false, "Show help for status command (shorthand)")

	// Parse the flags for only this command
	statusCmd.Parse(args)

	if *statusHelp || *statusHelpS {
		fmt.Println("Usage: hlsctl status [options]")
		fmt.Println("\nOptions:")
		statusCmd.PrintDefaults()
		return
	}

	commandMsg := server.RpcRequest{Command: "status"}

	response, err := exchangeRpc(socketPath, defaultTimeout, commandMsg)
	if err != nil {
		fmt.Printf("Error communicating with hlsdvr RPC server: %v\n", err)
		os.Exit(1)
	}

	if response == nil {
		fmt.Println("No response received from hlsdvr RPC server")
		return
	}

	if !response.Success {
		if response.Error != nil {
			fmt.Printf("Failed to do status command: %v\n", response.Error)
		}
		fmt.Println("Failed to do status command")
	}

	if response.Data != nil {
		statusData := server.RpcCmdStatusData{}

		// Unmarshal the generic json.RawMessage into the command specific data struct.
		err := json.Unmarshal(response.Data, &statusData)
		if err != nil {
			fmt.Printf("error unmarshalling status command response data from hlsdvr RPC server: %v\n", err)
		}

		if len(statusData) == 0 {
			fmt.Println("No streams are live/being recorded.")
		}

		for k, v := range statusData {
			totalBytesPerSecond := 0
			if *verbose {
				for _, stream := range v { // we're iterating this twice but its cleaner than the alternative
					totalBytesPerSecond += stream.Digest.BytesPerSecond
				}

				fmt.Printf("%s (%s/s):\n\n",
					k,
					util.HumanReadableBytes(totalBytesPerSecond))
			} else {
				fmt.Printf("%s:\n", k)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
			for _, stream := range v {
				fmt.Fprintf(w, "  %s:\t[%s]:\t%s / %s (%dx%d @ %.2f)\n",
					stream.Identifier,
					path.Base(stream.Digest.OutputPath),
					util.HumanReadableBytes(stream.Digest.BytesWritten),
					util.HumanReadableSeconds(int(stream.Digest.RecordingDuration)),
					stream.Digest.Width, stream.Digest.Height, stream.Digest.FrameRate)
				if *verbose {
					fmt.Fprintf(w, "  \t\t%s/s (%s/s avg.)\n",
						util.HumanReadableBytes(stream.Digest.BytesPerSecond),
						util.HumanReadableBytes(stream.Digest.AvgBytesPerSecond))
					fmt.Fprintf(w, "\t\t\n")
				}
			}
			w.Flush()
		}
	} else {
		fmt.Println("No status data received.")
	}
}

// Send an RPC request to the specified unix socket, then retrieve and return the response
func exchangeRpc(socketPath string, timeout time.Duration,
	req server.RpcRequest) (*server.RpcResponse, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("error connecting to hlsdvr unix socket: %v", err)
	}
	defer conn.Close()

	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("error marshaling RPC request to json: %v", err)
	}

	conn.SetDeadline(time.Now().Add(timeout))

	_, err = conn.Write(jsonData)
	if err != nil {
		return nil, fmt.Errorf("error writing json rpc request to socket: %v", err)
	}

	buffer := make([]byte, 8192)

	n, err := conn.Read(buffer)
	if err != nil {
		return nil, fmt.Errorf("error reading rpc response from socket: %v", err)
	}

	var response server.RpcResponse
	err = json.Unmarshal(buffer[:n], &response)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling rpc response: %v", err)
	}

	return &response, nil
}
