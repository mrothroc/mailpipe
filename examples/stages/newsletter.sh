#!/bin/sh
# Example mailpipe stage: labels a message "newsletter" when its subject says so.
#
# Contract: one JSON message arrives on stdin; print one JSON verdict on
# stdout and exit 0. Exit non-zero (with a reason on stderr) to report a
# failure for this message. See pkg/stage for the message and verdict fields.
#
# Configure it in ~/mail/stages.json:
#   {"name": "newsletter", "type": "exec", "command": ["/path/to/newsletter.sh"]}
set -eu
message=$(cat)
subject=$(printf '%s' "$message" | sed -n 's/.*"subject":"\([^"]*\)".*/\1/p')
case $(printf '%s' "$subject" | tr '[:upper:]' '[:lower:]') in
  *newsletter*) printf '{"labels":["newsletter"],"reason":"subject mentions a newsletter"}\n' ;;
  *) printf '{}\n' ;;
esac
