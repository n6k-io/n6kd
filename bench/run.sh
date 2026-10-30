#!/usr/bin/env bash
# Usage: ROWS=1000000 N=5 DUCKDB=duckdb N6KD=./n6kd N6K_CLIENT=n6k_client bench/run.sh
set -euo pipefail

cd "$(dirname "$0")"
DUCKDB=${DUCKDB:-duckdb}
N6KD=${N6KD:-../n6kd}
ROWS=${ROWS:-1000000}
N=${N:-5}
N6K_CLIENT=${N6K_CLIENT:-n6k_client}

want=$(awk '$1 == "github.com/duckdb/duckdb-go/v2" { sub(/^v2\./, "", $2); v = $2 + 0
	printf "v%d.%d.%d", v / 10000, v / 100 % 100, v % 100 }' ../go.mod)
have=$("$DUCKDB" --version | awk '{ print $1 }')
if [ "$have" != "$want" ]; then
	echo "$DUCKDB is $have; n6kd embeds $want" >&2
	exit 1
fi

pids=()
trap 'kill "${pids[@]}" 2>/dev/null || true' EXIT
boot=(-c "SET VARIABLE rows = $ROWS" -f schema.sql --threads 4)
# :8081 n6k, :8082 quack, each bridged through perms.yml; :8083, :8084 the same, no perms
"$N6KD" "${boot[@]}" --perms perms.yml --addr localhost:8081 >n6k.log 2>&1 &
pids+=($!)
"$N6KD" quack "${boot[@]}" --perms perms.yml --addr localhost:8082 --token bench >quack.log 2>&1 &
pids+=($!)
"$N6KD" "${boot[@]}" --addr localhost:8083 >n6k-noperms.log 2>&1 &
pids+=($!)
# no-perms quack serves the real main database, which `USE app` does not change
"$N6KD" quack -c "SET VARIABLE rows = $ROWS" -c "$(grep -v -e '^ATTACH' -e '^USE' schema.sql)" --threads 4 --addr localhost:8084 --token bench >quack-noperms.log 2>&1 &
pids+=($!)
echo "$(date +%T) starting servers with $ROWS rows" >&2
for i in 0 1 2 3; do
	port=$((8081 + i))
	until nc -z localhost "$port" 2>/dev/null; do
		if ! kill -0 "${pids[$i]}" 2>/dev/null; then
			echo "server on :$port exited; see bench/*.log" >&2
			exit 1
		fi
		sleep 0.2
	done
done

# Each quack scan thread opens its own connection, and quack's server queues only
# 5 (cpp-httplib's CPPHTTPLIB_LISTEN_BACKLOG); past that, connects fail on macOS 27.
# Every client and server gets the same 4 threads.
threads="SET threads = 4;"
attach_n6k="$threads LOAD '$N6K_CLIENT'; ATTACH 'n6k://localhost:8081' AS r (TYPE n6k, CATALOG 'app');"
attach_quack="$threads LOAD quack; CREATE SECRET (TYPE quack, TOKEN 'bench'); ATTACH 'quack:localhost:8082' AS r;"
attach_n6k_noperms="$threads LOAD '$N6K_CLIENT'; ATTACH 'n6k://localhost:8083' AS r (TYPE n6k, CATALOG 'app');"
attach_quack_noperms="$threads LOAD quack; CREATE SECRET (TYPE quack, TOKEN 'bench'); ATTACH 'quack:localhost:8084' AS r;"
# the same data in-process, as the reference each target's results must match
attach_local="$threads SET VARIABLE rows = $ROWS; $(sed 's/AS app;/AS r;/; s/USE app;/USE r;/' schema.sql)"

# A sanity check, not a full comparison: a result's row count and an
# order-independent hash. A LIMIT without ORDER BY may return any rows, so only
# its count is compared; a write is compared by its affected-row count.
fingerprint() {
	local attach=$1 query=${2%;} check
	if [[ $query == UPDATE* || $query == DELETE* ]]; then
		check="$query;"
	elif [[ $query == *LIMIT* && $query != *"ORDER BY"* ]]; then
		check="SELECT count(*) FROM ($query);"
	else
		check="SELECT count(*), bit_xor(hash(t)) FROM ($query) t;"
	fi
	printf '%s\n.mode csv\n.headers off\n%s\n' "$attach" "$check" | "$DUCKDB" -unsigned 2>&1 | tail -1
}

# ✗ when the target's fingerprint differs from the reference; nothing when the
# query failed, which the timing column already reports.
verdict() {
	local got
	got=$(fingerprint "$1" "$2")
	if [[ $got != *Error* && $got != "$3" ]]; then
		echo " ✗ result differs"
	fi
}

# median ± sample standard deviation of the profiled latency, or the error when every run failed the same way (an
# unsupported query). An IO error or a run that fails where others succeeded exits
# the bench. A failed statement prints no profile.
measure() {
	local attach=$1 query=$2 out times ok
	out=$({
		echo "$attach"
		echo ".mode trash"
		echo "PRAGMA enable_profiling = 'json';"
		for _ in $(seq "$N"); do echo "$query"; done
	} | "$DUCKDB" -unsigned 2>&1)
	times=$(awk '/^    "latency":/ { sub(/,$/, "", $2); print $2 }' <<<"$out" | sort -g)
	ok=$(grep -c . <<<"$times" || true)
	if grep -q "IO Error" <<<"$out" || { [ "$ok" -gt 0 ] && [ "$ok" -lt "$N" ]; }; then
		echo "$ok/$N runs ok: $(grep -m1 Error <<<"$out")" >&2
		exit 1
	fi
	if [ "$ok" -eq 0 ]; then
		echo "error: $(grep -m1 Error <<<"$out")"
		return
	fi
	# The std is rounded to 1 significant figure, 2 when it starts with a 1, and
	# the median to the same decimal place.
	awk '{ t[NR] = $1 * 1000; s += t[NR] } END {
		m = s / NR
		for (i = 1; i <= NR; i++) d += (t[i] - m) ^ 2
		sd = NR > 1 ? sqrt(d / (NR - 1)) : 0
		med = t[int((NR + 1) / 2)]
		if (sd == 0) { printf "%.2fms", med; exit }
		e = int(log(sd) / log(10)); if (10 ^ e > sd) e--
		place = e - (int(sd / 10 ^ e) == 1 ? 1 : 0)
		unit = 10 ^ place
		dp = place < 0 ? -place : 0
		printf "%." dp "f ±%." dp "fms", int(med / unit + 0.5) * unit, int(sd / unit + 0.5) * unit
	}' <<<"$times"
}

# Bytes sent / received by the client for one run of the query, attach excluded,
# from the client's own log: WS for n6k (n6k frames, without WebSocket framing),
# HTTP for quack (request and response bodies). Its own untimed run, since logging
# may cost time. Logging must be on before the attach: a connection the attach
# opens logs only to the log storage current then, and enable_logging and
# truncate_duckdb_logs replace it. So the attach's traffic is subtracted instead.
transfer() {
	local target=$1 attach=$2 query=$3 log sent recv out
	case $target in
	n6k)
		log=WS
		sent="sum(bytes) FILTER (direction = 'send')"
		recv="sum(bytes) FILTER (direction = 'recv')"
		;;
	quack)
		log=HTTP
		sent="sum(request.request_body_length)"
		recv="sum(response.headers['Content-Length']::BIGINT)"
		;;
	esac
	local totals="SELECT coalesce($sent, 0)::BIGINT AS sent, coalesce($recv, 0)::BIGINT AS recv FROM duckdb_logs_parsed('$log')"
	out=$({
		echo ".mode trash"
		echo "CALL enable_logging('$log', storage = 'memory');"
		echo "$attach"
		echo "CREATE TEMP TABLE before_query AS $totals;"
		echo "$query"
		echo ".mode list"
		echo ".headers off"
		echo "CREATE MACRO kib(n) AS CASE WHEN n < 1024 THEN '<1' ELSE format('{:,}', round(n / 1024)::BIGINT) END;"
		echo "SELECT kib(a.sent - b.sent) || ' / ' || kib(a.recv - b.recv) FROM ($totals) a, before_query b;"
	} | "$DUCKDB" -unsigned 2>&1)
	if grep -q Error <<<"$out"; then
		echo "-"
	else
		tail -1 <<<"$out" | sed "s/$(printf '\033')\[[0-9;]*m//g"
	fi
}

mkdir -p plans
targets=(local n6k quack n6k_noperms quack_noperms)
header="| query | native | n6k perms | quack perms | n6k no perms | quack no perms |
| --- | --- | --- | --- | --- | --- |"
bytes_rows=()
echo "$header"
for file in queries/*.sql; do
	name=$(basename "$file" .sql)
	query=$(cat "$file")
	want=$(fingerprint "$attach_local" "$query")
	row="| $name |"
	bytes_row="| $name |"
	for target in "${targets[@]}"; do
		attach_var=attach_$target
		echo "$(date +%T) $name $target" >&2
		echo "${!attach_var} EXPLAIN $query" | "$DUCKDB" -unsigned >"plans/$target-$name.txt" 2>&1 || true
		if [[ $target == n6k || $target == quack ]]; then
			bytes_row+=" $(transfer "$target" "${!attach_var}" "$query") |"
		fi
		time=$(measure "${!attach_var}" "$query")
		row+=" $time$(verdict "${!attach_var}" "$query" "$want") |"
	done
	echo "$row"
	bytes_rows+=("$bytes_row")
done

echo
echo "KiB up / down from the client, one run"
echo
echo "| query | n6k | quack |"
echo "| --- | --- | --- |"
printf '%s\n' "${bytes_rows[@]}"
