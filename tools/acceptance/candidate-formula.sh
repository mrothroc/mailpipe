#!/bin/bash
# Candidate installation is CI-only. Lifecycle tests exercise these same traps without Brew.
set -euo pipefail
umask 077
root='' root_identity='' helper='' server_pid='' phase_pid='' tap='' formula='' brew_bin='' tap_owned=0 install_attempted=0 trust_attempted=0 cleaning=0 cleaned=0 observer='' mode='' port=0 candidate='' ci=0 launching=0 pending_signal=0 bootstrap=0 archive_started=-1 archive_done=0 bootstrap_identity='' cleanup_deadline=0
cleanup_step() {
  local timeout=$1 reserve=$2 remaining_ops=$3 available budget result=0; shift 3
  available=$((cleanup_deadline-SECONDS-reserve))
  budget=$((available/remaining_ops))
  if [ "$budget" -le 0 ]; then echo '[candidate-formula] shared cleanup budget exhausted' >&2; return 1; fi
  [ "$budget" -le "$((timeout+5))" ] || budget=$((timeout+5))
  launching=1
  env -i PATH="$PATH" HOME="$root/home" USER="${USER:-}" LANG=C GOTOOLCHAIN=local XDG_CONFIG_HOME="$root/xdg" HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 HOMEBREW_CACHE="$root/cache" HOMEBREW_LOGS="$root/logs" HOMEBREW_TEMP="$root/tmp" GOPATH="$root/gopath" GOCACHE="$root/gocache" GOMODCACHE="$root/gomodcache" "$helper" run --phase cleanup --timeout "${timeout}s" --budget "${budget}s" -- "$@" &
  phase_pid=$!
  registered
  wait "$phase_pid" || result=$?
  phase_pid=''
  return "$result"
}
cleanup() {
  local original=$? failed=0 pid count grace
  if [ "$cleaning" -eq 1 ] || [ "$cleaned" -eq 1 ]; then return "$original"; fi
  cleaning=1
  pending_signal=0 launching=0
  cleanup_started=$SECONDS
  # Reserve a second for integer-clock granularity and orchestration overhead.
  cleanup_deadline=$((SECONDS+44))
  [ "$mode" != cleanup-budget ] || cleanup_deadline=$((SECONDS+8))
  process_reserve=9; [ "$mode" != cleanup-budget ] || process_reserve=1
  trap '' HUP INT TERM
  set +e
  for pid in "$phase_pid" "$server_pid"; do
    [ -n "$pid" ] || continue
    if [ "$bootstrap" -eq 1 ] && [ "$pid" = "$phase_pid" ]; then kill -TERM -- "-$pid" 2>/dev/null; else kill -TERM "$pid" 2>/dev/null; fi
    grace=50; [ "$pid" != "$server_pid" ] || grace=30
    count=0
    while kill -0 "$pid" 2>/dev/null && [ "$count" -lt "$grace" ] && [ "$SECONDS" -lt "$((cleanup_deadline-process_reserve))" ]; do sleep 0.1; count=$((count+1)); done
    if kill -0 "$pid" 2>/dev/null; then if [ "$bootstrap" -eq 1 ] && [ "$pid" = "$phase_pid" ]; then kill -KILL -- "-$pid" 2>/dev/null; else kill -KILL "$pid" 2>/dev/null; fi; fi
    count=0
    while kill -0 "$pid" 2>/dev/null && [ "$count" -lt 20 ] && [ "$SECONDS" -lt "$((cleanup_deadline-process_reserve))" ]; do sleep 0.1; count=$((count+1)); done
    if kill -0 "$pid" 2>/dev/null; then failed=1; else wait "$pid" 2>/dev/null; fi
  done
  phase_pid='' server_pid=''
  if [ "$ci" -eq 1 ] && [ -n "$helper" ] && [ -n "$formula" ]; then
    operations=$((install_attempted+trust_attempted+tap_owned))
    if [ "$install_attempted" -eq 1 ]; then cleanup_step 8 9 "$operations" "$brew_bin" uninstall --formula "$formula" || failed=1; operations=$((operations-1)); fi
    if [ "$trust_attempted" -eq 1 ]; then cleanup_step 8 9 "$operations" "$brew_bin" untrust --formula "$formula" || failed=1; operations=$((operations-1)); fi
    if [ "$tap_owned" -eq 1 ]; then cleanup_step 8 9 "$operations" "$brew_bin" untap "$tap" || failed=1; fi
  fi
  if [ "$mode" = cleanup-budget ]; then
    for operations in 4 3 2 1; do cleanup_step 8 2 "$operations" /bin/bash -c 'trap "" TERM; /bin/sleep 30' || failed=1; done
  fi
  if [ "$ci" -eq 1 ] && [ -n "$root_identity" ] && [ -d "$root/gomodcache" ]; then cleanup_step 2 5 1 go clean -modcache || failed=1; fi
  if [ -n "$root" ] && [ -n "$root_identity" ] && [ -x "$helper" ]; then
    cleanup_step 2 0 1 "$helper" remove-owned --root "$root" --identity "$root_identity" || failed=1
  elif [ -n "$root" ]; then
    # Bootstrap has no helper yet. Delete only the fresh, identity-checked private child.
    if [ -n "$bootstrap_identity" ] && [ -O "$root" ] && [ ! -L "$root" ] && [ "$(stat -f '%d:%i' "$root")" = "$bootstrap_identity" ]; then
      case "$root" in "$parent"/mailpipe-candidate.*) /bin/rm -rf -- "$root" || failed=1;; *) failed=1;; esac
    else failed=1; fi
  fi
  [ "$mode" != cleanup-failure ] || failed=1
  if [ "$ci" -eq 1 ] && [ "$archive_started" -ge 0 ] && [ "$archive_done" -eq 0 ]; then record_phase archive-readiness "$archive_started" fail || failed=1; archive_done=1; fi
  if [ "$ci" -eq 1 ] && [ "$archive_started" -ge 0 ]; then
    if [ "$failed" -eq 0 ]; then record_phase cleanup "$cleanup_started" pass || failed=1; else record_phase cleanup "$cleanup_started" fail || true; fi
  fi
  if [ "$SECONDS" -gt "$cleanup_deadline" ]; then failed=1; fi
  printf '[candidate-formula] cleanup_elapsed_seconds=%s shared_budget_seconds=%s\n' "$((SECONDS-cleanup_started))" "$((cleanup_deadline-cleanup_started))"
  cleaned=1 cleaning=0
  if [ "$failed" -ne 0 ]; then echo '[candidate-formula] cleanup failed; evidence incomplete' >&2; [ "$original" -ne 0 ] || original=1; fi
  return "$original"
}
trap 'status=$?; cleanup_status=0; cleanup || cleanup_status=$?; trap - EXIT; if [ "$status" -ne 0 ]; then exit "$status"; else exit "$cleanup_status"; fi' EXIT
caught() { pending_signal=$1; if [ "$launching" -eq 0 ]; then exit "$pending_signal"; fi; }
registered() { launching=0; if [ "$pending_signal" -ne 0 ]; then exit "$pending_signal"; fi; }
trap 'caught 129' HUP
trap 'caught 130' INT
trap 'caught 143' TERM
refuse() { echo '[candidate-formula] CI-only: real installation requires the disposable candidate_formula macos-15 job' >&2; exit 1; }
observe() {
  [ -n "$observer" ] || return 0
  # Test-owned paths have no escapes/controls; production never writes an observer.
  printf '{"event":"%s","root":"%s","port":%s,"pid":%s,"phase_pid":%s,"candidate":"%s"}\n' "$1" "$root" "$port" "${server_pid:-0}" "${phase_pid:-0}" "$candidate" >> "$observer"
}
parent='' test_helper=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --ci) ci=1; shift;;
    --candidate) candidate=${2:?}; shift 2;;
    --lifecycle-test) mode=${2:?}; shift 2;;
    --helper) test_helper=${2:?}; shift 2;;
    --test-parent) parent=${2:?}; shift 2;;
    --observer) observer=${2:?}; shift 2;;
    *) refuse;;
  esac
done
if [ "$ci" -eq 1 ]; then
  [ -z "$mode$test_helper$parent$observer" ] || refuse
  [ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] && [ "${RUNNER_OS:-}" = macOS ] && [ "${GITHUB_JOB:-}" = candidate_formula ] && [ "${MAILPIPE_CANDIDATE_RUNNER:-}" = macos-15 ] || refuse
  [[ "$candidate" =~ ^[0-9a-f]{40}$ ]] && [ "$candidate" = "${EXPECTED_CANDIDATE:-}" ] || refuse
  parent=${RUNNER_TEMP:?}
  [ "$(git rev-parse HEAD)" = "$candidate" ] && [ -z "$(git status --porcelain --untracked-files=normal)" ] || { echo '[candidate-formula] candidate source mismatch' >&2; exit 1; }
  # No Brew invocation occurs before this CI admission.
  brew_bin=$(command -v brew)
else
  [ -n "$mode" ] && [ -n "$test_helper" ] && [ -n "$parent" ] && [ -n "$observer" ] || refuse
  case "$mode" in success|hold|startup-hold|install-failure|phase-timeout|readiness-timeout|server-failure|cleanup-failure|repeated-cleanup|archive-corruption|cleanup-budget|bootstrap-failure|bootstrap-hold|no-observer-success|no-observer-failure|no-observer-hold) ;; *) refuse;; esac
  candidate=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
fi
[ -d "$parent" ] && [ -O "$parent" ] && [ ! -L "$parent" ] || refuse
root=$(mktemp -d "$parent/mailpipe-candidate.XXXXXXXX")
bootstrap_identity=$(stat -f '%d:%i' "$root")
observe created
bootstrap_helper() {
  # Bootstrap is public source compilation, not a CLI/mail invocation.
  # Job control gives the bootstrap compiler and its children an owned group.
  set -m
  bootstrap=1 launching=1
  TMPDIR="$root" "$@" &
  phase_pid=$!
  registered
  observe bootstrap
  bootstrap_deadline=$((SECONDS+10))
  while kill -0 "$phase_pid" 2>/dev/null && [ "$SECONDS" -lt "$bootstrap_deadline" ]; do sleep 0.1; done
  if kill -0 "$phase_pid" 2>/dev/null; then echo '[candidate-formula] helper bootstrap deadline' >&2; exit 1; fi
  wait "$phase_pid"
  phase_pid='' bootstrap=0
  set +m
}
if [ "$ci" -eq 1 ]; then
  bootstrap_helper go build -o "$root/acceptance" ./tools/acceptance
  helper="$root/acceptance"
elif [ "$mode" = bootstrap-failure ]; then
  bootstrap_helper /bin/bash -c 'printf partial > "$1/partial"; exit 7' fixture "$root"
elif [ "$mode" = bootstrap-hold ]; then
  bootstrap_helper /bin/bash -c 'trap "" TERM; /bin/sleep 30 & printf "%s" "$!" > "$1/bootstrap-child"; wait' fixture "$root"
else
  helper="$test_helper"
fi
root_identity=$("$helper" owned-root --root "$root")
mkdir "$root/home" "$root/mail" "$root/cache" "$root/logs" "$root/tmp" "$root/xdg" "$root/gopath" "$root/gocache" "$root/gomodcache"
export HOME="$root/home" XDG_CONFIG_HOME="$root/xdg" HOMEBREW_CACHE="$root/cache" HOMEBREW_LOGS="$root/logs" HOMEBREW_TEMP="$root/tmp" GOPATH="$root/gopath" GOCACHE="$root/gocache" GOMODCACHE="$root/gomodcache"
export MAILPIPE_CONFIG="$root/mail/config.json" MAILPIPE_DB="$root/mail/mirror.db" MAILPIPE_OUT_DIR="$root/mail/out" MAILPIPE_STAGES="$root/mail/stages.json" MAILPIPE_TEMPLATES="$root/mail/templates.json"
export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 GOTOOLCHAIN=local NO_PROXY=127.0.0.1 no_proxy=127.0.0.1
# Run descendants in a minimal environment. Never inherit credentials, proxy secrets or test hooks.
phase() {
  local name=$1 duration=$2 result summary_target="${GITHUB_STEP_SUMMARY:-}"; shift 2
  case "$name" in archive-readiness|cleanup) summary_target='';; esac
  # Positional parameters remain safe when the optional prefix is empty on
  # system Bash 3.2 with nounset; an empty array does not.
  set -- -- "$@"
  if [ "$ci" -eq 0 ]; then
    case "$mode" in no-observer-*) ;; *) set -- --observer "$root/phase-child" "$@";; esac
  fi
  launching=1
  (
    # Only the parent owns wrapper cleanup, including shell launch failures.
    trap - EXIT HUP INT TERM
    exec env -i TMPDIR="$root/tmp" PATH="$PATH" HOME="$HOME" USER="${USER:-}" LOGNAME="${LOGNAME:-}" LANG=C GOTOOLCHAIN=local XDG_CONFIG_HOME="$XDG_CONFIG_HOME" HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 HOMEBREW_CACHE="$HOMEBREW_CACHE" HOMEBREW_LOGS="$HOMEBREW_LOGS" HOMEBREW_TEMP="$HOMEBREW_TEMP" GOPATH="$GOPATH" GOCACHE="$GOCACHE" GOMODCACHE="$GOMODCACHE" NO_PROXY=127.0.0.1 no_proxy=127.0.0.1 MAILPIPE_CONFIG="$MAILPIPE_CONFIG" MAILPIPE_DB="$MAILPIPE_DB" MAILPIPE_OUT_DIR="$MAILPIPE_OUT_DIR" MAILPIPE_STAGES="$MAILPIPE_STAGES" MAILPIPE_TEMPLATES="$MAILPIPE_TEMPLATES" GITHUB_STEP_SUMMARY="$summary_target" "$helper" run --phase "$name" --timeout "$duration" "$@"
  ) &
  phase_pid=$!
  registered
  observe phase
  result=0; wait "$phase_pid" || result=$?
  phase_pid=''
  return "$result"
}
record_phase() {
  local name=$1 start=$2 result=$3 elapsed
  elapsed=$((SECONDS-start)); [ "$elapsed" -ge 0 ] || elapsed=0
  printf '[candidate-formula] phase=%s elapsed_seconds=%s status=%s end=%s\n' "$name" "$elapsed" "$result" "$(date -u +%FT%TZ)"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then printf '| %s | %s | %s |\n' "$name" "$elapsed" "$result" >> "$GITHUB_STEP_SUMMARY"; fi
}

archive_started=$SECONDS
[ "$archive_started" -ge 0 ] || archive_started=1
printf '[candidate-formula] phase=archive-readiness start=%s\n' "$(date -u +%FT%TZ)"
archive="$root/mailpipe-$candidate.tar.gz"
if [ "$ci" -eq 1 ]; then
  git archive --format=tar --prefix="mailpipe-$candidate/" "$candidate" | gzip -n > "$archive"
else
  mkdir "$root/source"
  git_fixture() { env -i PATH="$PATH" HOME="$HOME" LANG=C GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null git -c core.hooksPath=/dev/null -C "$root/source" "$@"; }
  git_fixture init -q
  printf 'invented archive example.org\n' > "$root/source/example.txt"
  git_fixture add example.txt
  git_fixture -c user.name=Example -c user.email=builder@example.org commit -qm fixture
  candidate=$(git_fixture rev-parse HEAD)
  archive="$root/mailpipe-$candidate.tar.gz"
  git_fixture archive --format=tar --prefix="mailpipe-$candidate/" "$candidate" | gzip -n > "$archive"
  if [ "$mode" = archive-corruption ]; then printf not-gzip >> "$archive"; fi
fi
# Consume the entire stream: a short-reading commit extractor must not mask gzip errors.
gzip -dc "$archive" > "$root/verified-source.tar"
archive_commit=$(git get-tar-commit-id < "$root/verified-source.tar")
[ "$archive_commit" = "$candidate" ] || exit 1
sha=$(shasum -a 256 "$archive"); sha=${sha%% *}
ready="$root/ready.json"
server_ready=$ready
if [ "$mode" = readiness-timeout ]; then server_ready="$root/unobserved-ready.json"; fi
launching=1
if [ "$mode" = server-failure ]; then
  "$helper" serve --archive "$archive" --candidate "$candidate" --sha256 "$(printf '%064d' 0)" --ready "$server_ready" &
else
  "$helper" serve --archive "$archive" --candidate "$candidate" --sha256 "$sha" --ready "$server_ready" &
fi
server_pid=$!
registered
observe started
if [ "$mode" = startup-hold ]; then
  phase install 30s /bin/sleep 25
fi
readiness_timeout=10s
[ "$mode" != readiness-timeout ] || readiness_timeout=200ms
port=$("$helper" wait-ready --ready "$ready" --candidate "$candidate" --sha256 "$sha" --pid "$server_pid" --download "$root/download.tar.gz" --timeout "$readiness_timeout")
observe ready
if [ "$ci" -eq 0 ]; then
  mkdir "$root/tap" "$root/trust" "$root/keg"
  case "$mode" in
    install-failure|no-observer-failure) phase install 2s /usr/bin/false;;
    phase-timeout) phase install 100ms /bin/sleep 25;;
    hold) phase install 30s /bin/sleep 25;;
    no-observer-hold) phase install 30s /bin/bash -c 'printf "%s" "$$" > "$1"; exec /bin/sleep 25' fixture "$root/fixture-child";;
    repeated-cleanup) cleanup; cleanup;;
    *) phase install 2s /usr/bin/true;;
  esac
  exit 0
fi
brew_read() { env -i TMPDIR="$root/tmp" PATH="$PATH" HOME="$HOME" USER="${USER:-}" XDG_CONFIG_HOME="$XDG_CONFIG_HOME" HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 HOMEBREW_CACHE="$HOMEBREW_CACHE" "$brew_bin" "$@"; }
prefix=$(brew_read --prefix);cellar=$(brew_read --cellar);repository=$(brew_read --repository)
"$helper" brew-layout --prefix "$prefix" --cellar "$cellar" --repository "$repository"
os_version=$(sw_vers -productVersion)
[[ "$os_version" =~ ^15\.[0-9]+(\.[0-9]+)?$ ]] || { echo '[candidate-formula] runner version mismatch' >&2; exit 1; }
architecture=$(uname -m)
case "$architecture" in arm64|x86_64) ;; *) exit 1;; esac
printf '[candidate-formula] candidate=%s archive_sha256=%s runner=macos-15 os_version=%s arch=%s\n' "$candidate" "$sha" "$os_version" "$architecture"
phase archive-readiness 10s "$helper" brew-version --brew "$brew_bin"
# Unique tap and no preexisting normal mailpipe keg; default prefix belongs to this disposable VM.
[ ! -e "$cellar/mailpipe" ] && [ ! -e "$prefix/opt/mailpipe" ] || exit 1
tap="candidate/run${root_identity##*:}";formula="$tap/mailpipe"
tap_path="$repository/Library/Taps/candidate/homebrew-${tap#candidate/}"
[ ! -e "$tap_path" ] && [ ! -L "$tap_path" ] || exit 1
tap_owned=1
phase archive-readiness 20s "$brew_bin" tap-new --no-git "$tap"
"$helper" tap-location --path "$tap_path" --repository "$repository"
"$helper" formula --candidate "$candidate" --sha256 "$sha" --port "$port" --out "$tap_path/Formula/mailpipe.rb"
formula_sha=$(shasum -a 256 "$tap_path/Formula/mailpipe.rb");formula_sha=${formula_sha%% *}
trust_attempted=1
phase archive-readiness 20s "$helper" trust-check --brew "$brew_bin" --formula "$formula"
[ "${formula_sha}" = "$(shasum -a 256 "$tap_path/Formula/mailpipe.rb" | cut -d ' ' -f 1)" ] || exit 1
record_phase archive-readiness "$archive_started" pass
archive_done=1
install_attempted=1
phase install 7m "$brew_bin" install --build-from-source "$formula"
phase formula-test 1m "$brew_bin" test "$formula"
installed_prefix=$(brew_read --prefix "$formula")
"$helper" installed-location --prefix "$installed_prefix" --cellar "$cellar"
phase installed-smoke 30s "$helper" installed-smoke --binary "$installed_prefix/bin/mailpipe" --examples "$installed_prefix/share/mailpipe/examples" --root "$root/installed-smoke"
# EXIT is the single cleanup authority; success is printed only after cleanup succeeded.
cleanup
[ ! -e "$cellar/mailpipe" ] && [ ! -e "$prefix/opt/mailpipe" ] && [ ! -e "$tap_path" ] || { echo '[candidate-formula] owned Brew resources remain' >&2; exit 1; }
printf '[candidate-formula] synthetic candidate installation and owned cleanup passed\n'
