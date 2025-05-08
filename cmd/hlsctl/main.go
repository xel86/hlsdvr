package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"path"
	"runtime/debug"
	"text/tabwriter"
	"time"

	"github.com/xel86/hlsdvr/internal/disk"
	"github.com/xel86/hlsdvr/internal/platform"
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

// TODO: this is gonna estimate time till drive is full per platform...
// which makes no sense. it should combine all the platforms.
func printDriveTimeStats(targets []platform.StreamerTargets, stats platform.PlatformStats) {
	outputDrives := make(map[string][]platform.StreamerTargets)
	archiveDrives := make(map[string][]platform.StreamerTargets)
	for _, t := range targets {
		// If a streamer has an archive directory path,
		// only print estimated time for it, not the output path as well.
		if t.ArchiveDirPath != nil {
			aDevice, err := disk.GetDriveIdentifier(*t.ArchiveDirPath)
			if err != nil {
				fmt.Printf("Error getting drive information for an archive path: %v\n", err)
				continue
			}

			archiveDrives[aDevice] = append(archiveDrives[aDevice], t)
			continue
		}

		oDevice, err := disk.GetDriveIdentifier(t.OutputDirPath)
		if err != nil {
			fmt.Printf("Error getting drive information for an output path: %v\n", err)
			continue
		}
		outputDrives[oDevice] = append(outputDrives[oDevice], t)
	}

	if len(outputDrives) > 0 {
		fmt.Printf("Output drives:\n")
		printEstimatedTimePerDrive(outputDrives, stats, true)
		fmt.Printf("\n")
	}

	if len(archiveDrives) > 0 {
		fmt.Printf("Archive drives:\n")
		printEstimatedTimePerDrive(archiveDrives, stats, false)
		fmt.Printf("\n")
	}
}

func printEstimatedTimePerDrive(
	drives map[string][]platform.StreamerTargets,
	stats platform.PlatformStats,
	outputDrive bool) {

	for driveId, pathsOnDrive := range drives {
		var totalBytesWritten int
		for _, target := range pathsOnDrive {
			sStats, exists := stats.StreamerStats[target.Username]
			if exists {
				totalBytesWritten += sStats.BytesWritten
			}
		}

		if totalBytesWritten == 0 {
			continue
		}

		avgBytesPerSecond := totalBytesWritten / int(time.Since(stats.StartTime).Seconds())

		var basePath string
		if outputDrive {
			basePath = pathsOnDrive[0].OutputDirPath
		} else {
			basePath = *pathsOnDrive[0].ArchiveDirPath
		}

		du, err := disk.GetDriveUtilization(basePath)
		if err != nil {
			fmt.Printf("Error getting disk utilization information for %s: %v", basePath, err)
			continue
		}

		if du.TotalBytes == 0 {
			continue
		}

		// Calculate seconds until full
		secondsUntilFull := du.FreeBytes / uint64(avgBytesPerSecond)
		duration := time.Duration(secondsUntilFull) * time.Second

		days := int(math.Floor(duration.Hours() / 24))
		hours := int(math.Floor(duration.Hours())) % 24
		minutes := int(math.Floor(duration.Minutes())) % 60

		rootDirPath, err := disk.GetMountPointForPath(basePath)
		if err != nil {
			fmt.Printf("Error getting mount point for path: %v", err)
			rootDirPath = driveId
		}

		if rootDirPath == "" {
			rootDirPath = driveId
		}

		fmt.Printf("%s: Estimated time till disk full: %dd %dh %dm (%s free)\n",
			rootDirPath,
			days, hours, minutes,
			util.HumanReadableBytes(int(du.FreeBytes)))
	}
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

	commandMsg := server.RpcRequest{Command: "stats", Value: map[string]any{
		"include_past_digests": *list,
	}}

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
		return
	}

	if response.Data != nil {
		statsData := server.RpcCmdStatsData{}

		// Unmarshal the generic json.RawMessage into the command specific data struct.
		err := json.Unmarshal(response.Data, &statsData)
		if err != nil {
			fmt.Printf("error unmarshalling stats command response data from hlsdvr RPC server: %v\n", err)
			return
		}

		if len(statsData) == 0 {
			fmt.Println("No stats available for any platforms (are any platforms being monitored?)")
			return
		}

		for platformName, v := range statsData {
			fmt.Printf("%s:\n", platformName)

			if v.Stats.BytesWritten == 0 || v.Stats.Recordings == 0 {
				fmt.Println(" No recordings made.")
				continue
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)

			for identifer, s := range v.Stats.StreamerStats {
				fmt.Fprintf(w, "  %s:\t(total: %s / recordings: %d) ~[%s/stream | %s/sec live]\n",
					identifer,
					util.HumanReadableBytes(s.BytesWritten),
					s.Recordings,
					util.HumanReadableBytes(s.AvgBytesPerStream),
					util.HumanReadableBytes(s.AvgBytesPerSecondLive))

				if *list {
					dLen := len(s.FinishedDigests)

					// Only print the last N tail elements from digests array
					// If tail is 0 print the whole array.
					startIndex := 0
					if (tail != 0) && (tail < dLen) {
						startIndex = dLen - (tail)
					}

					for i := startIndex; i < dLen; i++ {
						fmt.Fprintf(w, "\t%d: [%s]:\t%s / %s (%dx%d @ %.2f)\n",
							i+1,
							path.Base(s.FinishedDigests[i].OutputPath),
							util.HumanReadableBytes(s.FinishedDigests[i].BytesWritten),
							util.HumanReadableSeconds(int(s.FinishedDigests[i].RecordingDuration)),
							s.FinishedDigests[i].Width, s.FinishedDigests[i].Height,
							s.FinishedDigests[i].FrameRate)
					}
					fmt.Fprintf(w, "\t\t\n")
				}
			}

			fmt.Fprintf(w, "%s:\t(total: %s / recordings: %d) ~[%s/stream | %s/sec overall]\n\n",
				platformName,
				util.HumanReadableBytes(v.Stats.BytesWritten),
				v.Stats.Recordings,
				util.HumanReadableBytes(v.Stats.AvgBytesPerStream),
				util.HumanReadableBytes(v.Stats.AvgBytesPerSecond))

			w.Flush()

			printDriveTimeStats(v.StreamerTargets, v.Stats)
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
			fmt.Printf("Failed to do status command: %v\n", response.Error)
		}
		fmt.Println("Failed to do status command")
		return
	}

	if response.Data != nil {
		statusData := server.RpcCmdStatsData{}

		// Unmarshal the generic json.RawMessage into the command specific data struct.
		err := json.Unmarshal(response.Data, &statusData)
		if err != nil {
			fmt.Printf("error unmarshalling status command response data from hlsdvr RPC server: %v\n", err)
			return
		}

		if len(statusData) == 0 {
			fmt.Println("No platforms being monitored.")
			return
		}

		for platformName, v := range statusData {
			if len(v.LiveDigests) == 0 {
				fmt.Printf("%s: no streamers are live/being recorded.\n", platformName)
				return
			}

			totalBytesPerSecond := 0
			if *verbose {
				for _, digest := range v.LiveDigests { // we're iterating this twice but its cleaner than the alternative
					totalBytesPerSecond += digest.BytesPerSecond
				}

				fmt.Printf("%s (%s/s):\n\n",
					platformName,
					util.HumanReadableBytes(totalBytesPerSecond))
			} else {
				fmt.Printf("%s:\n", platformName)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
			for _, digest := range v.LiveDigests {
				fmt.Fprintf(w, "  %s:\t[%s]:\t%s / %s (%dx%d @ %.2f)\n",
					digest.Identifier,
					path.Base(digest.OutputPath),
					util.HumanReadableBytes(digest.BytesWritten),
					util.HumanReadableSeconds(int(digest.RecordingDuration)),
					digest.Width, digest.Height, digest.FrameRate)
				if *verbose {
					fmt.Fprintf(w, "  \t\t%s/s (%s/s avg.)\n",
						util.HumanReadableBytes(digest.BytesPerSecond),
						util.HumanReadableBytes(digest.AvgBytesPerSecond))
					fmt.Fprintf(w, "\t\t\n")
				}
			}
			w.Flush()

			fmt.Println()
			printDriveTimeStats(v.StreamerTargets, v.Stats)
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
