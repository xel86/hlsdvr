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

	"github.com/xel86/hlsdvr/internal/hls"
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
	case "stats":
		handleStats(args[1:])
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
	fmt.Println("  stats         Show historical stats for all platforms and streamers")
	fmt.Println("\nCommand options:")
	fmt.Println("  -h, --help    Show help for a specific command")
	fmt.Println("\nExamples:")
	fmt.Println("  hlsctl -socket /tmp/custom.sock status")
	fmt.Println("  hlsctl status -h")
}

func handleStats(args []string) {
	// Create a new FlagSet for the stats command
	var tail int

	statsCmd := flag.NewFlagSet("stats", flag.ExitOnError)
	list := statsCmd.Bool("list", false, "Print a list of all the individual streams recorded.")
	statsHelp := statsCmd.Bool("help", false, "Show help for stats command")
	statsHelpS := statsCmd.Bool("h", false, "Show help for stats command (shorthand)")
	statsCmd.IntVar(&tail,
		"tail",
		0,
		"Only list this amount of streams recorded for each streamer. (0 = all)")

	// Parse the flags for only this command
	statsCmd.Parse(args)

	if *statsHelp || *statsHelpS {
		fmt.Println("Usage: hlsctl stats")
		fmt.Println("\nOptions:")
		statsCmd.PrintDefaults()
		return
	}

	commandMsg := server.RpcRequest{Command: "stats"}

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
			fmt.Printf("Failed to do stats command: %v\n", response.Error)
		}
		fmt.Println("Failed to do stats command")
	}

	if response.Data != nil {
		statsData := server.RpcCmdStatsData{}

		// Unmarshal the generic json.RawMessage into the command specific data struct.
		err := json.Unmarshal(response.Data, &statsData)
		if err != nil {
			fmt.Printf("error unmarshalling stats command response data from hlsdvr RPC server: %v\n", err)
		}

		if len(statsData) == 0 {
			fmt.Println("No stats available for any platforms (are any platforms being monitored?)")
		}

		for platformName, v := range statsData {
			var bytesWrittenPlatform int
			var totalDurationPlatform float64
			var avgBytesPerStreamPlatform int
			var avgBytesPerSecondPlatform int
			var numRecordingsPlatform int
			fmt.Printf("%s:\n", platformName)

			if v.BytesWritten == 0 || v.Recordings == 0 {
				fmt.Println(" No recordings made.")
				continue
			}

			// Group all the digests into digests per streamer (identifer)
			groupedDigests := make(map[string][]hls.RecordingDigest)
			for _, digest := range v.FinishedDigests {
				identifer := digest.Identifier
				groupedDigests[identifer] = append(groupedDigests[identifer], digest)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)

			for identifer, digests := range groupedDigests {
				// Sum values per streamer (identifer)
				var bytesWritten int
				var totalDuration float64
				var avgBytesPerStream int
				var avgBytesPerSecond int
				numRecordings := len(digests)

				for _, digest := range digests {
					bytesWritten += digest.BytesWritten
					totalDuration += digest.RecordingDuration
				}
				avgBytesPerSecond = (bytesWritten / int(totalDuration))
				avgBytesPerStream = (bytesWritten / numRecordings)

				fmt.Fprintf(w, "  %s:\t(total: %s / recordings: %d) ~[%s/stream | %s/sec]\n",
					identifer,
					util.HumanReadableBytes(bytesWritten),
					numRecordings,
					util.HumanReadableBytes(avgBytesPerStream),
					util.HumanReadableBytes(avgBytesPerSecond))

				if *list {
					dLen := len(digests)

					// Only print the last N tail elements from digests array
					// If tail is 0 print the whole array.
					startIndex := 0
					if (tail != 0) && (tail < dLen) {
						startIndex = dLen - (tail)
					}

					for i := startIndex; i < dLen; i++ {
						fmt.Fprintf(w, "\t%d: [%s]:\t%s / %s (%dx%d @ %.2f)\n",
							i+1,
							path.Base(digests[i].OutputPath),
							util.HumanReadableBytes(digests[i].BytesWritten),
							util.HumanReadableSeconds(int(digests[i].RecordingDuration)),
							digests[i].Width, digests[i].Height, digests[i].FrameRate)
					}
					fmt.Fprintf(w, "\t\t\n")
				}

				// Update platform totals
				bytesWrittenPlatform += bytesWritten
				totalDurationPlatform += totalDuration
				numRecordingsPlatform += numRecordings
			}

			avgBytesPerSecondPlatform = (bytesWrittenPlatform / int(totalDurationPlatform))
			avgBytesPerStreamPlatform = (bytesWrittenPlatform / numRecordingsPlatform)
			fmt.Fprintf(w, "%s:\t(total: %s / recordings: %d) ~[%s/stream | %s/sec]\n\n",
				platformName,
				util.HumanReadableBytes(bytesWrittenPlatform),
				numRecordingsPlatform,
				util.HumanReadableBytes(avgBytesPerStreamPlatform),
				util.HumanReadableBytes(avgBytesPerSecondPlatform))
			w.Flush()
		} // end platform stats extraction/print loop
	} else {
		fmt.Println("No stats data received.")
	}
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

		for platformName, v := range statusData {
			totalBytesPerSecond := 0
			if *verbose {
				for _, stream := range v { // we're iterating this twice but its cleaner than the alternative
					totalBytesPerSecond += stream.Digest.BytesPerSecond
				}

				fmt.Printf("%s (%s/s):\n\n",
					platformName,
					util.HumanReadableBytes(totalBytesPerSecond))
			} else {
				fmt.Printf("%s:\n", platformName)
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

	var response server.RpcResponse
	decoder := json.NewDecoder(conn)
	err = decoder.Decode(&response)
	if err != nil {
		return nil, fmt.Errorf("error decoding rpc response: %v", err)
	}

	return &response, nil
}
