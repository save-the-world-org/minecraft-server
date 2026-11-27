# minecraft-server

A self-hosted Minecraft Java server that runs on [Infrared](https://darkshift.io), from Darkshift. One Go
program: it fetches a pinned Java runtime and [Paper](https://papermc.io) onto the environment's volume, runs
the server, and serves a status page on the environment's HTTPS address.

This is a first spike. It proves the server runs on Infrared's hosted plane and that the game's traffic
passes its edge inside a websocket. It is not yet somewhere to play.

## What works

- **The runtime and the server are pinned.** Temurin's Java 25 runtime and Paper 26.3 are downloaded once,
  each checked against a SHA-256 in `runtime.go`, and kept on the volume. Nothing runs that does not match.
- **A status page** at `/`, and the same as JSON at `/status.json`: what the server is doing, how long the
  download and the start took, and where the world is kept.
- **The game port over a websocket** at `/ws`: each binary frame carries the bytes a Minecraft client would
  send over TCP. `tools/mcping` asks a server for its status that way.
- **A clean stop.** On SIGTERM it tells Paper to stop, which saves the world, and waits up to 25 seconds.

## What does not, yet

- **Real Minecraft clients cannot join.** They speak TCP, and an Infrared environment's address is HTTPS.
  TCP and UDP addresses for an environment are planned in Infrared.
- **No backups.** Snapshots of the world to a bucket, and a restore on an empty volume, come next.
- **No map and no play in the browser.**

## Run it

On Infrared, give the environment a volume at `/data`, 3 GiB of memory, and the variable `EULA=true`.

Here:

```sh
make run     # the world goes under ./data
make ping    # asks the server for its status through the websocket
make verify  # vet and tests
```

| Variable | Default | What it is |
|---|---|---|
| `EULA` | none | `true` accepts [Minecraft's End User License Agreement](https://aka.ms/MinecraftEULA). The server does not start without it. |
| `DATA_DIR` | `/data` | Where the runtime, the server and the world are kept. |
| `PORT` | `8080` | The status page and the websocket. |
| `MEMORY_LIMIT` | none | The container's memory limit in bytes. The Java heap is 70 percent of it, or 1024 MB when it is not set. |

## Not affiliated

Minecraft is a trademark of Mojang Synergies AB. This project is not affiliated with or endorsed by Mojang
or Microsoft, and ships none of their software: the server is downloaded from Paper when it first starts.
