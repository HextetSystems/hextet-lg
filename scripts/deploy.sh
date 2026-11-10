#!/usr/bin/env bash
# Controlled upgrade and rollback of the Hextet LG Compose deployment.
#
# This is a thin safety wrapper around native `docker compose`. It adds only
# what Compose does not do by itself: it preserves the image being replaced
# under an immutable local tag, waits for health and readiness, checks that the
# metrics port is not published, and can put the previous image back.
#
# It never reads or writes configs/config*.yaml, never builds, never prunes and
# never removes images. Nothing happens without an explicit subcommand and an
# explicit image; without --yes it asks for confirmation on a terminal.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVICE="${HEXTET_LG_SERVICE:-hextet-lg}"
STATE_DIR="${HEXTET_LG_STATE_DIR:-$ROOT/.deploy-state}"
ENV_FILE="${HEXTET_LG_ENV_FILE:-$ROOT/.env}"
HISTORY="$STATE_DIR/history.tsv"
CONTAINER_PORT=8080

usage() {
  cat <<'EOF'
Usage:
  scripts/deploy.sh status
  scripts/deploy.sh upgrade <image> [options]
  scripts/deploy.sh rollback [<image>] [options]

<image> is a full image reference. For production prefer an immutable digest:
  ghcr.io/hextetsystems/hextet-lg@sha256:<64 hex digits>
A release tag (ghcr.io/hextetsystems/hextet-lg:v0.1.0) works but is mutable.

Options:
  --dry-run           validate and print the plan, change nothing
  --yes               do not ask for confirmation (required without a terminal)
  --timeout SECONDS   wait this long for the new container to be healthy (default 90)
  --no-auto-rollback  (upgrade) leave a failed new container in place for inspection
  --preserve-current  (upgrade) if the running image is no longer in the local image
                      store, preserve it with `docker commit` so it can be rolled back to
  --no-env-update     do not record the selected image in the .env file

Environment (for testing against an isolated Compose project):
  COMPOSE_FILE, COMPOSE_PROJECT_NAME   honoured by docker compose itself
  HEXTET_LG_ENV_FILE, HEXTET_LG_STATE_DIR   where the selection and history are kept

`rollback` without an image returns to the image that the last upgrade replaced.
EOF
}

log() { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

dc() { (cd "$ROOT" && docker compose "$@"); }
dc_with_image() { local img="$1"; shift; (cd "$ROOT" && HEXTET_LG_IMAGE="$img" docker compose "$@"); }

container_id() { dc ps -a -q "$SERVICE" | head -n1; }
image_exists() { docker image inspect "$1" >/dev/null 2>&1; }
image_id() { docker image inspect --format '{{.Id}}' "$1"; }
image_label() { docker image inspect --format "{{index .Config.Labels \"$2\"}}" "$1" 2>/dev/null || true; }
repo_digest() { docker image inspect --format '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' "$1" 2>/dev/null || true; }

describe_image() { # <ref-or-id>
  local v r c
  v="$(image_label "$1" org.opencontainers.image.version)"
  r="$(image_label "$1" org.opencontainers.image.revision)"
  c="$(image_label "$1" org.opencontainers.image.created)"
  printf 'version=%s revision=%s created=%s' "${v:-unknown}" "${r:-unknown}" "${c:-unknown}"
}

published_ports() { docker port "$1" 2>/dev/null | sort || true; }

record_history() { # action old_id rollback_ref new_ref new_id new_digest
  mkdir -p "$STATE_DIR"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" "$3" "$4" "$5" "$6" >>"$HISTORY"
}

set_env_image() { # ref
  [ "$NO_ENV_UPDATE" = 1 ] && return 0
  local tmp
  tmp="$(mktemp "${ENV_FILE}.XXXXXX")"
  if [ -f "$ENV_FILE" ]; then
    grep -v '^HEXTET_LG_IMAGE=' "$ENV_FILE" >"$tmp" || true
    chmod --reference="$ENV_FILE" "$tmp" 2>/dev/null || true
  fi
  printf 'HEXTET_LG_IMAGE=%s\n' "$1" >>"$tmp"
  mv "$tmp" "$ENV_FILE"
  log "Recorded HEXTET_LG_IMAGE=$1 in $ENV_FILE so a later plain 'docker compose up' keeps this image."
}

confirm() {
  [ "$ASSUME_YES" = 1 ] && return 0
  [ -t 0 ] || die "no terminal for confirmation; re-run with --yes (or --dry-run to review the plan)"
  local reply
  read -r -p "Type 'yes' to proceed: " reply
  [ "$reply" = yes ] || die "aborted; nothing was changed"
}

# wait_healthy <container-id> <timeout-seconds>; prints the reason on failure.
wait_healthy() {
  local cid="$1" deadline=$((SECONDS + $2)) state health
  while [ "$SECONDS" -lt "$deadline" ]; do
    state="$(docker inspect --format '{{.State.Status}}' "$cid" 2>/dev/null || echo missing)"
    health="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$cid" 2>/dev/null || echo missing)"
    case "$state/$health" in
      running/healthy) return 0 ;;
      running/starting) ;;
      running/none) die "container has no health check; refusing to treat it as healthy" ;;
      *) log "container state=$state health=$health"; [ "$state" != running ] && return 1 ;;
    esac
    sleep 2
  done
  log "timed out after $2s waiting for the container to become healthy"
  return 1
}

# verify <container-id> <timeout-seconds>: health, readiness, isolation, logging.
# Returns non-zero for failures that justify a rollback; logging format is advisory.
verify() {
  local cid="$1" timeout="$2" ports bad hostport
  wait_healthy "$cid" "$timeout" || return 1
  log "  health:    healthy"

  if docker exec "$cid" wget -q -O /dev/null "http://127.0.0.1:$CONTAINER_PORT/readyz" 2>/dev/null; then
    log "  readiness: /readyz OK"
  else
    log "  readiness: /readyz FAILED"; return 1
  fi

  ports="$(published_ports "$cid")"
  bad="$(printf '%s\n' "$ports" | grep -v "^$CONTAINER_PORT/tcp" | grep -v '^$' || true)"
  if [ -n "$bad" ]; then
    log "  isolation: UNEXPECTED published ports (only $CONTAINER_PORT may be published):"; printf '%s\n' "$bad"; return 1
  fi
  log "  isolation: only $CONTAINER_PORT/tcp is published (metrics port not published)"

  hostport="$(printf '%s\n' "$ports" | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p' | head -n1)"
  if [ -n "$hostport" ] && command -v curl >/dev/null 2>&1; then
    local code
    code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://127.0.0.1:$hostport/metrics" || true)"
    if [ "$code" = 404 ]; then log "  isolation: public listener answers /metrics with 404"
    else log "  isolation: public listener answered /metrics with HTTP $code (expected 404)"; return 1; fi
  fi

  local first
  first="$(docker logs --tail 50 "$cid" 2>&1 | grep -m1 -v '^$' || true)"
  if printf '%s' "$first" | grep -q '^{'; then log "  logging:   structured (JSON)"
  else warn "logging is not structured JSON (image predates M3.2, or logging.format: text)"; fi
  return 0
}

# replace_with <image-ref> <timeout>: controlled replacement through Compose only.
replace_with() {
  dc_with_image "$1" up -d --no-build --no-deps --pull never "$SERVICE"
}

do_status() {
  local cid
  cid="$(container_id)"
  [ -n "$cid" ] || { log "No '$SERVICE' container exists in this Compose project."; return 0; }
  log "container:      $cid ($(docker inspect --format '{{.Name}}' "$cid" | sed 's|^/||'))"
  log "state/health:   $(docker inspect --format '{{.State.Status}} / {{if .State.Health}}{{.State.Health.Status}}{{else}}no healthcheck{{end}}' "$cid")"
  log "started:        $(docker inspect --format '{{.State.StartedAt}}' "$cid")"
  log "configured ref: $(docker inspect --format '{{.Config.Image}}' "$cid")"
  local iid; iid="$(docker inspect --format '{{.Image}}' "$cid")"
  log "image id:       $iid"
  if image_exists "$iid"; then
    log "image metadata: $(describe_image "$iid")"
    log "repo digest:    $(repo_digest "$iid")"
  else
    log "image metadata: IMAGE NO LONGER IN THE LOCAL STORE (cannot be rolled back to; use 'upgrade --preserve-current')"
  fi
  log "ports:          $(published_ports "$cid" | tr '\n' ' ')"
  if [ -f "$ENV_FILE" ] && grep -q '^HEXTET_LG_IMAGE=' "$ENV_FILE"; then log "$ENV_FILE: $(grep '^HEXTET_LG_IMAGE=' "$ENV_FILE")"; fi
  if [ -f "$HISTORY" ]; then
    log "recent deployments (time, action, previous id, rollback ref, new ref, new id, new digest):"
    tail -n 5 "$HISTORY" | sed 's/^/  /'
  fi
}

ensure_target_available() { # <ref>
  if image_exists "$1"; then return 0; fi
  if [ "$DRY_RUN" = 1 ]; then
    log "Target is not in the local image store; checking the registry (no pull in a dry run)..."
    docker buildx imagetools inspect "$1" >/dev/null 2>&1 || die "image '$1' was not found locally or in its registry"
    log "Target exists in the registry."
  else
    log "Pulling $1 ..."
    docker pull "$1" >/dev/null || die "cannot pull '$1'"
  fi
}

do_upgrade() {
  local target="$1" cid cur_id cur_cfg rollback_ref new_id="" new_digest="" ts
  [ -n "$target" ] || { usage; exit 2; }
  case "$target" in *@sha256:????????????????????????????????????????????????????????????????) ;;
    *) warn "'$target' is not a digest reference; a mutable tag is not a safe rollback reference" ;; esac

  cid="$(container_id)"
  [ -n "$cid" ] || die "no '$SERVICE' container in this Compose project; use 'docker compose up -d' for a first deployment"
  cur_id="$(docker inspect --format '{{.Image}}' "$cid")"
  cur_cfg="$(docker inspect --format '{{.Config.Image}}' "$cid")"
  ts="$(date -u +%Y%m%dT%H%M%SZ)"
  rollback_ref="hextet-lg:rollback-$ts"

  ensure_target_available "$target"
  dc_with_image "$target" config -q || die "docker compose configuration is invalid for this image"
  log "Compose configuration valid."

  if image_exists "$target"; then
    new_id="$(image_id "$target")"
    new_digest="$(repo_digest "$target")"
    if [ "$new_id" = "$cur_id" ]; then
      log "The running container already uses image $new_id; nothing to do."
      return 0
    fi
  fi

  local cur_known=1
  image_exists "$cur_id" || cur_known=0
  if [ "$cur_known" = 0 ] && [ "$PRESERVE_CURRENT" != 1 ]; then
    die "the running image $cur_id is no longer in the local image store, so it could not be rolled back to.
       Re-run with --preserve-current to snapshot the running container's filesystem as $rollback_ref first."
  fi

  log ""
  log "PLAN"
  log "  project/service:  $SERVICE (container $cid)"
  log "  running image:    $cur_id"
  if [ "$cur_known" = 1 ]; then log "                    $(describe_image "$cur_id")"; else log "                    (not in the local store; will be preserved with docker commit)"; fi
  log "  configured as:    $cur_cfg  (a mutable name; the image id above is authoritative)"
  log "  rollback image:   $rollback_ref  (immutable local tag, created before replacement)"
  log "  new image:        $target"
  if [ -n "$new_id" ]; then log "                    id $new_id  $(describe_image "$target")"; log "                    digest ${new_digest:-none (locally built)}"; fi
  log "  private config:   untouched (bind mount stays as in compose.yaml)"
  log "  interruption:     the container is stopped (up to the stop grace period) and recreated; expect a brief outage"
  log "  health wait:      up to ${TIMEOUT}s; automatic rollback on failure: $([ "$NO_AUTO_ROLLBACK" = 1 ] && echo off || echo on)"
  if [ "$DRY_RUN" = 1 ]; then log ""; log "Dry run: nothing was changed."; return 0; fi
  log ""
  confirm

  # 1. Preserve the image being replaced under an immutable tag.
  if [ "$cur_known" = 1 ]; then
    docker tag "$cur_id" "$rollback_ref"
  else
    docker commit "$cid" "$rollback_ref" >/dev/null
    log "Preserved the running container as $rollback_ref (original image was not in the local store)."
  fi
  log "Rollback image: $rollback_ref -> $(image_id "$rollback_ref")"

  # 2. Replace through Compose.
  log "Replacing the container ..."
  if ! replace_with "$target"; then
    warn "docker compose failed during replacement"
    rollback_after_failure "$rollback_ref"; exit 1
  fi
  cid="$(container_id)"
  new_id="$(docker inspect --format '{{.Image}}' "$cid")"
  new_digest="$(repo_digest "$new_id")"

  # 3. Verify.
  log "Verifying new container $cid ..."
  if ! verify "$cid" "$TIMEOUT"; then
    warn "verification of the new container failed"
    rollback_after_failure "$rollback_ref"; exit 1
  fi
  record_history upgrade "$cur_id" "$rollback_ref" "$target" "$new_id" "${new_digest:-none}"
  set_env_image "$target"
  log ""
  log "Upgrade complete."
  log "  now running: $new_id ($(describe_image "$new_id"))"
  log "  to roll back: scripts/deploy.sh rollback    (returns to $rollback_ref)"
}

rollback_after_failure() { # <rollback-ref>
  if [ "$NO_AUTO_ROLLBACK" = 1 ]; then
    warn "automatic rollback disabled; the failed container was left in place. Roll back with: scripts/deploy.sh rollback $1"
    docker logs --tail 20 "$(container_id)" 2>&1 | sed 's/^/    | /' >&2 || true
    return 0
  fi
  warn "rolling back to $1"
  docker logs --tail 20 "$(container_id)" 2>&1 | sed 's/^/    | /' >&2 || true
  replace_with "$1" || die "ROLLBACK FAILED to start $1; the service is DOWN. Investigate with: docker compose logs $SERVICE"
  local cid; cid="$(container_id)"
  if verify "$cid" "$TIMEOUT"; then
    record_history auto-rollback "-" "-" "$1" "$(docker inspect --format '{{.Image}}' "$cid")" "-"
    set_env_image "$1"
    log "Rolled back to $1 and verified healthy. The upgrade did NOT take effect."
  else
    die "rollback container did not verify healthy; the service needs manual attention"
  fi
}

do_rollback() {
  local target="${1:-}" cid cur_id
  if [ -z "$target" ]; then
    [ -f "$HISTORY" ] || die "no deployment history at $HISTORY; give the image to roll back to explicitly"
    target="$(awk -F'\t' '$2=="upgrade" {r=$4} END {print r}' "$HISTORY")"
    [ -n "$target" ] || die "history has no upgrade to roll back; give the image explicitly"
  fi
  image_exists "$target" || die "rollback image '$target' is not in the local image store; pull it or give another reference"
  cid="$(container_id)"
  [ -n "$cid" ] || die "no '$SERVICE' container in this Compose project"
  cur_id="$(docker inspect --format '{{.Image}}' "$cid")"
  local tgt_id; tgt_id="$(image_id "$target")"
  if [ "$tgt_id" = "$cur_id" ]; then log "Already running $tgt_id; nothing to do."; return 0; fi
  dc_with_image "$target" config -q || die "docker compose configuration is invalid for this image"
  log "PLAN: roll back from $cur_id ($(describe_image "$cur_id" 2>/dev/null || true))"
  log "                  to $tgt_id ($(describe_image "$target")) via $target"
  if [ "$DRY_RUN" = 1 ]; then log "Dry run: nothing was changed."; return 0; fi
  # Keep the image being left behind reachable, too.
  local keep="hextet-lg:replaced-$(date -u +%Y%m%dT%H%M%SZ)"
  if image_exists "$cur_id"; then docker tag "$cur_id" "$keep"; log "Kept the image being replaced as $keep"; fi
  confirm
  replace_with "$target" || die "docker compose failed during rollback"
  cid="$(container_id)"
  log "Verifying ..."
  verify "$cid" "$TIMEOUT" || die "rolled-back container failed verification; manual attention needed"
  record_history rollback "$cur_id" "$keep" "$target" "$tgt_id" "$(repo_digest "$tgt_id")"
  set_env_image "$target"
  log "Rollback complete: running $tgt_id."
}

# ---- argument parsing ----------------------------------------------------------
[ $# -ge 1 ] || { usage; exit 2; }
CMD="$1"; shift
DRY_RUN=0; ASSUME_YES=0; TIMEOUT=90; NO_AUTO_ROLLBACK=0; PRESERVE_CURRENT=0; NO_ENV_UPDATE=0
ARG_IMAGE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --yes|-y) ASSUME_YES=1 ;;
    --timeout) shift; [ $# -gt 0 ] && [[ "$1" =~ ^[0-9]+$ ]] || die "--timeout needs a number of seconds"; TIMEOUT="$1" ;;
    --no-auto-rollback) NO_AUTO_ROLLBACK=1 ;;
    --preserve-current) PRESERVE_CURRENT=1 ;;
    --no-env-update) NO_ENV_UPDATE=1 ;;
    -h|--help) usage; exit 0 ;;
    -*) die "unknown option: $1" ;;
    *) [ -z "$ARG_IMAGE" ] || die "only one image may be given"; ARG_IMAGE="$1" ;;
  esac
  shift
done

command -v docker >/dev/null || die "docker is required"
case "$CMD" in
  status) do_status ;;
  upgrade) do_upgrade "$ARG_IMAGE" ;;
  rollback) do_rollback "$ARG_IMAGE" ;;
  -h|--help|help) usage ;;
  *) usage; exit 2 ;;
esac
