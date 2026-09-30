# Changelog

All notable changes to `wharf-agent` are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions match the `vX.Y.Z` git tags that trigger a release build.

## [Unreleased]

### Added
- The state the agent reports now carries, for vulnerability scanning on the controller: each image's registry digest (`docker images --digests`; empty for an image built on the host), the id of the image each container runs (one grouped `docker inspect`, since `docker ps` does not expose it), and the host's CPU architecture. An older controller ignores the new fields.

## [0.5.0] - 2026-09-29

### Added
- Git stacks: if `secrets.refs.yaml` sits next to the compose file, the agent sends it (and `secrets.enc.yaml`, if any) to the controller and deploys with the environment it returns, instead of decrypting `secrets.enc.yaml` wholesale. Without that file nothing changes. In this mode both files must be regular files: a symlink is refused rather than followed out of the clone.

## [0.4.0] - 2026-09-21

### Added
- A `remove` tunnel command (`docker rm`), for the controller's new "Remove" action on a stopped container it didn't deploy itself.

## [0.3.0] - 2026-09-19

### Changed
- `docker logs --tail` is now controller-configurable per request (the controller's new full-page logs view can ask for anywhere from 100 lines to all of them) instead of a hardcoded 200.

## [0.2.0] - 2026-09-18

### Added
- The agent now reports its own version on every state push, so the controller can flag an outdated agent without the agent needing to know what "latest" means itself.

## [0.1.0] - 2026-09-16

Initial public release.
