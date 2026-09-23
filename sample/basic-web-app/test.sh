#!/usr/bin/env bash
#
# test.sh — live waiting-room load test for the basic-web-app sample.
#
# Usage:
#   cd sample/basic-web-app
#   bash test.sh
#
# The script builds and starts the server, runs a load test, prints a
# live dashboard and a snapshot of the operator view (/admin/queue), then
# shuts everything down. Open http://localhost:8080/ in a browser while it
# runs to see yourself in the queue.
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

# Room lifecycle tags as printed by main.go's roomLog, e.g. "[ FULL    ]".
EVENT_PATTERN='\[ (FULL|DRAIN|QUEUE|ENTER|EXIT|EVICT|TIMEOUT|PROMOTE|REMOVE)'

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
#
#   sent      every session
#   served    every session that ended with a 200 (direct or after queuing)
#   queued    every session that landed in the waiting room
#   dequeued  every queued session that has finished (admitted or gave up)
#   errors    every session that ended without a 200

TMPDIR_TEST="$(mktemp -d)"

for t in sent served queued dequeued errors; do
    mkdir -p "${TMPDIR_TEST}/tally_${t}"
done

SERVER_PID=""
SERVER_LOG="${TMPDIR_TEST}/server.log"

# The server saves its queue on shutdown and restores it on startup. Keep
# that file inside this run's temp dir so a previous run's dead clients are
# never restored into the next test.
STATE_FILE="${TMPDIR_TEST}/queue-state.json"

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

# ── Helpers ──────────────────────────────────────────────────────────

tally() {
    local name="$1"
    local id="$2"
    touch "${TMPDIR_TEST}/tally_${name}/${id}"
}

tally_count() {
    local name="$1"
    find "${TMPDIR_TEST}/tally_${name}" -type f 2>/dev/null | wc -l | tr -d ' '
}

# sleep_ms sleeps for a whole number of milliseconds. Handles values of
# 1000 and above correctly (1500 → "1.500", not "0.1500").
sleep_ms() {
    local ms="$1"
    sleep "$(printf '%d.%03d' $((ms / 1000)) $((ms % 1000)))"
}

# grep_count prints the number of lines in FILE matching the extended
# regex PATTERN, always as a single plain integer (0 when the file is
# missing or nothing matches). grep -c exits 1 on zero matches, hence
# the "|| true".
grep_count() {
    local pattern="$1"
    local file="$2"
    local result
    result=$(grep -cE -- "$pattern" "$file" 2>/dev/null || true)
    result=$(head -1 <<< "$result" | tr -d '[:space:]')
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
    ROOM_STATE_FILE="$STATE_FILE" "${TMPDIR_TEST}/basic-web-app" > "$SERVER_LOG" 2>&1 &
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
#
# Behaves like a browser: stores cookies from EVERY response, including
# /queue/status polls — room re-sends room_ticket and room_probe on polls
# so they never expire during a long wait.

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
            # Same cadence as the real page: 3s plus up to ~0.5s of jitter.
            sleep_ms $(( (RANDOM % 500) + 3000 ))

            local status_json
            status_json=$(curl -s -c "$cookie_jar" -b "$cookie_jar" --max-time 5 \
                "${BASE_URL}/queue/status" 2>/dev/null || echo '{}')

            local ready
            ready=$(jq -r '.ready // false' <<< "$status_json" 2>/dev/null || echo "false")

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
                tally "dequeued" "$id"
                rm -f "$cookie_jar" "$body_file"
                return
            fi

            ((poll_count++)) || true
        done

        # Gave up waiting.
        tally "errors" "$id"
        tally "dequeued" "$id"
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

    local c_sent c_served c_queued c_dequeued c_errors
    c_sent=$(tally_count "sent")
    c_served=$(tally_count "served")
    c_queued=$(tally_count "queued")
    c_dequeued=$(tally_count "dequeued")
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

    # Sessions currently waiting in the room.
    local queue_now=$((c_queued - c_dequeued))
    if [[ $queue_now -lt 0 ]]; then queue_now=0; fi

    printf "\r  ${BOLD}[%3ds]${RESET} " "$elapsed"
    printf "${CYAN}sent:${RESET}%-4d " "$c_sent"
    printf "${GREEN}served:${RESET}%-4d " "$c_served"
    printf "${YELLOW}waiting:${RESET}%-4d " "$queue_now"
    printf "${RED}err:${RESET}%-3d " "$c_errors"
    printf "${MAGENTA}active:${RESET}%-3d " "$active"
    printf "${DIM}~%d req/s${RESET} " "$rps"
    printf "${DIM}[%s]${RESET}   " "$phase"
}

# ── Operator view ────────────────────────────────────────────────────
#
# Reads GET /admin/queue — the room's Queue() API behind a loopback-only
# endpoint. Tokens are never exposed; visitors appear by position and
# client key.

print_admin_snapshot() {
    echo -e "${BOLD}Operator view (GET /admin/queue?limit=5):${RESET}"
    echo -e "${DIM}──────────────────────────────────────────────────────────────────────${RESET}"

    local json
    json=$(curl -s --max-time 5 "${BASE_URL}/admin/queue?limit=5" 2>/dev/null || true)

    if [[ -z "$json" ]] || ! jq -e . >/dev/null 2>&1 <<< "$json"; then
        echo "  (admin view unavailable)"
    else
        jq -r '
            "  occupancy \(.occupancy)/\(.cap)   queue_depth \(.queue_depth)   live \(.live_queue_depth)   first_poll_grace \(.first_poll_grace)",
            (if (.tickets | length) == 0 then "  (nobody waiting)" else empty end),
            (.tickets[] | "  pos \(.position)\tclient=\(.client_key)\twaiting=\(.waiting_for)\tseen=\(.seen)\tpromoted=\(.promoted)")
        ' <<< "$json" || echo "  (could not parse admin view)"
    fi

    echo -e "${DIM}──────────────────────────────────────────────────────────────────────${RESET}"
    echo ""
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
            sleep_ms "$RAMP_DELAY_MS"
        done

        local wave_start=$SECONDS
        while [[ $((SECONDS - wave_start)) -lt 3 ]] && [[ $SECONDS -lt $end_time ]]; do
            print_dashboard "$((SECONDS - start_time))" "wave ${wave}"
            sleep 0.5
        done
    done

    echo ""
    echo ""

    # Snapshot the line while it is at its deepest.
    print_admin_snapshot

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
    #
    # Capture first, then trim with a here-string. Piping grep into head
    # under `set -o pipefail` makes the pipeline "fail" whenever head
    # stops reading early (grep gets SIGPIPE), which previously printed
    # "(no lifecycle events captured)" right after printing the events.

    echo -e "${BOLD}Server lifecycle events:${RESET}"
    echo -e "${DIM}──────────────────────────────────────────────────────────────────────${RESET}"
    if [[ -f "$SERVER_LOG" ]]; then
        local events
        events=$(grep -E -- "$EVENT_PATTERN" "$SERVER_LOG" 2>/dev/null || true)

        if [[ -z "$events" ]]; then
            echo "  (no lifecycle events captured)"
        else
            head -30 <<< "$events"

            local event_count
            event_count=$(grep_count "$EVENT_PATTERN" "$SERVER_LOG")
            if [[ "$event_count" -gt 30 ]]; then
                echo -e "  ${DIM}... and $((event_count - 30)) more events${RESET}"
            fi
        fi
    else
        echo "  (server log not found)"
    fi
    echo -e "${DIM}──────────────────────────────────────────────────────────────────────${RESET}"
    echo ""

    # ── Summary ──────────────────────────────────────────────────────

    local c_sent c_served c_queued c_dequeued c_errors
    c_sent=$(tally_count "sent")
    c_served=$(tally_count "served")
    c_queued=$(tally_count "queued")
    c_dequeued=$(tally_count "dequeued")
    c_errors=$(tally_count "errors")

    local still_waiting=$((c_queued - c_dequeued))
    if [[ $still_waiting -lt 0 ]]; then still_waiting=0; fi

    local total_elapsed=$((SECONDS - start_time))
    local effective_rps=0
    if [[ $total_elapsed -gt 0 ]]; then
        effective_rps=$((c_served / total_elapsed))
    fi

    local full_events drain_events queue_events evict_events
    full_events=$(grep_count '\[ FULL' "$SERVER_LOG")
    drain_events=$(grep_count '\[ DRAIN' "$SERVER_LOG")
    queue_events=$(grep_count '\[ QUEUE' "$SERVER_LOG")
    evict_events=$(grep_count '\[ EVICT' "$SERVER_LOG")

    echo -e "${BOLD}╔══════════════════════════════════════════════════╗${RESET}"
    echo -e "${BOLD}║   Results                                       ║${RESET}"
    echo -e "${BOLD}╠══════════════════════════════════════════════════╣${RESET}"
    printf  "${BOLD}║${RESET}  %-22s  ${CYAN}%5d${RESET}                  ${BOLD}║${RESET}\n" "Total sent:" "$c_sent"
    printf  "${BOLD}║${RESET}  %-22s  ${GREEN}%5d${RESET}                  ${BOLD}║${RESET}\n" "Served (200):" "$c_served"
    printf  "${BOLD}║${RESET}  %-22s  ${YELLOW}%5d${RESET}                  ${BOLD}║${RESET}\n" "Queued (waited):" "$c_queued"
    printf  "${BOLD}║${RESET}  %-22s  ${YELLOW}%5d${RESET}                  ${BOLD}║${RESET}\n" "Still waiting at end:" "$still_waiting"
    printf  "${BOLD}║${RESET}  %-22s  ${RED}%5d${RESET}                  ${BOLD}║${RESET}\n" "Errors:" "$c_errors"
    printf  "${BOLD}║${RESET}  %-22s  %3ds                    ${BOLD}║${RESET}\n" "Elapsed:" "$total_elapsed"
    printf  "${BOLD}║${RESET}  %-22s  %3d req/s              ${BOLD}║${RESET}\n" "Throughput:" "$effective_rps"
    printf  "${BOLD}║${RESET}  %-22s  %3d                     ${BOLD}║${RESET}\n" "Waves:" "$wave"
    echo -e "${BOLD}╠══════════════════════════════════════════════════╣${RESET}"
    printf  "${BOLD}║${RESET}  %-22s  %5d                  ${BOLD}║${RESET}\n" "FULL transitions:" "$full_events"
    printf  "${BOLD}║${RESET}  %-22s  %5d                  ${BOLD}║${RESET}\n" "DRAIN transitions:" "$drain_events"
    printf  "${BOLD}║${RESET}  %-22s  %5d                  ${BOLD}║${RESET}\n" "QUEUE events:" "$queue_events"
    printf  "${BOLD}║${RESET}  %-22s  %5d                  ${BOLD}║${RESET}\n" "EVICT events:" "$evict_events"
    echo -e "${BOLD}╚══════════════════════════════════════════════════╝${RESET}"
    echo ""

    if [[ "$c_queued" -gt 0 ]]; then
        echo -e "${GREEN}✓${RESET} Waiting room activated — ${c_queued} requests queued."
        echo -e "  ${full_events} FULL / ${drain_events} DRAIN transitions."
    else
        echo -e "${YELLOW}⚠${RESET}  No requests were queued. Try:"
        echo "     CONCURRENCY=100 bash test.sh"
    fi

    if [[ "$still_waiting" -gt 0 ]]; then
        echo ""
        echo -e "${DIM}  ${still_waiting} clients were still waiting when the drain window closed.${RESET}"
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
