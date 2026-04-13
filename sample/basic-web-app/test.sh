#!/usr/bin/env bash
#
# test.sh — live waiting-room load test for the basic-web-app sample.
#
# Usage:
#   cd sample/basic-web-app
#   bash test.sh
#
# The script builds and starts the server, runs a load test, prints a
# live dashboard, then shuts everything down. Open http://localhost:8080/
# in a browser while it runs to see yourself in the queue.
#
# Requirements:
#   bash  >= 5.2
#   go    (to build and run the server)
#   curl  (any recent version)
#   jq    (for JSON parsing)
#
# Works on macOS and Linux — no flock, no GNU coreutils required.
#
# ─────────────────────────────────────────────────────────────────────

set -euo pipefail

# ── Guard: bash version ──────────────────────────────────────────────

if [[ "${BASH_VERSINFO[0]}" -lt 5 ]] || { [[ "${BASH_VERSINFO[0]}" -eq 5 ]] && [[ "${BASH_VERSINFO[1]}" -lt 2 ]]; }; then
    echo "error: bash >= 5.2 required (found ${BASH_VERSION})" >&2
    exit 1
fi

# ── Guard: not root ──────────────────────────────────────────────────

if [[ "$(id -u)" -eq 0 ]]; then
    echo "error: do not run as root" >&2
    exit 1
fi

# ── Guard: dependencies ──────────────────────────────────────────────

for cmd in go curl jq; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "error: $cmd is required but not found in PATH" >&2
        exit 1
    fi
done

# ── Configuration ────────────────────────────────────────────────────

BASE_URL="${BASE_URL:-http://localhost:8080}"
TARGET_PATH="${TARGET_PATH:-/about}"
CONCURRENCY="${CONCURRENCY:-30}"
DURATION_SECS="${DURATION_SECS:-30}"
RAMP_DELAY_MS="${RAMP_DELAY_MS:-50}"

# ── Colors ───────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
MAGENTA='\033[0;35m'
CYAN='\033[0;36m'
BOLD='\033[1m'
DIM='\033[2m'
RESET='\033[0m'

# ── Temp directory and tally directories ─────────────────────────────
#
# Each client session touches a unique file in a per-event directory.
# The dashboard counts files. Lock-free, atomic, works everywhere.

TMPDIR_TEST="$(mktemp -d)"

mkdir -p "${TMPDIR_TEST}/tally_sent"
mkdir -p "${TMPDIR_TEST}/tally_served"
mkdir -p "${TMPDIR_TEST}/tally_queued"
mkdir -p "${TMPDIR_TEST}/tally_errors"

SERVER_PID=""
SERVER_LOG="${TMPDIR_TEST}/server.log"

# ── Cleanup ──────────────────────────────────────────────────────────

cleanup() {
    echo ""
    if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        echo -e "${DIM}Stopping server (PID ${SERVER_PID})...${RESET}"
        kill -SIGTERM "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
    jobs -rp 2>/dev/null | xargs kill 2>/dev/null || true
    wait 2>/dev/null || true
    rm -rf "$TMPDIR_TEST"
}
trap cleanup EXIT

# ── Tally helpers (lock-free, subshell-safe) ─────────────────────────

tally() {
    local name="$1"
    local id="$2"
    touch "${TMPDIR_TEST}/tally_${name}/${id}"
}

tally_count() {
    local name="$1"
    find "${TMPDIR_TEST}/tally_${name}" -type f 2>/dev/null | wc -l | tr -d ' '
}

# ── Safe grep count (always returns a plain integer) ─────────────────
# grep -c can produce unexpected output on macOS with certain inputs.
# This helper always returns a single integer on stdout.

grep_count() {
    local pattern="$1"
    local file="$2"
    local result
    result=$(grep -c "$pattern" "$file" 2>/dev/null || true)
    # Strip whitespace and take only the first line.
    result=$(echo "$result" | head -1 | tr -d '[:space:]')
    if [[ -z "$result" ]] || ! [[ "$result" =~ ^[0-9]+$ ]]; then
        echo "0"
    else
        echo "$result"
    fi
}

# ── Start the server ─────────────────────────────────────────────────

start_server() {
    echo -e "${DIM}Building server...${RESET}"

    if [[ ! -f "main.go" ]]; then
        echo "error: main.go not found. Run this from sample/basic-web-app/" >&2
        exit 1
    fi

    go build -o "${TMPDIR_TEST}/basic-web-app" . 2>&1

    echo -e "${DIM}Starting server...${RESET}"
    "${TMPDIR_TEST}/basic-web-app" > "$SERVER_LOG" 2>&1 &
    SERVER_PID=$!

    local attempts=0
    while [[ $attempts -lt 100 ]]; do
        if curl -s -o /dev/null --max-time 1 "${BASE_URL}/" 2>/dev/null; then
            echo -e "${GREEN}✓${RESET} Server is up at ${BASE_URL} (PID ${SERVER_PID})"
            return 0
        fi
        sleep 0.1
        ((attempts++)) || true
    done

    echo -e "${RED}error:${RESET} server did not start within 10 seconds" >&2
    if [[ -f "$SERVER_LOG" ]]; then
        echo "Last 20 lines of server log:" >&2
        tail -20 "$SERVER_LOG" >&2
    fi
    exit 1
}

# ── Single client session ────────────────────────────────────────────

client_session() {
    local id="$1"
    local cookie_jar="${TMPDIR_TEST}/cookies_${id}.txt"
    local body_file="${TMPDIR_TEST}/body_${id}.txt"

    local http_code
    http_code=$(curl -s -o "$body_file" -w '%{http_code}' \
        -c "$cookie_jar" -b "$cookie_jar" \
        --max-time 10 \
        "${BASE_URL}${TARGET_PATH}" 2>/dev/null || echo "000")

    tally "sent" "$id"

    if [[ -f "$body_file" ]] && grep -q "You're in the queue" "$body_file" 2>/dev/null; then
        tally "queued" "$id"

        local max_polls=40
        local poll_count=0
        while [[ $poll_count -lt $max_polls ]]; do
            local jitter_ms=$(( (RANDOM % 1000) + 2500 ))
            sleep "$(printf '%d.%03d' $((jitter_ms / 1000)) $((jitter_ms % 1000)))"

            local status_json
            status_json=$(curl -s -b "$cookie_jar" --max-time 5 \
                "${BASE_URL}/queue/status" 2>/dev/null || echo '{}')

            local ready
            ready=$(echo "$status_json" | jq -r '.ready // false' 2>/dev/null || echo "false")

            if [[ "$ready" == "true" ]]; then
                http_code=$(curl -s -o /dev/null -w '%{http_code}' \
                    -c "$cookie_jar" -b "$cookie_jar" \
                    --max-time 10 \
                    "${BASE_URL}${TARGET_PATH}" 2>/dev/null || echo "000")

                if [[ "$http_code" == "200" ]]; then
                    tally "served" "$id"
                else
                    tally "errors" "$id"
                fi
                rm -f "$cookie_jar" "$body_file"
                return
            fi

            ((poll_count++)) || true
        done

        tally "errors" "$id"
    elif [[ "$http_code" == "200" ]]; then
        tally "served" "$id"
    else
        tally "errors" "$id"
    fi

    rm -f "$cookie_jar" "$body_file"
}

# ── Dashboard ────────────────────────────────────────────────────────

print_dashboard() {
    local elapsed="$1"
    local phase="$2"

    local c_sent c_served c_queued c_errors
    c_sent=$(tally_count "sent")
    c_served=$(tally_count "served")
    c_queued=$(tally_count "queued")
    c_errors=$(tally_count "errors")

    local active
    active=$(jobs -rp 2>/dev/null | wc -l | tr -d ' ')
    if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        active=$((active - 1))
        if [[ $active -lt 0 ]]; then active=0; fi
    fi

    local rps=0
    if [[ $elapsed -gt 0 ]]; then
        rps=$((c_served / elapsed))
    fi

    local queue_now=$((c_queued - c_served - c_errors))
    if [[ $queue_now -lt 0 ]]; then queue_now=0; fi

    printf "\r  ${BOLD}[%3ds]${RESET} " "$elapsed"
    printf "${CYAN}sent:${RESET}%-4d " "$c_sent"
    printf "${GREEN}served:${RESET}%-4d " "$c_served"
    printf "${YELLOW}queued:${RESET}%-4d " "$queue_now"
    printf "${RED}err:${RESET}%-3d " "$c_errors"
    printf "${MAGENTA}active:${RESET}%-3d " "$active"
    printf "${DIM}~%d req/s${RESET} " "$rps"
    printf "${DIM}[%s]${RESET}   " "$phase"
}

# ── Main ─────────────────────────────────────────────────────────────

main() {
    echo ""
    echo -e "${BOLD}╔══════════════════════════════════════════════════╗${RESET}"
    echo -e "${BOLD}║   room — Waiting Room Load Test                 ║${RESET}"
    echo -e "${BOLD}╚══════════════════════════════════════════════════╝${RESET}"
    echo ""
    echo -e "  target:       ${BOLD}${BASE_URL}${TARGET_PATH}${RESET}"
    echo -e "  concurrency:  ${BOLD}${CONCURRENCY}${RESET} simultaneous clients"
    echo -e "  duration:     ${BOLD}${DURATION_SECS}s${RESET}"
    echo -e "  ramp delay:   ${BOLD}${RAMP_DELAY_MS}ms${RESET} between client launches"
    echo ""

    start_server
    echo ""

    echo -e "${DIM}──────────────────────────────────────────────────────────────────────${RESET}"
    echo -e "${BOLD}  Open ${CYAN}${BASE_URL}/${RESET}${BOLD} in your browser to see the waiting room.${RESET}"
    echo -e "${DIM}──────────────────────────────────────────────────────────────────────${RESET}"
    echo ""
    echo -e "  ${DIM}Server log: tail -f ${SERVER_LOG}${RESET}"
    echo ""

    sleep 2

    local start_time=$SECONDS
    local end_time=$((SECONDS + DURATION_SECS))
    local wave=0

    echo -e "${GREEN}▶${RESET} Starting load test..."
    echo ""

    while [[ $SECONDS -lt $end_time ]]; do
        ((wave++)) || true

        local batch_size=$CONCURRENCY
        local remaining=$((end_time - SECONDS))
        if [[ $remaining -lt 5 ]]; then
            batch_size=$(( (CONCURRENCY / 3) + 1 ))
        fi

        for (( i=0; i<batch_size; i++ )); do
            client_session "${wave}_${i}" &
            sleep "$(printf '0.%03d' "$RAMP_DELAY_MS")" 2>/dev/null || sleep 0.05
        done

        local wave_start=$SECONDS
        while [[ $((SECONDS - wave_start)) -lt 3 ]] && [[ $SECONDS -lt $end_time ]]; do
            print_dashboard "$((SECONDS - start_time))" "wave ${wave}"
            sleep 0.5
        done
    done

    echo ""
    echo ""
    echo -e "${YELLOW}⏳${RESET} Draining in-flight requests (up to 30s)..."
    echo ""

    local drain_deadline=$((SECONDS + 30))
    while [[ $SECONDS -lt $drain_deadline ]]; do
        local bg_count
        bg_count=$(jobs -rp 2>/dev/null | wc -l | tr -d ' ')
        if [[ $bg_count -le 1 ]]; then
            break
        fi
        print_dashboard "$((SECONDS - start_time))" "draining"
        sleep 1
    done

    echo ""
    echo ""

    # ── Server log highlights ────────────────────────────────────────
    # The server log uses tags like "[ FULL    ]" with internal spaces,
    # so we grep for the keyword anywhere on the line.

    echo -e "${BOLD}Server lifecycle events:${RESET}"
    echo -e "${DIM}──────────────────────────────────────────────────────────────────────${RESET}"
    if [[ -f "$SERVER_LOG" ]]; then
        # Match the actual log format: [ FULL   ], [ DRAIN  ], etc.
        grep -E '(FULL|DRAIN|QUEUE|ENTER|EXIT|EVICT|TIMEOUT)' "$SERVER_LOG" \
            | head -30 || echo "  (no lifecycle events captured)"

        local event_count
        event_count=$(grep_count -E 'FULL|DRAIN|QUEUE|ENTER|EXIT|EVICT|TIMEOUT' "$SERVER_LOG")
        if [[ "$event_count" -gt 30 ]]; then
            echo -e "  ${DIM}... and $((event_count - 30)) more events${RESET}"
        fi
    else
        echo "  (server log not found)"
    fi
    echo -e "${DIM}──────────────────────────────────────────────────────────────────────${RESET}"
    echo ""

    # ── Summary ──────────────────────────────────────────────────────

    local c_sent c_served c_queued c_errors
    c_sent=$(tally_count "sent")
    c_served=$(tally_count "served")
    c_queued=$(tally_count "queued")
    c_errors=$(tally_count "errors")

    local total_elapsed=$((SECONDS - start_time))
    local effective_rps=0
    if [[ $total_elapsed -gt 0 ]]; then
        effective_rps=$((c_served / total_elapsed))
    fi

    local full_events drain_events queue_events
    full_events=$(grep_count 'FULL' "$SERVER_LOG")
    drain_events=$(grep_count 'DRAIN' "$SERVER_LOG")
    queue_events=$(grep_count 'QUEUE' "$SERVER_LOG")

    echo -e "${BOLD}╔══════════════════════════════════════════════════╗${RESET}"
    echo -e "${BOLD}║   Results                                       ║${RESET}"
    echo -e "${BOLD}╠══════════════════════════════════════════════════╣${RESET}"
    printf  "${BOLD}║${RESET}  %-22s  ${CYAN}%5d${RESET}                  ${BOLD}║${RESET}\n" "Total sent:" "$c_sent"
    printf  "${BOLD}║${RESET}  %-22s  ${GREEN}%5d${RESET}                  ${BOLD}║${RESET}\n" "Served (200):" "$c_served"
    printf  "${BOLD}║${RESET}  %-22s  ${YELLOW}%5d${RESET}                  ${BOLD}║${RESET}\n" "Queued (waited):" "$c_queued"
    printf  "${BOLD}║${RESET}  %-22s  ${RED}%5d${RESET}                  ${BOLD}║${RESET}\n" "Errors:" "$c_errors"
    printf  "${BOLD}║${RESET}  %-22s  %3ds                    ${BOLD}║${RESET}\n" "Elapsed:" "$total_elapsed"
    printf  "${BOLD}║${RESET}  %-22s  %3d req/s              ${BOLD}║${RESET}\n" "Throughput:" "$effective_rps"
    printf  "${BOLD}║${RESET}  %-22s  %3d                     ${BOLD}║${RESET}\n" "Waves:" "$wave"
    echo -e "${BOLD}╠══════════════════════════════════════════════════╣${RESET}"
    printf  "${BOLD}║${RESET}  %-22s  %5d                  ${BOLD}║${RESET}\n" "FULL transitions:" "$full_events"
    printf  "${BOLD}║${RESET}  %-22s  %5d                  ${BOLD}║${RESET}\n" "DRAIN transitions:" "$drain_events"
    printf  "${BOLD}║${RESET}  %-22s  %5d                  ${BOLD}║${RESET}\n" "QUEUE events:" "$queue_events"
    echo -e "${BOLD}╚══════════════════════════════════════════════════╝${RESET}"
    echo ""

    if [[ "$c_queued" -gt 0 ]]; then
        echo -e "${GREEN}✓${RESET} Waiting room activated — ${c_queued} requests queued."
        echo -e "  ${full_events} FULL / ${drain_events} DRAIN transitions."
    else
        echo -e "${YELLOW}⚠${RESET}  No requests were queued. Try:"
        echo "     CONCURRENCY=100 bash test.sh"
    fi

    if [[ "$c_errors" -gt 0 ]]; then
        echo ""
        echo -e "${YELLOW}⚠${RESET}  ${c_errors} errors — expected for clients whose poll timeout"
        echo "   expired before admission."
    fi

    echo ""
    echo -e "${DIM}Full server log: ${SERVER_LOG}${RESET}"
    echo ""
}

main "$@"