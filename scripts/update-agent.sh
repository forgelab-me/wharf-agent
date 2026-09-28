#!/usr/bin/env bash
# update-agent.sh -- update a wharf-agent container to the latest image on
# its current tag, recreating it with the exact same runtime config it
# already has (mounts, command args, restart policy, network mode) --
# nothing is touched if the digest hasn't moved.
#
# Not part of Wharf itself: a manual stand-in for the day agents get an
# in-app update button. Run this ON the host the agent container lives
# on (needs docker.sock access there, same as the agent itself).
#
# Usage:
#   ./update-agent.sh [container-name]
#   ./update-agent.sh -y [container-name]   # skip the confirmation prompt
#
# container-name defaults to "wharf-agent".
#
# Cron (one entry per host, since this needs that host's own docker.sock;
# -y is required here, an interactive prompt would just hang forever):
#   0 4 * * * /path/to/update-agent.sh -y wharf-agent >> /var/log/wharf-agent-update.log 2>&1

set -euo pipefail

YES=0
if [ "${1:-}" = "-y" ] || [ "${1:-}" = "--yes" ]; then
  YES=1
  shift
fi

CONTAINER="${1:-wharf-agent}"

short() { echo "${1#sha256:}" | cut -c1-12; }

# One header line per run -- the log this appends to (cf. the cron
# example above) has nothing else marking where one invocation ends and
# the next begins otherwise.
echo "=== $(date '+%Y-%m-%d %H:%M:%S %Z') ==="

if ! docker inspect "$CONTAINER" >/dev/null 2>&1; then
  echo "error: no container named '$CONTAINER'" >&2
  exit 1
fi

IMAGE_REF=$(docker inspect --format '{{.Config.Image}}' "$CONTAINER")
CURRENT_DIGEST=$(docker inspect --format '{{.Image}}' "$CONTAINER")

echo "container:      $CONTAINER"
echo "image ref:      $IMAGE_REF"
echo "current digest: $(short "$CURRENT_DIGEST")"

echo "pulling $IMAGE_REF ..."
docker pull "$IMAGE_REF"

NEW_DIGEST=$(docker image inspect --format '{{.Id}}' "$IMAGE_REF")

if [ "$CURRENT_DIGEST" = "$NEW_DIGEST" ]; then
  echo "already up to date ($(short "$NEW_DIGEST")) -- nothing to do."
  exit 0
fi

echo "update available: $(short "$CURRENT_DIGEST") -> $(short "$NEW_DIGEST")"

if [ "$YES" -ne 1 ]; then
  read -r -p "Remove and recreate '$CONTAINER' on the new image? [y/N] " reply
  case "$reply" in
    [yY]|[yY][eE][sS]) ;;
    *) echo "aborted -- old container left untouched."; exit 0 ;;
  esac
fi

# Recreate with the exact same runtime config the container already has,
# read straight off it so this works unchanged across hosts with
# different mounts/flags (docker-1, docker-2, ...) rather than hardcoding
# any one host's setup.
RUN_ARGS=()
while IFS= read -r line; do
  [ -n "$line" ] && RUN_ARGS+=(-v "$line")
done < <(docker inspect --format '{{range .Mounts}}{{.Source}}:{{.Destination}}{{if not .RW}}:ro{{end}}{{"\n"}}{{end}}' "$CONTAINER")

mapfile -t CMD_ARGS < <(docker inspect --format '{{range .Args}}{{.}}{{"\n"}}{{end}}' "$CONTAINER")

RESTART_POLICY=$(docker inspect --format '{{.HostConfig.RestartPolicy.Name}}' "$CONTAINER")
NETWORK_MODE=$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$CONTAINER")

echo "removing $CONTAINER ..."
docker rm -f "$CONTAINER" >/dev/null

echo "recreating $CONTAINER on $IMAGE_REF ..."
docker run -d \
  --name "$CONTAINER" \
  --restart "${RESTART_POLICY:-unless-stopped}" \
  --network "$NETWORK_MODE" \
  "${RUN_ARGS[@]}" \
  "$IMAGE_REF" \
  "${CMD_ARGS[@]}"

echo "done -- $CONTAINER is now running $(short "$NEW_DIGEST")"
docker ps --filter "name=^${CONTAINER}\$"
