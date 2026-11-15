# Changelog

## [0.2.0](https://github.com/LaneBirmingham/taillan/compare/v0.1.0...v0.2.0) (2026-11-15)


### Features

* **cli:** manage running sessions from any terminal with status, invite, lock, unlock, kick, stop, name and retire ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))
* **cli:** run host and join headless on servers, over SSH and under systemd ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))
* **cli:** set every host, join and ui flag from TAILLAN_* variables ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))
* **docker:** publish a container image to ghcr.io for linux/amd64 and linux/arm64 ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))
* **server:** add a server guide for Docker, Kubernetes and systemd, with ready-made examples ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))


### Bug Fixes

* detect running game servers and ports on Windows and macOS ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))
* **helpers:** only point friends at a Minecraft LAN world that is really running, so forged announcements can't redirect them ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))
* **session:** cap control messages so a peer can't run TailLAN out of memory ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))
* **session:** make Remove lock the room so a removed friend can't rejoin with a new key ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))
* **ui:** start only one session at a time, and let Stop work while starting ([eb7d94e](https://github.com/LaneBirmingham/taillan/commit/eb7d94e1674cea6bddd0550620e1a4ecd514e174))
