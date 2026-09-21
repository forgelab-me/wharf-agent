# Changelog

All notable changes to `wharf-agent` are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions match the `vX.Y.Z` git tags that trigger a release build.

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
