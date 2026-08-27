#!/usr/bin/env sh
# Reports DevMan availability and this project's state as a single JSON object.
#
# The script never starts anything: if the daemon is not running it says so,
# rather than launching a background process as a side effect of a check.
#
#   sh devman-check.sh [project dir]
#
# Requires only devman and a POSIX shell. jq is used when present for the
# registration details; without it the raw status JSON is passed through.

set -u

DIR=${1:-$PWD}
case $DIR in
    /*) ;;
    *) DIR=$PWD/$DIR ;;
esac

emit() {
    # $1 next_action, $2..$n extra "key": value fragments
    action=$1
    shift
    printf '{\n'
    printf '  "devman_installed": %s,\n' "$INSTALLED"
    printf '  "devman_version": %s,\n' "$VERSION"
    printf '  "daemon_running": %s,\n' "$DAEMON"
    printf '  "project_dir": "%s",\n' "$DIR"
    printf '  "config_path": "%s",\n' "$CONFIG"
    printf '  "config_present": %s,\n' "$PRESENT"
    printf '  "config_valid": %s,\n' "$VALID"
    for fragment in "$@"; do
        printf '  %s,\n' "$fragment"
    done
    printf '  "next_action": "%s"\n' "$action"
    printf '}\n'
    exit 0
}

json_string() {
    # Minimal JSON string escaping for the diagnostic passthrough.
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | awk 'BEGIN{ORS=""} {print (NR>1 ? "\\n" : "") $0}'
}

INSTALLED=false
VERSION=null
DAEMON=false
CONFIG=$DIR/devman.yaml
PRESENT=false
VALID=null

command -v devman >/dev/null 2>&1 || emit install_devman
INSTALLED=true

version_json=$(devman --json version 2>/dev/null || true)
case $version_json in
    *'"version"'*)
        VERSION=\"$(printf '%s' "$version_json" | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')\"
        ;;
esac

[ -f "$CONFIG" ] || emit init
PRESENT=true

validate_out=$(devman --json validate "$DIR" 2>&1)
if [ $? -eq 0 ]; then
    VALID=true
else
    VALID=false
    emit fix_config "\"config_output\": \"$(json_string "$validate_out")\""
fi

daemon_out=$(devman --json daemon status 2>&1)
case $daemon_out in
    *'"info"'*) DAEMON=true ;;
    # Registration lives in the daemon's database, so it cannot be answered here
    # without starting one. `devman status` will start it when the agent is ready.
    *) emit query_status ;;
esac

status_out=$(devman --json status --project "$DIR" 2>&1)
case $status_out in
    *'"code": "PROJECT_NOT_FOUND"'*|*'"code":"PROJECT_NOT_FOUND"'*)
        emit register '"registered": false'
        ;;
    *'"error"'*)
        emit fix_config "\"config_output\": \"$(json_string "$status_out")\""
        ;;
esac

if command -v jq >/dev/null 2>&1; then
    trusted=$(printf '%s' "$status_out" | jq -r '.trusted')
    running=$(printf '%s' "$status_out" | jq '[.services[]? | select(.status == "RUNNING")] | length')
    services=$(printf '%s' "$status_out" | jq -c '[.services[]? | {name, status, health: .health.status, ports: [.ports[]?.port], url, message}]')
    if [ "$trusted" != "true" ]; then
        action=trust
    elif [ "$running" = "0" ]; then
        action=start
    else
        action=nothing
    fi
    emit "$action" '"registered": true' "\"trusted\": $trusted" "\"services\": $services"
fi

# No jq: hand the caller the daemon's own JSON rather than a half-parsed summary.
emit query_status '"registered": true' "\"status\": $status_out"
