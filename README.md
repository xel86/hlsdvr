# hlsdvr

> A lightweight daemon for recording HLS streams as they go live, with no external dependencies.

**hlsdvr** captures live HLS streams from supported platforms like Twitch, designed with a modular architecture for easy addition of new streaming platforms. Perfect for creators archiving their own content or editors who need immediate access to raw footage after a stream ends.

This is a work-in-progress. New features are being added rapidly, and testing has mainly been done with a twitch user_token with turbo provided, for ad-less streams. Twitch streams with ads (not source) are currently written directly as received without any editing or remuxing, which may result in unexpected ad cuts in the output video.

## Features

- **No external dependencies** - Built entirely in Go with zero third-party requirements
- **Cross-platform** - Works anywhere Go works (Linux, macOS, Windows, etc.)
- **Live monitoring** - Automatically detects when streams go live
- **Modular design** - Easy to extend with support for additional platforms
- **Runtime configuration** - Edit settings (adding streamers, etc) without restarting the daemon
- **Archive finished recordings** - Record to one location and move the file to another once completed
- **Control & Inspect with RPC** - Use the controller binary **hlsctl** to monitor and control the daemon.

## Currently Supported Platforms

- Twitch

## Installation

### Prerequisites

- Latest Golang release: [https://go.dev/doc/install](https://go.dev/doc/install)

### Build from Source

```bash
# Clone the repository
git clone https://github.com/xel86/hlsdvr.git

# Build the application
cd hlsdvr
go build ./cmd/hlsdvr
go build ./cmd/hlsctl

# The executable will be available at ./hlsdvr and ./hlsctl
```

## Usage

The project consists of two binaries:
- `hlsdvr` - The main daemon that monitors and records streams
- `hlsctl` - Control utility to interact with the running daemon

### hlsdvr (Daemon)

After setting up your configuration file, simply run the `hlsdvr` binary:

```bash
# Run with default configuration path
./hlsdvr

# Run with custom configuration path & socket path
./hlsdvr -config /path/to/config.json -socket /path/to/unix.sock

# Automatically remux (lossless conversion) recordings from .ts (MPEG transport) to .mkv
./hlsdvr -remux "ts:mkv"
```

### Command-line Arguments

| Flag | Description |
|------|-------------|
| `-config <path>` | Path to a configuration file to use/create |
| `-socket <path>` | Path to create unix socket for RPC |
| `-no-rpc` | Disable the RPC server from running or being used |
| `-no-persist-stats` | Don't restore or save stats file between hlsdvr instances. |
| `-remux <string>` | Remux recordings from one video container to another. (**Requires FFmpeg**). |
| `-debug` | Enable debug log level |
| `-version` | Get the version of the build |

### hlsctl (Control Utility)

The `hlsctl` utility allows you to interact with a running hlsdvr daemon:

```bash
# Check the status of the daemon and the current streams being recorded.
./hlsctl status

# View statistics for the daemon since start time for recordings, streamers, and platforms.
./hlsctl stats -list
```

### Command-line Arguments

| Flag | Description |
|------|-------------|
| `-socket <path>` | Path to unix socket that the running hlsdvr daemon is using for RPC |
| `-version` | Get the version of the build |

### Commands

| Command | Description |
|------|-------------|
| `status` | See daemon status info and the current streams being recorded |
| `stats` | View various stats about the daemon, recordings, streamers, and platforms |

Each subcommand has its own set of options. Use the help flag to see them:

```bash
./hlsctl status -h
./hlsctl stats -h
```

## Configuration

The configuration file uses JSON format and can be edited while hlsdvr is running.
See the Runtime Behavior section for more details.

### Default Configuration Locations

- **Linux**: `~/.config/hlsdvr/config.json`
- **Windows**: `/AppData/Roaming/hlsdvr/config.json`
- **macOS**: `/Library/Application Support/hlsdvr/config.json`

A default configuration will be created automatically if one doesn't exist, you must edit it before using it.

### Configuration Hierarchy

Configuration options can exist at three levels, with deeper levels taking priority:

1. **Global level** - Applied to all platforms and streamers
2. **Platform level** - Applied to all streamers within a platform
3. **Streamer level** - Applied to a specific streamer

Deeper levels inherit from higher levels if they do not set a value.

### Configuration Reference

Check the `configs/` folder in the project repository for various config file examples.

#### Global Options

| Option | Type | Required | Description |
|--------|------|----------|-------------|
| `output_dir_path` | string | Yes | Base directory for all recordings |
| `archive_dir_path` | string | No | Base directory for recordings to be moved to once completed |
| `unix_socket_path` | string | No | Custom path for the unix socket to be created too for RPC |
| `remux` | string* | No | Remux recordings from one video container to another. (**Requires FFmpeg**). |

\* The remux string value must be in the format: "source1,source2:target".
 eg: "ts:mkv" or "ts,mp4:mkv" or "any:mp4", etc.
 the "any" source will remux any recording to the target container specified.

#### Twitch Platform Options

| Option | Type | Required | Description |
|--------|------|----------|-------------|
| `output_dir_path` | string | No | Override the global output path for all Twitch streams |
| `archive_dir_path` | string | No | Override the global archive path for all Twitch streams |
| `client_id` | string | Yes | Twitch Helix API client ID |
| `client_secret` | string | Yes | Twitch Helix API client secret |
| `user_token` | string | No | User token for API requests (enables ad-free recording for Turbo users) |
| `check_live_interval` | integer | No | How often to check if streamers are live (in seconds) |
| `streamers` | array | Yes | List of streamers to monitor and record |

#### Twitch Streamer Options

| Option | Type | Required | Description |
|--------|------|----------|-------------|
| `output_dir_path` | string | No | Override the platform output path for this specific streamer |
| `archive_dir_path` | string | No | Override the platform archive path for this specific streamer |
| `enabled` | boolean | Yes | Whether this streamer should be monitored and recorded |
| `user_login` | string | Yes* | Streamer's login name (e.g., "twitch" in twitch.tv/twitch) |
| `user_id` | string | No | Streamer's user ID (takes precedence over `user_login` if provided) |

\* Required if `user_id` is not provided

## API Credentials

### Twitch

1. **Register an application**: Follow the [Twitch documentation](https://dev.twitch.tv/docs/authentication/register-app/) to get your client ID and client secret.

2. **User token** (optional): For ad-free recording (Turbo users), follow the [Streamlink guide](https://streamlink.github.io/cli/plugins/twitch.html) to obtain your user token.

## Runtime Behavior

- The configuration file can be edited while hlsdvr is running
- Most changes take effect immediately without requiring a restart
- If the updated configuration contains errors, changes will be rejected and previous configuration retained
- Errors will be logged when configuration updates fail

## Contributing

Contributions are welcome! The project is designed to make adding support for additional platforms straightforward.
