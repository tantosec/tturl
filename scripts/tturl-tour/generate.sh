#!/usr/bin/env bash
# Generate a representative tour of tturl's help and report surfaces.
#
# The user supplies a running demo server. By default it is expected at
# https://127.0.0.1:8443; override it with --url or TTURL_TOUR_BASE_URL.

set -euo pipefail
export LC_ALL=C

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../.." && pwd)
captures_root=$script_dir/captures
base_url=${TTURL_TOUR_BASE_URL:-https://127.0.0.1:8443}
tour_verbosity=${TTURL_TOUR_VERBOSITY:-as-authored}
failures=()
capture_count=0
slug=

usage() {
	cat <<'EOF'
Usage: scripts/tturl-tour/generate.sh [OPTIONS]

Generate a representative output tour against a running tturl demo server.

Options:
  -u, --url URL    demo-server base URL
                    (default: $TTURL_TOUR_BASE_URL or https://127.0.0.1:8443)
  -s, --slug SLUG  append SLUG to the UTC capture-directory name
  -h, --help       show this help and exit

SLUG may contain letters and numbers separated by '.', '_', or '-'. Captures
are written beneath this script at captures/<UTC-datetime>[-SLUG]/.

Environment:
  TTURL_TOUR_BASE_URL   default demo-server base URL
  TTURL_TOUR_VERBOSITY as-authored, normal, or verbose (default: as-authored)
EOF
}

usage_error() {
	printf 'tturl tour: %s\n\n' "$1" >&2
	usage >&2
	exit 2
}

while (($# > 0)); do
	case $1 in
	-h | --help)
		usage
		exit 0
		;;
	-u | --url)
		(($# >= 2)) || usage_error "$1 requires a URL"
		base_url=$2
		shift 2
		;;
	--url=*)
		base_url=${1#*=}
		shift
		;;
	-s | --slug)
		(($# >= 2)) || usage_error "$1 requires a slug"
		slug=$2
		shift 2
		;;
	--slug=*)
		slug=${1#*=}
		shift
		;;
	--)
		shift
		(($# == 0)) || usage_error "unexpected positional argument: $1"
		;;
	-*)
		usage_error "unknown option: $1"
		;;
	*)
		usage_error "unexpected positional argument: $1"
		;;
	esac
done

if [[ -n $slug && ! $slug =~ ^[A-Za-z0-9]+([._-][A-Za-z0-9]+)*$ ]]; then
	printf 'tturl tour: invalid slug: %s\n' "$slug" >&2
	printf "use letters and numbers separated by '.', '_' or '-'\n" >&2
	exit 2
fi
if [[ ! $base_url =~ ^https?://[^/[:space:]]+(/.*)?$ ]]; then
	printf 'tturl tour: invalid demo-server URL: %s\n' "$base_url" >&2
	printf 'use an absolute http:// or https:// URL\n' >&2
	exit 2
fi
base_url=${base_url%/}
sleep_url=$base_url/sleep
case $tour_verbosity in
as-authored | normal | verbose) ;;
*)
	printf 'tturl tour: TTURL_TOUR_VERBOSITY must be as-authored, normal, or verbose\n' >&2
	exit 2
	;;
esac

require_command() {
	if ! command -v "$1" >/dev/null 2>&1; then
		printf 'tturl tour: %s is required\n' "$1" >&2
		exit 1
	fi
}

require_command cat
require_command curl
require_command date
require_command go
require_command grep
require_command head
require_command mktemp
require_command python3
require_command rm

if ! curl -ksS --max-time 3 --output /dev/null "$sleep_url?duration=0"; then
	printf 'tturl tour: demo server is unavailable at %s\n' \
		"$base_url" >&2
	exit 1
fi

build_dir=$(mktemp -d "${TMPDIR:-/tmp}/tturl-tour.XXXXXX") || exit 1
trap 'rm -rf "$build_dir"' EXIT
tturl_bin=$build_dir/tturl

cd "$repo_root" || exit 1
if ! go build -o "$tturl_bin" ./cmd/tturl; then
	printf 'tturl tour: could not build tturl\n' >&2
	exit 1
fi

capture_id=$(date -u '+%Y-%m-%dT%H-%M-%SZ')
if [[ -n $slug ]]; then
	capture_id+=-$slug
fi
capture_dir=$captures_root/$capture_id
if ! mkdir "$capture_dir" 2>/dev/null; then
	printf 'tturl tour: capture directory already exists: %s\n' \
		"$capture_dir" >&2
	exit 1
fi
mkdir "$capture_dir/usage" "$capture_dir/commands"

render_command() {
	local suffix=$1
	local command=$2
	local argument quoted value_quoted i
	local -a groups=()
	shift 2

	while (($# > 0)); do
		argument=$1
		shift
		printf -v quoted '%q' "$argument"
		if [[ $argument == -* && $# -gt 0 && $1 != -* ]]; then
			printf -v value_quoted '%q' "$1"
			quoted+=" $value_quoted"
			shift
		fi
		groups+=("$quoted")
	done

	if ((${#groups[@]} == 0)); then
		printf '%% tturl %q%s\n\n' "$command" "$suffix"
		return
	fi
	printf '%% tturl %q \\\n' "$command"
	for ((i = 0; i < ${#groups[@]}; i++)); do
		if ((i + 1 < ${#groups[@]})); then
			printf '    %s \\\n' "${groups[$i]}"
		else
			printf '    %s%s\n\n' "${groups[$i]}" "$suffix"
		fi
	done
}

# capture_in_mode CATEGORY NAME EXPECTED_EXIT MODE SUFFIX ARGS...
capture_in_mode() {
	local category=$1
	local name=$2
	local expected=$3
	local mode=$4
	local suffix=$5
	local destination=$capture_dir/$category/$name.txt
	local status
	shift 5
	local -a capture_args=("$@")
	if [[ $category == commands && $tour_verbosity != as-authored ]]; then
		local -a rewritten=("${capture_args[0]}")
		local argument
		for argument in "${capture_args[@]:1}"; do
			[[ $argument == --verbose || $argument == -v ]] ||
				rewritten+=("$argument")
		done
		if [[ $tour_verbosity == verbose ]]; then
			capture_args=("${rewritten[0]}" --verbose "${rewritten[@]:1}")
		else
			capture_args=("${rewritten[@]}")
		fi
	fi

	if [[ -e $destination ]]; then
		printf 'duplicate capture name: %s\n' "$name" >&2
		exit 1
	fi

	{
		render_command "$suffix" "${capture_args[@]}"
		set +e
		python3 - "$mode" "$tturl_bin" "${capture_args[@]}" <<'PY'
import os
import selectors
import subprocess
import sys

mode = sys.argv[1]
process = subprocess.Popen(
    sys.argv[2:],
    stdin=subprocess.DEVNULL,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
)
consumer = None
stdout = process.stdout
extra_stderr = None
if mode in ("cat", "head"):
    command = ["cat"] if mode == "cat" else ["head", "-n", "8"]
    consumer = subprocess.Popen(
        command,
        stdin=process.stdout,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    process.stdout.close()
    stdout = consumer.stdout
    extra_stderr = consumer.stderr

selector = selectors.DefaultSelector()
# Register stderr first so an immediately available command notice precedes a
# simultaneously available stdout banner.
selector.register(process.stderr, selectors.EVENT_READ, b"STDERR: ")
if extra_stderr is not None:
    selector.register(extra_stderr, selectors.EVENT_READ, b"STDERR: ")
selector.register(stdout, selectors.EVENT_READ, b"")
buffers = {key.fileobj: b"" for key in selector.get_map().values()}

while selector.get_map():
    for key, _ in selector.select():
        chunk = os.read(key.fileobj.fileno(), 4096)
        if not chunk:
            remainder = buffers[key.fileobj]
            if remainder:
                sys.stdout.buffer.write(key.data + remainder + b"\n")
            selector.unregister(key.fileobj)
            continue

        pending = buffers[key.fileobj] + chunk
        lines = pending.split(b"\n")
        buffers[key.fileobj] = lines.pop()
        for line in lines:
            sys.stdout.buffer.write(key.data + line + b"\n")
        sys.stdout.buffer.flush()

consumer_status = consumer.wait() if consumer is not None else 0
process_status = process.wait()
sys.exit(process_status if process_status != 0 else consumer_status)
PY
		status=$?
		set -e
	} >"$destination"

	capture_count=$((capture_count + 1))
	if [[ $status -ne $expected ]]; then
		failures+=("$category/$name: exit $status, expected $expected")
	fi
	printf 'captured %s/%s.txt\n' "$category" "$name"
}

# capture_in CATEGORY NAME EXPECTED_EXIT ARGS...
capture_in() {
	capture_in_mode "$1" "$2" "$3" direct "" "${@:4}"
}

capture() {
	capture_in commands "$@"
}

capture_usage() {
	capture_in usage "$@"
}

capture_pipe() {
	local name=$1
	local expected=$2
	local consumer=$3
	local suffix=" | $consumer"
	shift 3
	if [[ $consumer == head ]]; then
		suffix=' | head -n 8'
	fi
	capture_in_mode commands "$name" "$expected" "$consumer" "$suffix" "$@"
}

validate_json_capture() {
	local name=$1
	local path=$capture_dir/commands/$name.txt
	if ! python3 - "$path" "${2:-}" <<'PY'
import json
import sys

with open(sys.argv[1]) as capture:
    records = [json.loads(line) for line in capture if line.startswith("{")]
assert records, "no JSON records"
assert records[0]["kind"] == "run", "first record must define the run"
assert records[-1]["kind"] == "result", "last record must close the run"
assert sum(record["kind"] == "run" for record in records) == 1
assert sum(record["kind"] == "result" for record in records) == 1
assert sum(record["kind"] == "request" for record in records) == records[0]["request_count"]
assert all(type(record) == dict and type(record["kind"]) == str for record in records)
if sys.argv[2]:
    assert records[-1]["completion"]["state"] == sys.argv[2]
PY
	then
		failures+=("commands/$name: invalid JSON record structure")
	fi
}

require_capture_text() {
	local name=$1
	local text=$2
	local path=$capture_dir/commands/$name.txt
	if ! grep -Fq -- "$text" "$path"; then
		failures+=("commands/$name: missing expected text: $text")
	fi
}

# Concise usage surfaces: root help and every command-level -h page.
capture_usage 01-tturl 0 -h
capture_usage 02-race 0 race -h
capture_usage 03-measure 0 measure -h
capture_usage 04-analyse 0 analyse -h
capture_usage 05-detect 0 detect -h
capture_usage 06-demo-server 0 demo-server -h
capture_usage 07-completion 0 completion -h
capture_usage 08-help 0 help -h
capture_usage 09-time 0 time -h

# race: exact response-level reports and their noisier edges.
capture 01-race-two-request-baseline 0 \
	race --insecure \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture 02-race-warmup-parallel-trials 0 \
	race --insecure --trials 3 --warmup 1 --connections 2 \
	--block "$sleep_url?duration=0" --name A --repeat 4

capture 03-race-mixed-http-statuses 0 \
	race --insecure --trials 2 \
	--block "$sleep_url?duration=0" --name no-content \
	--block "$base_url/not-found" --name not-found

capture 04-race-body-extraction 0 \
	race --insecure --capture-body 64 \
	--extract-regex '(?P<number>[0-9]+)' \
	--block "$base_url/now" --name clock --repeat 2

capture 05-race-whole-and-named-extraction 0 \
	race --insecure --capture-body 64 \
	--extract-regex '([0-9]{4})[0-9]+' \
	--extract-regex '(?P<prefix>[0-9]{4})(?P<suffix>[0-9]+)' \
	--block "$base_url/now" --name clock

capture 06-race-prefix-only-extraction 0 \
	race --insecure --capture-body 32 \
	--extract-regex '(?P<route>GET /now)' \
	--block "$base_url/" --name routes

# measure: the complete descriptive report at small, wide, and awkward shapes.
capture 07-measure-two-request-baseline 0 \
	measure --insecure --cycles 6 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture 08-measure-random-odd-budget 0 \
	measure --insecure --arrange random --trials 11 \
	--block "$sleep_url?duration=Dus" --vary D=0-300:100

capture 09-measure-fixed-confounding 0 \
	measure --insecure --arrange none --trials 8 \
	--block "$sleep_url?duration=Dus" --vary D=0-300:100

capture 10-measure-width-eight 0 \
	measure --insecure --cycles 2 \
	--block "$sleep_url?duration=Dus" --vary D=0-700:100

capture 11-measure-width-seventeen 0 \
	measure --insecure --cycles 1 \
	--block "$sleep_url?duration=Dus" --vary D=0-1600:100

capture 12-measure-explicit-elision-and-pin 0 \
	measure --insecure --cycles 1 --rank-rows 2 --pin D=800 \
	--block "$sleep_url?duration=Dus" --vary D=0-1600:100

capture 13-measure-status-mismatch 0 \
	measure --insecure --cycles 6 \
	--block "$sleep_url?duration=0" --name no-content \
	--block "$base_url/not-found" --name not-found

capture 14-measure-verbose-form-bodies 0 \
	measure --insecure --cycles 2 --verbose --data duration=0 \
	--block "$sleep_url" --name fast \
	--block "$sleep_url" --name slow --reset-body --data duration=5ms

# analyse: bounded inferential reports, including invalid and unavailable data.
capture 15-analyse-clear-pair 0 \
	analyse --insecure --cycles 6 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture 16-analyse-null-pair 0 \
	analyse --insecure --cycles 6 \
	--block "$sleep_url?duration=0" --name A \
	--block "$sleep_url?duration=0" --name B

capture 17-analyse-width-four 0 \
	analyse --insecure --cycles 8 \
	--block "$sleep_url?duration=Dus" --vary D=0-3000:1000

capture 18-analyse-width-eight 0 \
	analyse --insecure --cycles 11 \
	--block "$sleep_url?duration=Dus" --vary D=0-7000:1000

capture 19-analyse-status-mismatch 0 \
	analyse --insecure --cycles 6 \
	--block "$sleep_url?duration=0" --name no-content \
	--block "$base_url/not-found" --name not-found

capture 20-analyse-unavailable-acquisition 1 \
	analyse --cycles 6 \
	--block https://127.0.0.1:1/a --name A \
	--block https://127.0.0.1:1/b --name B

# detect: positive, bounded-negative, fanned, and verbose request presentations.
capture 21-detect-clear-late-outlier 0 \
	detect --insecure --direction late --false-positive-risk 0.01 \
		--false-negative-risk 0.05 --comparisons-max 60 \
	--block "$sleep_url?duration=0" --name fast --repeat 3 \
	--block "$sleep_url?duration=5ms" --name slow

capture 22-detect-capped-flat-batch 0 \
	detect --insecure --direction late --comparisons-max 30 \
	--block "$sleep_url?duration=0" --name equal --repeat 4

capture 23-detect-varied-width 0 \
	detect --insecure --direction late --width 3 --comparisons-max 60 \
	--block "$sleep_url?duration=Dus" --vary D=0-4000:1000

capture 24-detect-verbose-form-bodies 0 \
	detect --insecure --direction late --comparisons-max 60 --verbose \
	--data duration=0 \
	--block "$sleep_url" --name fast --repeat 3 \
	--block "$sleep_url" --name slow --reset-body --data duration=5ms

# A 256-member field is ordinary for a byte-classification search and is the
# main stress case for detect's adaptively allocated report.
capture 25-detect-256-candidate-field 0 \
	detect --insecure --direction late --false-positive-risk 0.01 \
		--false-negative-risk 0.05 --comparisons-max 200 \
	--block "$sleep_url?duration=100us" --name fast --repeat 255 \
	--block "$sleep_url?duration=5ms" --name slow

# Output routing: compact JSON examples, visible progress, and selected
# destination/consumer behaviour. Ordinary captures already give tturl an open
# stdout pipe through the stream merger; the explicit cat case makes that fact
# visible in the rendered command.
capture 26-race-json-boundary 0 \
	race --report json --insecure --trials 1 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture 27-measure-json-boundary 0 \
	measure --report json --insecure --cycles 1 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture 28-analyse-json-boundary 0 \
	analyse --report json --insecure --cycles 6 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture 29-detect-json-boundary 0 \
	detect --report json --insecure --direction late --comparisons-max 2 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture 30-measure-rate-limited-progress 0 \
	measure --insecure --arrange none --trials 3 --batch-rate-max 1/s \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture_pipe 31-measure-open-pipe 0 cat \
	measure --insecure --cycles 2 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture_pipe 32-race-closed-pipe 0 head \
	race --insecure --trials 100 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

# /dev/stdout exercises an owned -o destination while keeping its report in
# the capture. The tour is a Bash/Python developer aid, not a portable tturl
# runtime surface.
capture 33-measure-output-dev-stdout 0 \
	measure --output /dev/stdout --insecure --cycles 2 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

# Writable reports close with an explicit unavailable or failed terminal state
# after acquisition or comparison failure, in both presentation formats.
capture 34-race-unavailable-acquisition 1 \
	race --trials 1 \
	--block https://127.0.0.1:1/a --name A \
	--block https://127.0.0.1:1/b --name B

capture 35-measure-unavailable-acquisition 1 \
	measure --trials 2 \
	--block https://127.0.0.1:1/a --name A \
	--block https://127.0.0.1:1/b --name B

capture 36-detect-unavailable-comparison 1 \
	detect --direction late --comparisons-max 2 \
	--block https://127.0.0.1:1/a --name A \
	--block https://127.0.0.1:1/b --name B

capture 37-race-json-unavailable-acquisition 1 \
	race --report json --trials 1 \
	--block https://127.0.0.1:1/a --name A \
	--block https://127.0.0.1:1/b --name B

capture 38-measure-json-unavailable-acquisition 1 \
	measure --report json --trials 2 \
	--block https://127.0.0.1:1/a --name A \
	--block https://127.0.0.1:1/b --name B

capture 39-analyse-json-unavailable-acquisition 1 \
	analyse --report json --cycles 6 \
	--block https://127.0.0.1:1/a --name A \
	--block https://127.0.0.1:1/b --name B

capture 40-detect-json-unavailable-comparison 1 \
	detect --report json --direction late --comparisons-max 2 \
	--block https://127.0.0.1:1/a --name A \
	--block https://127.0.0.1:1/b --name B

# time: traditional durations remain descriptive, with one active request per
# connection. These bounded cases add 22 request operations, including priming;
# the preview and refused acquisition do not add target requests.
capture 41-time-two-request-baseline 0 \
	time --insecure --trials 3 \
	--block "$sleep_url?duration=0" --name short \
	--block "$sleep_url?duration=5ms" --name long

capture 42-time-verbose-form-bodies 0 \
	time --insecure --trials 2 --verbose --data duration=0 \
	--block "$sleep_url" --name fast \
	--block "$sleep_url" --name slow --reset-body --data duration=5ms

capture 43-time-mixed-protocol-preview 0 \
	time --insecure --dry-run --verbose --synchronise --last-byte-sync \
	--block "$sleep_url?duration=0" --name first --http1.1 \
	--block "$sleep_url?duration=5ms" --name second

capture 44-time-mixed-protocol-common-gates 0 \
	time --insecure --trials 2 --warmup 1 --synchronise \
	--last-byte-sync --release-delay 100us --request-timeout 2s \
	--block "$sleep_url?duration=0" --name first --http1.1 \
	--block "$sleep_url?duration=5ms" --name second

capture 45-time-rate-limited-starts 0 \
	time --insecure --trials 2 --batch-rate-max 2/s --run-timeout 5s \
	--block "$sleep_url?duration=0" --name bounded

capture 46-time-json-capture-and-extraction 0 \
	time --report json --insecure --trials 2 --capture-headers \
	--capture-body 64 --extract-regex '(?P<number>[0-9]+)' \
	--block "$base_url/now" --name clock

capture 47-time-mixed-http-statuses 0 \
	time --insecure \
	--block "$sleep_url?duration=0" --name no-content \
	--block "$base_url/not-found" --name not-found

capture 48-time-json-unavailable-acquisition 1 \
	time --report json --request-timeout 1s \
	--block https://127.0.0.1:1/a --name unavailable

require_capture_text 41-time-two-request-baseline 'Duration summaries'
require_capture_text 42-time-verbose-form-bodies 'duration=5ms'
require_capture_text 43-time-mixed-protocol-preview 'Preview: network-free dry run'
if [[ $tour_verbosity != normal ]]; then
	require_capture_text 43-time-mixed-protocol-preview 'HTTP/1.1'
	require_capture_text 43-time-mixed-protocol-preview ':method:'
fi
require_capture_text 44-time-mixed-protocol-common-gates 'Completion: complete'
require_capture_text 45-time-rate-limited-starts 'Completion: complete'
require_capture_text 46-time-json-capture-and-extraction '"schema":"tturl.time/v1"'
require_capture_text 46-time-json-capture-and-extraction '"extracts":'
require_capture_text 47-time-mixed-http-statuses 'HTTP 404: 1'
validate_json_capture 48-time-json-unavailable-acquisition failed

for json_capture in \
	26-race-json-boundary \
	27-measure-json-boundary \
	28-analyse-json-boundary \
	29-detect-json-boundary; do
	require_capture_text "$json_capture" 'STDERR: '
	require_capture_text "$json_capture" '"kind":'
done
for json_capture in \
	26-race-json-boundary \
	27-measure-json-boundary \
	28-analyse-json-boundary \
	29-detect-json-boundary \
	37-race-json-unavailable-acquisition \
	38-measure-json-unavailable-acquisition \
	39-analyse-json-unavailable-acquisition \
	40-detect-json-unavailable-comparison \
	46-time-json-capture-and-extraction \
	48-time-json-unavailable-acquisition; do
	validate_json_capture "$json_capture"
done
require_capture_text 30-measure-rate-limited-progress 'STDERR:   ... '
require_capture_text 31-measure-open-pipe '4/4 trials were rank-complete'
require_capture_text 32-race-closed-pipe \
	'================================================================================'
require_capture_text 33-measure-output-dev-stdout '4/4 trials were rank-complete'
for failed_text_capture in \
	34-race-unavailable-acquisition \
	35-measure-unavailable-acquisition \
	36-detect-unavailable-comparison; do
	require_capture_text "$failed_text_capture" 'Completion: failed.'
done
for failed_json_capture in \
	37-race-json-unavailable-acquisition \
	38-measure-json-unavailable-acquisition \
	39-analyse-json-unavailable-acquisition \
	40-detect-json-unavailable-comparison; do
	require_capture_text "$failed_json_capture" '"state":"failed"'
done

if ((${#failures[@]} > 0)); then
	printf 'capture %s completed with %d unexpected result(s):\n' \
		"$capture_id" "${#failures[@]}" >&2
	for failure in "${failures[@]}"; do
		printf '  %s\n' "$failure" >&2
	done
	exit 1
fi

printf 'capture %s completed: %d reports in %s\n' \
	"$capture_id" "$capture_count" "$capture_dir"
