
# hlsdvr (WIP)
Daemon for recording HLS streams as they go live, such as twitch. Requires no third party dependencies or external programs to run. Written in a modular fashion that will enable adding additional platform support quickly.

Perfect for creators who wish to archive their own streams, or official editors who want to work on the raw video files immediately after a stream has ended, so they can upload content faster than ever!

Currently Supported Platforms:
- Twitch

## Usage

After editing the default configuration file generated or that you grabbed from the ```examples/``` folder, simply run the ```hlsdvr``` binary.

### Flags/Arguments<br>
```-config <path-to-config>``` path to a configuration file to use instead of the default path(s)<br>
```-debug``` flag that enables debug log level<br>

### Configuration

The config file (config.json) will by default be found, and created if necessary, in these default directories:<br>
```~/.config/hlsdvr/config.json``` on Linux<br>
```/AppData/Roaming/hlsdvr/config.json``` on Windows<br>
```/Library/Application Support/hlsdvr/config.json``` on macOS<br>

A default configuration will be made in the default directory if there isn't one present, you can then edit it to your liking and rerun hlsdvr.

A common configuration pattern will be having the same option at 3 "levels" with the deeper the option being the higher priority it has. Some options will apply to their lower level counterpart if no override is present. The levels usually being: top-level, platform-level, and streamer-level.

The configuration is in the json format, and can be edited during runtime. Meaning you can add or remove streamers to monitor and record without restarting the daemon, or change any other config option. Simply make your edits to the config file in use and save.

**Currently, if there are errors in the new config after an edit, the default behavior is to simply ignore the changes and continue using the previous configuration. Logs will show errors reflecting this.**

### Twitch Platform Config
  ```"client_id"``` & ```"client_secret"```: Helix API credentials you must provide, [register an app](https://dev.twitch.tv/docs/authentication/register-app/) on twitch to get them.<br>
  ```"user_token"``` User token to be used for queries to get HLS links from twitch, this will also enable turbo users to get recordings with no ad interruptions. <br>
 ```"check_live_interval"``` How often we check if any streamers have gone live. (in seconds)<br>
 ```"output_dir_path"``` platform-level override, will record all twitch streams to this directory with username subfolders.<br>
 ```"streamers"``` List of all the streamers to monitor and record.<br>
  - ```"enabled"``` whether this streamer should be considered when loading config (true/false)
  - ```"user_login"``` the streamer's login (the name of the channel, such as "twitch" in twitch.tv/twitch)
  - ```"user_id"``` the streamer's user id (will be used instead of ```user_login``` if provided.
  - ```"output_dir_path"``` streamer-level override, will record streams for this user to this directory

## Installation
Install golang https://go.dev/doc/install<br>

From Source:
- ```git clone https://github.com/xel86/hlsdvr.git``` then ```cd hlsdvr && go build ./cmd/hlsdvr```
- After building, the binary ```hlsdvr``` will be located in the same directory, which can then be moved elsewhere.
